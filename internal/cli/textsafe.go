package cli

import (
	"fmt"
	"strings"
	"unicode"
)

// cleanText removes C0 and C1 control characters, including escape and
// newline, so text from a gateway or a repository cannot move the cursor or
// rewrite earlier terminal output.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// cleanBlock is cleanText for multi-line text: it keeps newlines and tabs.
func cleanBlock(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// revealControls shows control characters as \xNN, for text a person must
// review before approving it: hiding them would hide what the text does.
func revealControls(s string) string {
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
