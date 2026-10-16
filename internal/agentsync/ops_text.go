package agentsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// mdBlockOp keeps one marked block inside a shared markdown file
// (CLAUDE.md, AGENTS.md, GEMINI.md). Everything outside the block is the
// user's. An empty body removes the block.
type mdBlockOp struct {
	meta
	path string
	id   string
	body string
}

func mdBegin(id string) string { return "<!-- " + Marker + ":begin " + id + " -->" }
func mdEnd(id string) string   { return "<!-- " + Marker + ":end " + id + " -->" }

func (o *mdBlockOp) Run(c *Ctx) ([]Item, error) {
	data, exists, real, err := c.read(o.scope, o.path)
	if items, ok := o.refused(err); ok {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	text := string(data)
	begin, end := mdBegin(o.id), mdEnd(o.id)
	bi := strings.Index(text, begin)
	ei := strings.Index(text, end)
	if (bi >= 0) != (ei >= 0) || (bi >= 0 && ei < bi) {
		return []Item{o.item(o.id, real, StatusSkipped, "unbalanced relay block markers; fix the file by hand")}, nil
	}

	if o.body == "" {
		if bi < 0 {
			return nil, nil
		}
		rest := strings.TrimRight(text[:bi], "\n")
		tail := strings.TrimLeft(text[ei+len(end):], "\n")
		out := rest
		if tail != "" {
			if out != "" {
				out += "\n\n"
			}
			out += tail
		}
		if strings.TrimSpace(out) == "" {
			err = c.remove(real)
		} else {
			err = c.write(real, 0o644, []byte(strings.TrimRight(out, "\n")+"\n"))
		}
		return []Item{o.item(o.id, real, StatusRemoved, "")}, err
	}

	block := begin + "\n" + strings.TrimRight(o.body, "\n") + "\n" + end
	var out, status string
	switch {
	case bi >= 0:
		if text[bi:ei+len(end)] == block {
			return []Item{o.item(o.id, real, StatusUnchanged, "")}, nil
		}
		out, status = text[:bi]+block+text[ei+len(end):], StatusUpdated
	case !exists || strings.TrimSpace(text) == "":
		out, status = block+"\n", StatusAdded
	default:
		out, status = strings.TrimRight(text, "\n")+"\n\n"+block+"\n", StatusAdded
	}
	return []Item{o.item(o.id, real, status, "")}, c.write(real, 0o644, []byte(out))
}

// tomlTable is one table relay manages inside a TOML file. path is the key
// path used to detect a user definition outside the managed block.
type tomlTable struct {
	header string
	path   []string
	body   string
}

// tomlBlockOp keeps a marked block of tables inside a TOML file. TOML has no
// order-preserving round trip, so the block is plain text between comment
// markers and tables the user defined elsewhere are left alone.
type tomlBlockOp struct {
	meta
	path   string
	id     string
	tables []tomlTable
}

func tomlBegin(id string) string { return "# >>> " + Marker + ":begin " + id + " >>>" }
func tomlEnd(id string) string   { return "# <<< " + Marker + ":end " + id + " <<<" }

var tomlHeader = regexp.MustCompile(`(?m)^\[+([^\]\n]+)\]+\s*$`)

func (o *tomlBlockOp) Run(c *Ctx) ([]Item, error) {
	data, exists, real, err := c.read(o.scope, o.path)
	if items, ok := o.refused(err); ok {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	text := string(data)
	begin, end := tomlBegin(o.id), tomlEnd(o.id)
	bi, ei := strings.Index(text, begin), strings.Index(text, end)
	if (bi >= 0) != (ei >= 0) || (bi >= 0 && ei < bi) {
		return []Item{o.item(o.id, real, StatusSkipped, "unbalanced relay block markers; fix the file by hand")}, nil
	}
	if len(o.tables) == 0 && bi < 0 {
		return nil, nil
	}

	outside, oldBlock := text, ""
	if bi >= 0 {
		oldBlock = text[bi : ei+len(end)]
		outside = text[:bi] + text[ei+len(end):]
	}
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(outside), &parsed); err != nil {
		return []Item{o.item(o.id, real, StatusSkipped, "not valid TOML ("+firstLine(err.Error())+"); edit it by hand")}, nil
	}

	oldHeaders := map[string]bool{}
	for _, m := range tomlHeader.FindAllStringSubmatch(oldBlock, -1) {
		oldHeaders[strings.TrimSpace(m[1])] = true
	}
	var items []Item
	var bodies []string
	newHeaders := map[string]bool{}
	for _, t := range o.tables {
		// Even under --force: a second definition of the same table makes the file invalid TOML.
		if definedAt(parsed, t.path) {
			items = append(items, o.item(t.header, real, StatusConflict, "already defined and not managed by relay; --force is not supported for TOML tables, remove the existing table first"))
			continue
		}
		bodies = append(bodies, strings.TrimRight(t.body, "\n"))
		newHeaders[t.header] = true
		switch {
		case strings.Contains(oldBlock, strings.TrimRight(t.body, "\n")):
			items = append(items, o.item(t.header, real, StatusUnchanged, ""))
		case oldHeaders[t.header]:
			items = append(items, o.item(t.header, real, StatusUpdated, ""))
		default:
			items = append(items, o.item(t.header, real, StatusAdded, ""))
		}
	}
	var gone []string
	for h := range oldHeaders {
		if !newHeaders[h] && !isSubTable(h, newHeaders) {
			gone = append(gone, h)
		}
	}
	sort.Strings(gone)
	for _, h := range gone {
		items = append(items, o.item(h, real, StatusRemoved, ""))
	}

	var out string
	if len(bodies) == 0 {
		if bi < 0 {
			return items, nil
		}
		rest := strings.TrimRight(text[:bi], "\n")
		tail := strings.TrimLeft(text[ei+len(end):], "\n")
		out = rest
		if tail != "" {
			if out != "" {
				out += "\n\n"
			}
			out += tail
		}
		if strings.TrimSpace(out) == "" {
			return items, c.remove(real)
		}
		return items, c.write(real, 0o644, []byte(strings.TrimRight(out, "\n")+"\n"))
	}
	block := begin + "\n" + strings.Join(bodies, "\n\n") + "\n" + end
	switch {
	case bi >= 0:
		if oldBlock == block {
			return items, nil
		}
		out = text[:bi] + block + text[ei+len(end):]
	case !exists || strings.TrimSpace(text) == "":
		out = block + "\n"
	default:
		out = strings.TrimRight(text, "\n") + "\n\n" + block + "\n"
	}
	return items, c.write(real, 0o600, []byte(out))
}

func isSubTable(header string, present map[string]bool) bool {
	for p := range present {
		if strings.HasPrefix(header, p+".") {
			return true
		}
	}
	return false
}

func definedAt(m map[string]any, path []string) bool {
	cur := m
	for i, k := range path {
		v, ok := cur[k]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

type ownedFile struct {
	name string
	data []byte
}

// ownedFilesOp manages whole files in a directory that relay owns by name.
// A file is relay's only while the ledger says relay wrote it and it still
// holds those bytes: a same-named file the ledger does not know, or one the
// user edited since, is never overwritten or removed, whatever text it
// contains. Ledgered files that are no longer desired are removed.
type ownedFilesOp struct {
	meta
	dir   string
	ext   string
	files []ownedFile
	mode  fs.FileMode
}

// ledgerFileID keys a whole file in the ledger; the value is the sha256 of
// the bytes relay wrote.
const ledgerFileID = "file"

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (o *ownedFilesOp) Run(c *Ctx) ([]Item, error) {
	var items []Item
	keep := map[string]bool{}
	for _, f := range o.files {
		path := filepath.Join(o.dir, f.name+o.ext)
		data, exists, real, err := c.read(o.scope, path)
		if refused, ok := o.refused(err); ok {
			items = append(items, refused...)
			keep[filepath.Base(path)] = true
			continue
		}
		if err != nil {
			return nil, err
		}
		keep[filepath.Base(path)] = true
		prev, ledgered := c.Ledger.entries(real)[ledgerFileID]
		want := contentHash(f.data)
		switch {
		case !exists:
			items = append(items, o.item(f.name, real, StatusAdded, ""))
		case bytes.Equal(data, f.data):
			c.Ledger.set(real, ledgerFileID, want)
			items = append(items, o.item(f.name, real, StatusUnchanged, ""))
			continue
		case c.Force:
			items = append(items, o.item(f.name, real, StatusUpdated, ""))
		case !ledgered:
			items = append(items, o.item(f.name, real, StatusConflict, "file exists and is not managed by relay; --force overwrites"))
			continue
		case contentHash(data) != prev:
			items = append(items, o.item(f.name, real, StatusConflict, "edited since relay wrote it; --force overwrites"))
			continue
		default:
			items = append(items, o.item(f.name, real, StatusUpdated, ""))
		}
		if err := c.write(real, o.mode, f.data); err != nil {
			return nil, err
		}
		c.Ledger.set(real, ledgerFileID, want)
	}

	dir, err := c.guard(o.scope, o.dir)
	if err != nil {
		return items, nil
	}
	var stale []string
	for file, entries := range c.Ledger.Files {
		_, ledgered := entries[ledgerFileID]
		if ledgered && filepath.Dir(file) == dir && strings.HasSuffix(file, o.ext) && !keep[filepath.Base(file)] {
			stale = append(stale, file)
		}
	}
	sort.Strings(stale)
	for _, file := range stale {
		name := strings.TrimSuffix(filepath.Base(file), o.ext)
		data, exists, real, err := c.read(o.scope, file)
		if err != nil {
			continue
		}
		if !exists {
			c.Ledger.drop(real, ledgerFileID)
			continue
		}
		if contentHash(data) != c.Ledger.entries(real)[ledgerFileID] {
			items = append(items, o.item(name, real, StatusConflict, "edited since relay wrote it; left in place"))
			continue
		}
		if err := c.remove(real); err != nil {
			return nil, err
		}
		c.Ledger.drop(real, ledgerFileID)
		items = append(items, o.item(name, real, StatusRemoved, ""))
	}
	return items, nil
}
