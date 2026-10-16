package signing

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// Go's encoder escapes these two characters; JSON.stringify writes them raw.
var jsLineSeparators = strings.NewReplacer("\\u2028", string(rune(0x2028)), "\\u2029", string(rune(0x2029)))

// Message is the exact byte string that is signed: the canonical JSON (keys
// sorted at every depth, no whitespace) of {id, sha256, verdict, version},
// where verdict is the decoded scan-verdict object, not its base64 header.
func Message(a Artifact) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Verdict))
	if err != nil {
		return nil, errors.New("the scan verdict header is not base64")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var verdict any
	if err := dec.Decode(&verdict); err != nil {
		return nil, errors.New("the scan verdict header is not base64 JSON")
	}
	var b bytes.Buffer
	if err := canonicalJSON(&b, map[string]any{"id": a.ID, "sha256": a.SHA256, "verdict": verdict, "version": a.Version}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// CanonicalJSON returns v, a decoded JSON document (maps, slices and
// scalars), in the form the gateway signs.
func CanonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := canonicalJSON(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// canonicalJSON writes v the way the gateway's canonicalJson does: object keys
// sorted at every depth, no whitespace, and <, > and & left unescaped.
func canonicalJSON(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := scalar(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := canonicalJSON(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonicalJSON(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return scalar(b, t)
	}
	return nil
}

func scalar(b *bytes.Buffer, v any) error {
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return err
	}
	b.WriteString(jsLineSeparators.Replace(strings.TrimSuffix(enc.String(), "\n")))
	return nil
}
