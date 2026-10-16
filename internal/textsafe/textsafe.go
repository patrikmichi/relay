// Package textsafe makes text from a gateway or a repository safe to print on
// a terminal.
package textsafe

import (
	"fmt"
	"strings"
	"unicode"
)

// Clean removes C0 and C1 control characters, including escape and newline,
// so the text cannot move the cursor or rewrite earlier terminal output.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Strict is Clean for text written by a party you do not trust, such as an
// agent card: it also removes the format characters that rewrite what a line
// seems to say (bidirectional overrides, zero-width and tag characters).
func Strict(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// Block is Clean for multi-line text: it keeps newlines and tabs.
func Block(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Reveal shows control characters other than newline and tab as \xNN, for
// text a person must review before approving it: dropping them would hide
// what the text does.
func Reveal(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			fmt.Fprintf(&b, "\\x%02x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
