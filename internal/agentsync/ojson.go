package agentsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

type jkind uint8

const (
	jScalar jkind = iota
	jObject
	jArray
)

// jnode is an order-preserving JSON value. Shared agent config files are
// edited in place, so unrelated keys must keep their order and values.
type jnode struct {
	kind jkind
	raw  []byte
	keys []string
	vals map[string]*jnode
	arr  []*jnode
}

// kv and obj build ordered JSON objects in code.
type kv struct {
	k string
	v any
}

type obj []kv

func newObject() *jnode { return &jnode{kind: jObject, vals: map[string]*jnode{}} }

func encodeScalar(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func toNode(v any) *jnode {
	switch t := v.(type) {
	case *jnode:
		return t
	case obj:
		n := newObject()
		for _, e := range t {
			n.set(e.k, toNode(e.v))
		}
		return n
	case map[string]string:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := newObject()
		for _, k := range keys {
			n.set(k, toNode(t[k]))
		}
		return n
	case []string:
		n := &jnode{kind: jArray}
		for _, s := range t {
			n.arr = append(n.arr, toNode(s))
		}
		return n
	case []any:
		n := &jnode{kind: jArray}
		for _, e := range t {
			n.arr = append(n.arr, toNode(e))
		}
		return n
	default:
		return &jnode{kind: jScalar, raw: encodeScalar(v)}
	}
}

func (n *jnode) get(key string) *jnode {
	if n == nil || n.kind != jObject {
		return nil
	}
	return n.vals[key]
}

func (n *jnode) set(key string, v *jnode) {
	if _, ok := n.vals[key]; !ok {
		n.keys = append(n.keys, key)
	}
	n.vals[key] = v
}

func (n *jnode) del(key string) {
	if _, ok := n.vals[key]; !ok {
		return
	}
	delete(n.vals, key)
	for i, k := range n.keys {
		if k == key {
			n.keys = append(n.keys[:i], n.keys[i+1:]...)
			break
		}
	}
}

func (n *jnode) str() string {
	if n == nil || n.kind != jScalar {
		return ""
	}
	var s string
	if json.Unmarshal(n.raw, &s) != nil {
		return ""
	}
	return s
}

func parseJSON(data []byte) (*jnode, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := decodeNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return n, nil
}

func decodeNode(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.set(kt.(string), v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &jnode{kind: jArray}
			for dec.More() {
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.arr = append(n.arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)
	case json.Number:
		return &jnode{kind: jScalar, raw: []byte(t.String())}, nil
	default:
		return &jnode{kind: jScalar, raw: encodeScalar(t)}, nil
	}
}

func (n *jnode) encode(buf *bytes.Buffer, indent string, depth int) {
	pad := func(d int) {
		for i := 0; i < d; i++ {
			buf.WriteString(indent)
		}
	}
	switch n.kind {
	case jScalar:
		buf.Write(n.raw)
	case jObject:
		if len(n.keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteString("{\n")
		for i, k := range n.keys {
			pad(depth + 1)
			buf.Write(encodeScalar(k))
			buf.WriteString(": ")
			n.vals[k].encode(buf, indent, depth+1)
			if i < len(n.keys)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		pad(depth)
		buf.WriteByte('}')
	case jArray:
		if len(n.arr) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, e := range n.arr {
			pad(depth + 1)
			e.encode(buf, indent, depth+1)
			if i < len(n.arr)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		pad(depth)
		buf.WriteByte(']')
	}
}

func (n *jnode) marshal(indent string) []byte {
	var buf bytes.Buffer
	n.encode(&buf, indent, 0)
	buf.WriteByte('\n')
	return buf.Bytes()
}

// canon renders the value with sorted keys and no whitespace so two
// semantically equal nodes hash identically regardless of key order.
func (n *jnode) canon(buf *bytes.Buffer) {
	switch n.kind {
	case jScalar:
		buf.Write(n.raw)
	case jObject:
		keys := append([]string(nil), n.keys...)
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(encodeScalar(k))
			buf.WriteByte(':')
			n.vals[k].canon(buf)
		}
		buf.WriteByte('}')
	case jArray:
		buf.WriteByte('[')
		for i, e := range n.arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			e.canon(buf)
		}
		buf.WriteByte(']')
	}
}

func (n *jnode) hash() string {
	var buf bytes.Buffer
	n.canon(&buf)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// detectIndent reports the indentation unit of an existing JSON document so
// the rewrite does not reformat the user's file.
func detectIndent(data []byte) string {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 || (line[0] != ' ' && line[0] != '\t') {
			continue
		}
		if line[0] == '\t' {
			return "\t"
		}
		n := 0
		for n < len(line) && line[n] == ' ' {
			n++
		}
		return string(bytes.Repeat([]byte(" "), n))
	}
	return "  "
}
