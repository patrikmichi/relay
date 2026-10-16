package agentsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// SetJSONMember sets key to value in the object at path inside a JSON or JSONC
// settings document, creating the object when it is missing. Only the bytes of
// that member change: comments, ordering and indentation elsewhere stay as
// they were. An empty document starts as {}.
func SetJSONMember(data []byte, path []string, key string, value any) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}\n")
	}
	return setJSONCMember(data, path, key, plainNode(value), "  ")
}

// DeleteJSONMember removes key from the object at path, if it is there.
func DeleteJSONMember(data []byte, path []string, key string) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return data, nil
	}
	return deleteJSONCMember(data, path, key)
}

// ReadJSONValue decodes the value at path.key of a JSON or JSONC document.
func ReadJSONValue(data []byte, path []string, key string) (value any, found bool, err error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false, nil
	}
	var doc any
	if err := json.Unmarshal(stripJSONC(data), &doc); err != nil {
		return nil, false, fmt.Errorf("not valid JSON: %w", err)
	}
	cur := doc
	for _, seg := range append(append([]string(nil), path...), key) {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		if cur, ok = m[seg]; !ok {
			return nil, false, nil
		}
	}
	return cur, true, nil
}

// plainNode converts decoded JSON values to nodes, with object keys sorted.
func plainNode(v any) *jnode {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := newObject()
		for _, k := range keys {
			n.set(k, plainNode(t[k]))
		}
		return n
	case []any:
		n := &jnode{kind: jArray}
		for _, e := range t {
			n.arr = append(n.arr, plainNode(e))
		}
		return n
	case []string:
		n := &jnode{kind: jArray}
		for _, e := range t {
			n.arr = append(n.arr, plainNode(e))
		}
		return n
	}
	return toNode(v)
}
