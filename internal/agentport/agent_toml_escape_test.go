package agentport

import (
	"strings"
	"testing"
)

func TestScanTOMLMultilineBody_Escapes(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"quote and backslash", `a\"b\\c"""`, `a"b\c`},
		{"control escapes", `\n\t\r\b\f"""`, "\n\t\r\b\f"},
		{"short unicode", `café"""`, "café"},
		{"long unicode", `\U0001F600"""`, "\U0001F600"},
		{"line continuation", "one \\\n    two\"\"\"", "one two"},
		{"continuation with trailing spaces", "one \\  \n\n  two\"\"\"", "one two"},
		{"leading newline trimmed", "\nbody\"\"\"", "body"},
		{"content quotes before delimiter", `say ""hi"""""`, `say ""hi""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := scanTOMLMultilineBody(tc.raw)
			if err != nil {
				t.Fatalf("scan(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("scan(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestScanTOMLMultilineBody_RejectsMalformedEscapes(t *testing.T) {
	cases := map[string]string{
		"dangling backslash":       `abc\`,
		"truncated short unicode":  `\u12`,
		"truncated long unicode":   `\U0001F6`,
		"non-hex unicode":          `\uZZZZ"""`,
		"unknown escape":           `\q"""`,
		"space not before newline": `a\ b"""`,
		"unterminated":             `no closing delimiter`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if got, _, err := scanTOMLMultilineBody(raw); err == nil {
				t.Fatalf("scan(%q) = %q, want an error", raw, got)
			}
		})
	}
}

// Every string up to six symbols over the characters the encoder treats
// specially must survive encode -> decode unchanged.
func TestEncodeTOMLMultilineString_RoundTripsThroughDecoder(t *testing.T) {
	alphabet := []string{`\`, `"`, "a", "\n", "\r", " "}
	var check func(s string, depth int)
	check = func(s string, depth int) {
		enc := encodeTOMLMultilineString(s)
		got, _, err := scanTOMLMultilineBody(strings.TrimPrefix(enc, `"""`))
		if err != nil || got != s {
			t.Fatalf("round trip of %q via %q = %q, %v", s, enc, got, err)
		}
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			check(s+c, depth-1)
		}
	}
	check("", 6)
}
