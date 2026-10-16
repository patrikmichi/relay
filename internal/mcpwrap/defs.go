package mcpwrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/patrikmichi/zuano/internal/signing"
)

// Definition is a tool as an MCP server describes it.
type Definition = map[string]any

// hashedMeta lists the _meta keys that are part of a definition; the rest are
// runtime values.
var hashedMeta = map[string]bool{"ui": true}

// normalizeValue applies Unicode NFC to every string and drops _meta keys that
// are not part of a definition.
func normalizeValue(v any, inMeta bool) any {
	switch t := v.(type) {
	case string:
		return norm.NFC.String(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalizeValue(x, false)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if inMeta && !hashedMeta[k] {
				continue
			}
			out[norm.NFC.String(k)] = normalizeValue(x, k == "_meta")
		}
		return out
	}
	return v
}

// DefHash identifies a definition: the sha256 of its canonical JSON (sorted
// keys, no whitespace, NFC).
func DefHash(def Definition) (string, error) {
	canon, err := signing.CanonicalJSON(normalizeValue(def, false))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// StripInvisible removes characters a person cannot see but a model reads:
// zero-width and bidirectional controls and the tag block.
func StripInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		if isInvisible(r) {
			return -1
		}
		return r
	}, s)
}

func isInvisible(r rune) bool {
	switch {
	case r >= 0xE0000 && r <= 0xE007F:
		return true
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069, r == 0xFEFF:
		return true
	}
	return unicode.Is(unicode.Cf, r) && r != 0x00AD
}

// cleanDefinition strips invisible characters from every string of a
// definition, for serving.
func cleanDefinition(v any) any {
	switch t := v.(type) {
	case string:
		return StripInvisible(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = cleanDefinition(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = cleanDefinition(x)
		}
		return out
	}
	return v
}

func nameOf(def Definition) string {
	n, _ := def["name"].(string)
	return n
}

// decodeDefinitions reads the tools of a tools/list result.
func decodeDefinitions(result json.RawMessage) (defs []Definition, rest map[string]json.RawMessage, err error) {
	if err = json.Unmarshal(result, &rest); err != nil {
		return nil, nil, err
	}
	if raw, ok := rest["tools"]; ok {
		err = json.Unmarshal(raw, &defs)
	}
	return defs, rest, err
}
