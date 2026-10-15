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

var secretLine = regexp.MustCompile(`(?i)("[^"]*(token|secret|password|api[_-]?key|authorization|bearer|credential)[^"]*"\s*:\s*)"[^"]*"`)

// redactLine masks values of secret-looking keys unless the value is an
// environment reference, which is what relay itself writes.
func redactLine(line string) string {
	return secretLine.ReplaceAllStringFunc(line, func(m string) string {
		if strings.Contains(m, "${") || strings.Contains(m, "{env:") {
			return m
		}
		idx := secretLine.FindStringSubmatchIndex(m)
		return m[:idx[3]] + `"***"`
	})
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

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	for _, h := range hunks {
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aPos[h[0]]+1, aPos[h[1]]-aPos[h[0]], bPos[h[0]]+1, bPos[h[1]]-bPos[h[0]])
		for _, op := range ops[h[0]:h[1]] {
			out.WriteByte(op.tag)
			out.WriteString(redactLine(op.line))
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
