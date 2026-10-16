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
	eol := lineEnding(data)
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	begin, end := mdBegin(o.id), mdEnd(o.id)
	if strings.Count(text, begin) > 1 || strings.Count(text, end) > 1 {
		return []Item{o.item(o.id, real, StatusSkipped, "duplicate zuano block markers; fix the file by hand")}, nil
	}
	bi := strings.Index(text, begin)
	ei := strings.Index(text, end)
	if (bi >= 0) != (ei >= 0) || (bi >= 0 && ei < bi) {
		return []Item{o.item(o.id, real, StatusSkipped, "unbalanced zuano block markers; fix the file by hand")}, nil
	}

	ledgerID := "block:" + o.id
	led := c.Ledger.entries(real)
	current := ""
	if bi >= 0 {
		current = text[bi : ei+len(end)]
	}
	// A block whose text no longer matches what zuano recorded was edited by hand.
	edited := bi >= 0 && led[ledgerID] != "" && contentHash([]byte(current)) != led[ledgerID]

	if o.body == "" {
		if bi < 0 {
			c.Ledger.drop(real, ledgerID)
			return nil, nil
		}
		if edited && !c.Force {
			return []Item{o.item(o.id, real, StatusConflict, "block edited since zuano wrote it; --force removes it")}, nil
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
		c.Ledger.drop(real, ledgerID)
		if strings.TrimSpace(out) == "" {
			err = c.remove(real)
		} else {
			err = c.write(real, 0o644, []byte(withEOL(strings.TrimRight(out, "\n")+"\n", eol)))
		}
		return []Item{o.item(o.id, real, StatusRemoved, "")}, err
	}

	block := begin + "\n" + strings.TrimRight(o.body, "\n") + "\n" + end
	var out, status string
	switch {
	case bi >= 0:
		if current == block {
			c.Ledger.set(real, ledgerID, contentHash([]byte(block)))
			return []Item{o.item(o.id, real, StatusUnchanged, "")}, nil
		}
		if edited && !c.Force {
			return []Item{o.item(o.id, real, StatusConflict, "block edited since zuano wrote it; --force overwrites")}, nil
		}
		out, status = text[:bi]+block+text[ei+len(end):], StatusUpdated
	case !exists || strings.TrimSpace(text) == "":
		out, status = block+"\n", StatusAdded
	default:
		out, status = strings.TrimRight(text, "\n")+"\n\n"+block+"\n", StatusAdded
	}
	c.Ledger.set(real, ledgerID, contentHash([]byte(block)))
	return []Item{o.item(o.id, real, status, "")}, c.write(real, 0o644, []byte(withEOL(out, eol)))
}

// lineEnding reports the line ending a file uses, so a rewrite keeps it.
func lineEnding(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func withEOL(s, eol string) string {
	if eol == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\n", eol)
}

// tomlTable is one table zuano manages inside a TOML file. path is the key
// path used to detect a user definition outside the managed block.
type tomlTable struct {
	header string
	path   []string
	body   string
	// server is the MCP server name the table configures, empty otherwise.
	server string
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
	eol := lineEnding(data)
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	begin, end := tomlBegin(o.id), tomlEnd(o.id)
	if strings.Count(text, begin) > 1 || strings.Count(text, end) > 1 {
		return []Item{o.item(o.id, real, StatusSkipped, "duplicate zuano block markers; fix the file by hand")}, nil
	}
	bi, ei := strings.Index(text, begin), strings.Index(text, end)
	if (bi >= 0) != (ei >= 0) || (bi >= 0 && ei < bi) {
		return []Item{o.item(o.id, real, StatusSkipped, "unbalanced zuano block markers; fix the file by hand")}, nil
	}
	ledgerID := "block:" + o.id
	if len(o.tables) == 0 && bi < 0 {
		c.Ledger.drop(real, ledgerID)
		return nil, nil
	}

	outside, oldBlock := text, ""
	if bi >= 0 {
		oldBlock = text[bi : ei+len(end)]
		outside = text[:bi] + text[ei+len(end):]
		// A block whose text no longer matches what zuano recorded was edited by hand.
		if want := c.Ledger.entries(real)[ledgerID]; want != "" && contentHash([]byte(oldBlock)) != want && !c.Force {
			return []Item{o.item(o.id, real, StatusConflict, "block edited since zuano wrote it; --force overwrites")}, nil
		}
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
	tableItem := func(t tomlTable, status, detail string) Item {
		it := o.item(t.header, real, status, detail)
		it.Server = t.server
		return it
	}
	for _, t := range o.tables {
		// Even under --force: a second definition of the same table makes the file invalid TOML.
		if definedAt(parsed, t.path) {
			items = append(items, tableItem(t, StatusConflict, "already defined and not managed by zuano; --force is not supported for TOML tables, remove the existing table first"))
			continue
		}
		bodies = append(bodies, strings.TrimRight(t.body, "\n"))
		newHeaders[t.header] = true
		switch {
		case strings.Contains(oldBlock, strings.TrimRight(t.body, "\n")):
			items = append(items, tableItem(t, StatusUnchanged, ""))
		case oldHeaders[t.header]:
			items = append(items, tableItem(t, StatusUpdated, ""))
		default:
			items = append(items, tableItem(t, StatusAdded, ""))
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
		c.Ledger.drop(real, ledgerID)
		if strings.TrimSpace(out) == "" {
			return items, c.remove(real)
		}
		return items, c.write(real, 0o644, []byte(withEOL(strings.TrimRight(out, "\n")+"\n", eol)))
	}
	block := begin + "\n" + strings.Join(bodies, "\n\n") + "\n" + end
	switch {
	case bi >= 0:
		if oldBlock == block {
			c.Ledger.set(real, ledgerID, contentHash([]byte(block)))
			return items, nil
		}
		out = text[:bi] + block + text[ei+len(end):]
	case !exists || strings.TrimSpace(text) == "":
		out = block + "\n"
	default:
		out = strings.TrimRight(text, "\n") + "\n\n" + block + "\n"
	}
	c.Ledger.set(real, ledgerID, contentHash([]byte(block)))
	return items, c.write(real, 0o600, []byte(withEOL(out, eol)))
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

// ownedFilesOp manages whole files in a directory that zuano owns by name.
// A file is zuano's only while the ledger says zuano wrote it and it still
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
// the bytes zuano wrote.
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
			items = append(items, o.item(f.name, real, StatusConflict, "file exists and is not managed by zuano; --force overwrites"))
			continue
		case contentHash(data) != prev:
			items = append(items, o.item(f.name, real, StatusConflict, "edited since zuano wrote it; --force overwrites"))
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
			items = append(items, o.item(name, real, StatusConflict, "edited since zuano wrote it; left in place"))
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
