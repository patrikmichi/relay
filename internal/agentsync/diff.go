package agentsync

import (
	"fmt"
	"regexp"
	"strings"
)

const diffContext = 2

// maxDiffCells bounds the LCS table; edits are local, so the trimmed middle
// is normally tiny, and a pathological rewrite degrades to replace-all.
const maxDiffCells = 4_000_000

var (
	jsonPair = regexp.MustCompile(`("([^"\\]+)"\s*:\s*)"((?:[^"\\]|\\.)*)"`)
	tomlPair = regexp.MustCompile(`^(\s*"?([A-Za-z0-9_.\-]+)"?\s*=\s*)"((?:[^"\\]|\\.)*)"`)
	// secretFlag is a command-line option whose next argument is a secret.
	secretFlagLine = regexp.MustCompile(`(?i)"--?(?:token|auth-?token|api-?key|key|password|passwd|secret|bearer)"\s*,?\s*$`)
	flagWithValue  = regexp.MustCompile(`(?i)("--?(?:token|auth-?token|api-?key|key|password|passwd|secret|bearer)(?:=|"\s*,\s*"))((?:[^"\\]|\\.)*)"`)
	quotedValue    = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	// secretToken matches the shape of common credentials wherever they appear.
	secretToken = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,})`)
)

func isEnvRef(s string) bool { return strings.Contains(s, "${") || strings.Contains(s, "{env:") }

// secretName reports whether a key names a credential: a token, secret,
// password or key (also as one part of KEY_NAME or key-name).
func secretName(key string) bool {
	k := strings.ToLower(key)
	for _, w := range []string{"token", "secret", "password", "passwd", "pwd", "credential", "bearer", "authorization", "apikey", "api_key", "api-key", "private"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	for _, part := range strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if part == "key" || part == "pass" {
			return true
		}
	}
	return false
}

// redactor masks credentials in a stream of diff lines. It keeps one line of
// state: a "--token" argument on its own line makes the next line's string a
// secret, as in an args array printed one element per line.
type redactor struct{ afterFlag bool }

// redactLine masks values of secret-looking keys unless the value is an
// environment reference, which is what relay itself writes.
func redactLine(line string) string { return (&redactor{}).line(line) }

func (r *redactor) line(line string) string {
	flagged := r.afterFlag
	r.afterFlag = false
	if flagged && !isEnvRef(line) {
		line = quotedValue.ReplaceAllString(line, `"***"`)
	}
	if secretFlagLine.MatchString(line) {
		r.afterFlag = true
	}
	line = flagWithValue.ReplaceAllStringFunc(line, func(m string) string {
		if isEnvRef(m) {
			return m
		}
		i := flagWithValue.FindStringSubmatchIndex(m)
		return m[:i[3]] + `***"`
	})
	for _, re := range []*regexp.Regexp{jsonPair, tomlPair} {
		line = re.ReplaceAllStringFunc(line, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if !secretName(sub[2]) || isEnvRef(sub[3]) {
				return m
			}
			return sub[1] + `"***"`
		})
	}
	return secretToken.ReplaceAllString(line, "***")
}

// Diff renders a unified diff of the edit for --dry-run output.
func (e Edit) Diff() string {
	var a, b []string
	if e.Existed {
		a = splitLines(string(e.Before))
	}
	if !e.Remove {
		b = splitLines(string(e.After))
	}
	from, to := "a/"+e.Path, "b/"+e.Path
	if !e.Existed {
		from = "/dev/null"
	}
	if e.Remove {
		to = "/dev/null"
	}
	return unifiedDiff(from, to, a, b)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type diffOp struct {
	tag  byte
	line string
}

func unifiedDiff(from, to string, a, b []string) string {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]

	var ops []diffOp
	for _, l := range a[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, lcsDiff(midA, midB)...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}

	aPos := make([]int, len(ops)+1)
	bPos := make([]int, len(ops)+1)
	for i, op := range ops {
		aPos[i+1], bPos[i+1] = aPos[i], bPos[i]
		if op.tag != '+' {
			aPos[i+1]++
		}
		if op.tag != '-' {
			bPos[i+1]++
		}
	}

	var hunks [][2]int
	for i, op := range ops {
		if op.tag == ' ' {
			continue
		}
		lo, hi := max(0, i-diffContext), min(len(ops), i+diffContext+1)
		if n := len(hunks); n > 0 && lo <= hunks[n-1][1] {
			hunks[n-1][1] = hi
		} else {
			hunks = append(hunks, [2]int{lo, hi})
		}
	}

	// Redact in file order, one stream per side, before choosing hunks, so a
	// secret flag and its value are linked even when a hunk starts between them.
	oldSide, newSide := &redactor{}, &redactor{}
	masked := make([]string, len(ops))
	for i, op := range ops {
		switch op.tag {
		case ' ':
			masked[i] = oldSide.line(op.line)
			newSide.line(op.line)
		case '-':
			masked[i] = oldSide.line(op.line)
		default:
			masked[i] = newSide.line(op.line)
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	for _, h := range hunks {
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aPos[h[0]]+1, aPos[h[1]]-aPos[h[0]], bPos[h[0]]+1, bPos[h[1]]-bPos[h[0]])
		for i := h[0]; i < h[1]; i++ {
			out.WriteByte(ops[i].tag)
			out.WriteString(masked[i])
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func lcsDiff(a, b []string) []diffOp {
	if len(a) == 0 || len(b) == 0 || len(a)*len(b) > maxDiffCells {
		ops := make([]diffOp, 0, len(a)+len(b))
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	w := len(b) + 1
	t := make([]int32, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i*w+j] = t[(i+1)*w+j+1] + 1
			} else {
				t[i*w+j] = max(t[(i+1)*w+j], t[i*w+j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case t[(i+1)*w+j] >= t[i*w+j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}
