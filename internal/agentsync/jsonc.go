package agentsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JSONC files (.vscode/mcp.json, opencode.jsonc) hold comments and trailing
// commas that a JSON round trip would lose. They are edited at the text
// level instead: the document is scanned into value spans, and each change
// replaces, inserts or removes exactly the bytes of one object member.
// Everything else, comments and commas included, is left as it was.

// jmember is one "key": value pair of an object.
type jmember struct {
	key      string
	keyStart int
	val      *jspan
	// commaAt is the index of the comma after the member, or -1.
	commaAt int
}

// jspan is a parsed value with the byte range it occupies.
type jspan struct {
	kind       jkind
	start, end int
	members    []jmember // objects
}

func (s *jspan) child(key string) (int, *jmember) {
	if s.kind != jObject {
		return -1, nil
	}
	for i := range s.members {
		if s.members[i].key == key {
			return i, &s.members[i]
		}
	}
	return -1, nil
}

type jscanner struct {
	data []byte
	i    int
}

var errJSONC = errors.New("not a JSON document")

// skip moves past whitespace and comments.
func (s *jscanner) skip() error {
	for s.i < len(s.data) {
		switch c := s.data[s.i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.i++
		case c == '/' && s.i+1 < len(s.data) && s.data[s.i+1] == '/':
			for s.i < len(s.data) && s.data[s.i] != '\n' {
				s.i++
			}
		case c == '/' && s.i+1 < len(s.data) && s.data[s.i+1] == '*':
			end := bytes.Index(s.data[s.i+2:], []byte("*/"))
			if end < 0 {
				return fmt.Errorf("%w: unterminated comment", errJSONC)
			}
			s.i += end + 4
		default:
			return nil
		}
	}
	return nil
}

func (s *jscanner) str() (string, error) {
	start := s.i
	s.i++ // opening quote
	for s.i < len(s.data) {
		switch s.data[s.i] {
		case '\\':
			s.i += 2
			continue
		case '"':
			s.i++
			var out string
			if err := json.Unmarshal(s.data[start:s.i], &out); err != nil {
				return "", fmt.Errorf("%w: %v", errJSONC, err)
			}
			return out, nil
		}
		s.i++
	}
	return "", fmt.Errorf("%w: unterminated string", errJSONC)
}

func (s *jscanner) value() (*jspan, error) {
	if err := s.skip(); err != nil {
		return nil, err
	}
	if s.i >= len(s.data) {
		return nil, fmt.Errorf("%w: unexpected end", errJSONC)
	}
	v := &jspan{start: s.i}
	switch s.data[s.i] {
	case '{':
		v.kind = jObject
		s.i++
		for {
			if err := s.skip(); err != nil {
				return nil, err
			}
			if s.i >= len(s.data) {
				return nil, fmt.Errorf("%w: unterminated object", errJSONC)
			}
			if s.data[s.i] == '}' {
				s.i++
				break
			}
			if s.data[s.i] != '"' {
				return nil, fmt.Errorf("%w: expected a key at offset %d", errJSONC, s.i)
			}
			m := jmember{keyStart: s.i, commaAt: -1}
			var err error
			if m.key, err = s.str(); err != nil {
				return nil, err
			}
			if err = s.skip(); err != nil {
				return nil, err
			}
			if s.i >= len(s.data) || s.data[s.i] != ':' {
				return nil, fmt.Errorf("%w: expected ':' at offset %d", errJSONC, s.i)
			}
			s.i++
			if m.val, err = s.value(); err != nil {
				return nil, err
			}
			if err = s.skip(); err != nil {
				return nil, err
			}
			if s.i < len(s.data) && s.data[s.i] == ',' {
				m.commaAt = s.i
				s.i++
			}
			v.members = append(v.members, m)
		}
	case '[':
		v.kind = jArray
		s.i++
		for {
			if err := s.skip(); err != nil {
				return nil, err
			}
			if s.i >= len(s.data) {
				return nil, fmt.Errorf("%w: unterminated array", errJSONC)
			}
			if s.data[s.i] == ']' {
				s.i++
				break
			}
			if _, err := s.value(); err != nil {
				return nil, err
			}
			if err := s.skip(); err != nil {
				return nil, err
			}
			if s.i < len(s.data) && s.data[s.i] == ',' {
				s.i++
			}
		}
	case '"':
		v.kind = jScalar
		if _, err := s.str(); err != nil {
			return nil, err
		}
	default:
		v.kind = jScalar
		for s.i < len(s.data) && !strings.ContainsRune(" \t\r\n,}]/", rune(s.data[s.i])) {
			s.i++
		}
		if s.i == v.start {
			return nil, fmt.Errorf("%w: unexpected %q at offset %d", errJSONC, s.data[s.i], s.i)
		}
	}
	v.end = s.i
	return v, nil
}

func scanJSONC(data []byte) (*jspan, error) {
	s := &jscanner{data: data}
	root, err := s.value()
	if err != nil {
		return nil, err
	}
	if err := s.skip(); err != nil {
		return nil, err
	}
	if s.i != len(data) {
		return nil, fmt.Errorf("%w: data after the value", errJSONC)
	}
	if root.kind != jObject {
		return nil, fmt.Errorf("%w: the top level is not an object", errJSONC)
	}
	return root, nil
}

// loadJSONC reads a document with comments or trailing commas into a node
// tree for reading and comparing. The tree is never written back.
func loadJSONC(data []byte) (root *jnode, indent string, ok bool) {
	if _, err := scanJSONC(data); err != nil {
		return nil, "", false
	}
	n, err := parseJSON(stripJSONC(data))
	if err != nil || n.kind != jObject {
		return nil, "", false
	}
	return n, detectIndent(data), true
}

// jmut is one change to a member of the object at the path being edited.
type jmut struct {
	key string
	val *jnode
	del bool
}

// editJSONC applies mutations to the object at objPath, keeping every byte
// outside the touched members. The result is checked to still be valid JSONC.
func editJSONC(data []byte, objPath []string, muts []jmut, unit string) ([]byte, error) {
	style := styleOf(data)
	text := bytes.ReplaceAll(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), []byte("\r\n"), []byte("\n"))
	var err error
	for _, m := range muts {
		if m.del {
			text, err = deleteJSONCMember(text, objPath, m.key)
		} else {
			text, err = setJSONCMember(text, objPath, m.key, m.val, unit)
		}
		if err != nil {
			return nil, err
		}
		if _, err = scanJSONC(text); err != nil {
			return nil, fmt.Errorf("the edit would leave invalid JSON: %w", err)
		}
	}
	return style.apply(text), nil
}

// walk returns the object at path, the deepest existing prefix and how many
// segments matched, so a missing tail can be created.
func walk(root *jspan, path []string) (obj *jspan, depth int) {
	cur := root
	for _, seg := range path {
		_, m := cur.child(seg)
		if m == nil || m.val.kind != jObject {
			break
		}
		cur = m.val
		depth++
	}
	return cur, depth
}

func setJSONCMember(text []byte, objPath []string, key string, val *jnode, unit string) ([]byte, error) {
	root, err := scanJSONC(text)
	if err != nil {
		return nil, err
	}
	obj, depth := walk(root, objPath)
	if depth < len(objPath) {
		// A path segment is missing: add the whole missing branch at once.
		if _, m := obj.child(objPath[depth]); m != nil {
			return nil, fmt.Errorf("%s is not an object", strings.Join(objPath[:depth+1], "."))
		}
		branch := newObject()
		branch.set(key, val)
		for i := len(objPath) - 1; i > depth; i-- {
			outer := newObject()
			outer.set(objPath[i], branch)
			branch = outer
		}
		return insertJSONCMember(text, obj, objPath[depth], branch, unit), nil
	}
	if _, m := obj.child(key); m != nil {
		indent := lineIndent(text, m.keyStart)
		return replaceRange(text, m.val.start, m.val.end, renderJSONC(val, unit, indent)), nil
	}
	return insertJSONCMember(text, obj, key, val, unit), nil
}

func deleteJSONCMember(text []byte, objPath []string, key string) ([]byte, error) {
	root, err := scanJSONC(text)
	if err != nil {
		return nil, err
	}
	obj, depth := walk(root, objPath)
	if depth < len(objPath) {
		return text, nil
	}
	i, m := obj.child(key)
	if m == nil {
		return text, nil
	}
	start, end := m.keyStart, m.val.end
	if m.commaAt >= 0 {
		end = m.commaAt + 1
	} else if i > 0 && obj.members[i-1].commaAt >= 0 {
		// The last member has no comma of its own: drop the previous one's.
		text = replaceRange(text, obj.members[i-1].commaAt, obj.members[i-1].commaAt+1, "")
		start, end = start-1, end-1
	}
	// A member alone on its lines takes its lines with it.
	ls := bytes.LastIndexByte(text[:start], '\n') + 1
	le := bytes.IndexByte(text[end:], '\n')
	if le < 0 {
		le = len(text)
	} else {
		le += end
	}
	if len(bytes.TrimSpace(text[ls:start])) == 0 && len(bytes.TrimSpace(text[end:le])) == 0 {
		if le < len(text) {
			le++
		}
		return replaceRange(text, ls, le, ""), nil
	}
	return replaceRange(text, start, end, ""), nil
}

func insertJSONCMember(text []byte, obj *jspan, key string, val *jnode, unit string) []byte {
	closeAt := obj.end - 1
	var memberIndent string
	if len(obj.members) > 0 {
		memberIndent = lineIndent(text, obj.members[0].keyStart)
	} else {
		memberIndent = lineIndent(text, obj.start) + unit
	}
	member := jsonString(key) + ": " + renderJSONC(val, unit, memberIndent)
	if len(obj.members) == 0 {
		closeIndent := lineIndent(text, obj.start)
		// Replace whatever blank space sits between the braces.
		if len(bytes.TrimSpace(text[obj.start+1:closeAt])) == 0 {
			return replaceRange(text, obj.start+1, closeAt, "\n"+memberIndent+member+"\n"+closeIndent)
		}
		return replaceRange(text, obj.start+1, obj.start+1, "\n"+memberIndent+member+",")
	}
	last := obj.members[len(obj.members)-1]
	after := last.val.end
	trailing := last.commaAt >= 0
	if trailing {
		after = last.commaAt + 1
	}
	// A comment after the last member belongs to that member: the new one
	// goes on the line below it.
	lineEnd := len(text)
	if i := bytes.IndexByte(text[after:], '\n'); i >= 0 {
		lineEnd = after + i
	}
	rest := bytes.TrimSpace(text[after:lineEnd])
	insertAt := after
	if bytes.HasPrefix(rest, []byte("//")) || (bytes.HasPrefix(rest, []byte("/*")) && bytes.HasSuffix(rest, []byte("*/"))) {
		insertAt = lineEnd
	}
	if trailing {
		// Trailing commas are in use: keep the style.
		return replaceRange(text, insertAt, insertAt, "\n"+memberIndent+member+",")
	}
	if insertAt != after {
		// Edit the later position first so the earlier offset stays valid.
		text = replaceRange(text, lineEnd, lineEnd, "\n"+memberIndent+member)
		return replaceRange(text, after, after, ",")
	}
	return replaceRange(text, after, after, ",\n"+memberIndent+member)
}

func renderJSONC(v *jnode, unit, indent string) string {
	out := strings.TrimRight(string(v.marshal(unit)), "\n")
	return strings.ReplaceAll(out, "\n", "\n"+indent)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// lineIndent is the leading whitespace of the line holding offset.
func lineIndent(text []byte, offset int) string {
	ls := bytes.LastIndexByte(text[:offset], '\n') + 1
	end := ls
	for end < len(text) && (text[end] == ' ' || text[end] == '\t') {
		end++
	}
	return string(text[ls:end])
}

func replaceRange(text []byte, from, to int, with string) []byte {
	out := make([]byte, 0, len(text)-(to-from)+len(with))
	out = append(out, text[:from]...)
	out = append(out, with...)
	return append(out, text[to:]...)
}
