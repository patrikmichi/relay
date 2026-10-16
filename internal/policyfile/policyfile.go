// Package policyfile loads an organisation's policy directory, checks it
// against the published JSON Schema, formats it and renders plans, all on this
// machine. The gateway compiles and applies it; this package finds the
// mistakes that need no gateway, with file and line.
package policyfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/signing"
)

// Limits the gateway enforces too.
const (
	MaxDocumentBytes = 1 << 20
	maxFiles         = 200
)

// File is one policy file as sent to the gateway.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Diag is one problem, with where it is.
type Diag struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

func (d Diag) String() string {
	loc := d.File
	if loc != "" && d.Line > 0 {
		loc = fmt.Sprintf("%s:%d", d.File, d.Line)
	}
	if loc == "" {
		return d.Message
	}
	return loc + ": " + d.Message
}

// Doc is a policy directory merged into one document.
type Doc struct {
	Files []File
	Value map[string]any
	pos   map[string]position
}

type position struct {
	file string
	line int
}

// Load merges every *.yaml and *.yml file under dir by top-level section. A
// section defined in two files is an error. Links are not followed.
func Load(dir string) (*Doc, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if e.IsDir() {
			if p != dir && strings.HasPrefix(e.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(p)); ext == ".yaml" || ext == ".yml" {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .yaml files under %s", dir)
	}
	if len(paths) > maxFiles {
		return nil, fmt.Errorf("%d policy files; the limit is %d", len(paths), maxFiles)
	}
	sort.Strings(paths)

	d := &Doc{Value: map[string]any{}, pos: map[string]position{}}
	total := 0
	owner := map[string]string{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if total += len(raw); total > MaxDocumentBytes {
			return nil, fmt.Errorf("the policy is larger than %d bytes", MaxDocumentBytes)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		d.Files = append(d.Files, File{Path: rel, Content: string(raw)})
		var root yaml.Node
		if err := yaml.Unmarshal(raw, &root); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if len(root.Content) == 0 {
			continue
		}
		top := root.Content[0]
		if top.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s:%d: a policy file is a mapping of sections", rel, top.Line)
		}
		for i := 0; i+1 < len(top.Content); i += 2 {
			key, val := top.Content[i], top.Content[i+1]
			if prev, dup := owner[key.Value]; dup {
				return nil, fmt.Errorf("%s:%d: section %q is already defined in %s", rel, key.Line, key.Value, prev)
			}
			owner[key.Value] = fmt.Sprintf("%s:%d", rel, key.Line)
			var v any
			if err := val.Decode(&v); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", rel, key.Line, err)
			}
			d.Value[key.Value] = normalize(v)
			d.index("/"+escapePointer(key.Value), rel, key, val)
		}
	}
	return d, nil
}

// normalize makes YAML-decoded values JSON-compatible.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
		return t
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[fmt.Sprint(k)] = normalize(x)
		}
		return out
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
	}
	return v
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// index records the file and line of every key and sequence item, by JSON
// pointer.
func (d *Doc) index(ptr, file string, key, val *yaml.Node) {
	line := val.Line
	if key != nil {
		line = key.Line
	}
	d.pos[ptr] = position{file, line}
	switch val.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(val.Content); i += 2 {
			d.index(ptr+"/"+escapePointer(val.Content[i].Value), file, val.Content[i], val.Content[i+1])
		}
	case yaml.SequenceNode:
		for i, item := range val.Content {
			d.index(fmt.Sprintf("%s/%d", ptr, i), file, nil, item)
		}
	}
}

// Where finds the file and line of a JSON pointer, or of its nearest parent.
func (d *Doc) Where(ptr string) (string, int) {
	for ptr != "" {
		if p, ok := d.pos[ptr]; ok {
			return p.file, p.line
		}
		ptr = ptr[:strings.LastIndexByte(ptr, '/')]
	}
	return "", 0
}

// CanonicalJSON is the merged document as canonical JSON.
func (d *Doc) CanonicalJSON() ([]byte, error) { return signing.CanonicalJSON(d.Value) }

var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(SchemaJSON()))
		if err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if schemaErr = c.AddResource("policy-v1.json", doc); schemaErr != nil {
			return
		}
		schema, schemaErr = c.Compile("policy-v1.json")
	})
	return schema, schemaErr
}

// Validate checks the merged document against the embedded schema.
func (d *Doc) Validate() ([]Diag, error) {
	s, err := compiledSchema()
	if err != nil {
		return nil, fmt.Errorf("load the policy schema: %w", err)
	}
	raw, err := json.Marshal(d.Value)
	if err != nil {
		return nil, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	verr := s.Validate(inst)
	if verr == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return nil, verr
	}
	var diags []Diag
	seen := map[string]bool{}
	add := func(diag Diag) {
		if key := diag.String(); !seen[key] {
			seen[key] = true
			diags = append(diags, diag)
		}
	}
	for _, u := range leafErrors(ve.DetailedOutput()) {
		msg := u.Error.String()
		if m := unknownProps.FindStringSubmatch(msg); m != nil {
			for _, name := range quoted.FindAllStringSubmatch(m[1], -1) {
				ptr := u.InstanceLocation + "/" + escapePointer(name[1])
				file, line := d.Where(ptr)
				add(Diag{Code: "schema", Message: displayPath(ptr) + ": unknown property", File: file, Line: line})
			}
			continue
		}
		file, line := d.Where(u.InstanceLocation)
		add(Diag{Code: "schema", Message: displayPath(u.InstanceLocation) + ": " + msg, File: file, Line: line})
	}
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].File != diags[j].File {
			return diags[i].File < diags[j].File
		}
		return diags[i].Line < diags[j].Line
	})
	return diags, nil
}

var (
	unknownProps = regexp.MustCompile(`^additional properties (.+) not allowed$`)
	quoted       = regexp.MustCompile(`'([^']+)'`)
)

func displayPath(ptr string) string {
	if ptr == "" {
		return "document"
	}
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return strings.Join(parts, ".")
}

// leafErrors collects the most specific errors of a validation tree: a
// parent's own "validation failed" adds nothing to the reasons below it.
func leafErrors(u *jsonschema.OutputUnit) []jsonschema.OutputUnit {
	if len(u.Errors) == 0 {
		if u.Error == nil {
			return nil
		}
		return []jsonschema.OutputUnit{*u}
	}
	var out []jsonschema.OutputUnit
	for i := range u.Errors {
		out = append(out, leafErrors(&u.Errors[i])...)
	}
	return out
}

// Format rewrites one YAML file in the canonical layout: two-space indents and
// the sections in the order written. Comments are kept. Formatting a
// formatted file changes nothing.
func Format(src []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 {
		return src, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
