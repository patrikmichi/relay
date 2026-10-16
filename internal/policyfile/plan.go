package policyfile

import (
	"fmt"
	"io"
	"strings"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// MarkdownLimit is the longest plan comment the gateway's hosts accept.
const MarkdownLimit = 60000

// Plan is what applying a policy would change, as the gateway computes it.
type Plan struct {
	ID             string   `json:"planId"`
	ParentID       string   `json:"parentId"`
	Classification string   `json:"classification"`
	Changes        []Change `json:"changes"`
	Impact         []Impact `json:"impact"`
	Loosening      []string `json:"loosening"`
	TopIdentities  []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Newly string `json:"newlyReachable"`
	} `json:"topIdentities"`
	Lifecycle []string `json:"lifecycle"`
	Packs     []string `json:"packs"`
	Errors    []Diag   `json:"errors"`
	URL       string   `json:"url"`
}

// Change is one entry of the document diff.
type Change struct {
	Section string `json:"section"`
	Op      string `json:"op"`
	Path    string `json:"path"`
	File    string `json:"file"`
	Line    int    `json:"line"`
}

// Impact is one row of the decision diff: a service and risk class whose mode
// changes for some identities.
type Impact struct {
	Service    string `json:"service"`
	Risk       string `json:"risk"`
	From       string `json:"from"`
	To         string `json:"to"`
	Identities int    `json:"identities"`
	Loosening  bool   `json:"loosening"`
}

// HasChanges reports whether applying would change anything.
func (p Plan) HasChanges() bool { return len(p.Changes) > 0 }

// IsLoosening reports whether any change makes the policy less strict.
func (p Plan) IsLoosening() bool {
	return p.Classification == "loosening" || p.Classification == "mixed" || len(p.Loosening) > 0
}

func op(o string) string {
	switch o {
	case "added":
		return "+"
	case "removed":
		return "-"
	}
	return "~"
}

// RenderTable writes the plan for a terminal.
func (p Plan) RenderTable(w io.Writer) {
	if !p.HasChanges() {
		fmt.Fprintln(w, "No changes. The live policy matches these files.")
		return
	}
	fmt.Fprintf(w, "Policy plan (%s): %d change(s)\n\n", orNone(p.Classification), len(p.Changes))
	for _, c := range p.Changes {
		loc := ""
		if c.File != "" {
			loc = fmt.Sprintf("  %s:%d", textsafe.Clean(c.File), c.Line)
		}
		fmt.Fprintf(w, "  %s %s%s\n", op(c.Op), textsafe.Clean(c.Path), loc)
	}
	if len(p.Packs) > 0 {
		fmt.Fprintf(w, "\nPacks: %s\n", textsafe.Clean(strings.Join(p.Packs, "; ")))
	}
	if len(p.Impact) > 0 {
		fmt.Fprintln(w, "\nImpact (loosening first)")
		for _, i := range sortedImpact(p.Impact) {
			mark := ""
			if i.Loosening {
				mark = "  [loosens]"
			}
			fmt.Fprintf(w, "  %s %s: %s -> %s for %d identities%s\n", textsafe.Clean(i.Service), textsafe.Clean(i.Risk), textsafe.Clean(i.From), textsafe.Clean(i.To), i.Identities, mark)
		}
	}
	for _, l := range p.Lifecycle {
		fmt.Fprintf(w, "  lifecycle: %s\n", textsafe.Clean(l))
	}
	if p.URL != "" {
		fmt.Fprintf(w, "\nFull plan: %s\n", textsafe.Clean(p.URL))
	}
}

func orNone(s string) string {
	if s == "" {
		return "no classification"
	}
	return textsafe.Clean(s)
}

func sortedImpact(in []Impact) []Impact {
	out := make([]Impact, 0, len(in))
	for _, i := range in {
		if i.Loosening {
			out = append(out, i)
		}
	}
	for _, i := range in {
		if !i.Loosening {
			out = append(out, i)
		}
	}
	return out
}

// RenderMarkdown writes the plan for a pull request comment, cut to the
// limit with a pointer to the full plan.
func (p Plan) RenderMarkdown() string {
	var b strings.Builder
	if !p.HasChanges() {
		b.WriteString("### Policy plan\n\nNo changes. The live policy matches these files.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "### Policy plan: %s\n\n%d change(s).\n\n", orNone(p.Classification), len(p.Changes))
	if len(p.Loosening) > 0 {
		b.WriteString("**Loosens the policy:**\n\n")
		for _, l := range p.Loosening {
			fmt.Fprintf(&b, "- %s\n", mdEscape(l))
		}
		b.WriteString("\n")
	}
	b.WriteString("| | Change | Where |\n|---|---|---|\n")
	for _, c := range p.Changes {
		loc := ""
		if c.File != "" {
			loc = fmt.Sprintf("`%s:%d`", c.File, c.Line)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s |\n", op(c.Op), mdEscape(c.Path), loc)
	}
	if len(p.Impact) > 0 {
		b.WriteString("\n| Service | Risk | Change | Identities |\n|---|---|---|---|\n")
		for _, i := range sortedImpact(p.Impact) {
			flag := ""
			if i.Loosening {
				flag = " (loosens)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s to %s%s | %d |\n", mdEscape(i.Service), mdEscape(i.Risk), mdEscape(i.From), mdEscape(i.To), flag, i.Identities)
		}
	}
	for _, l := range p.Lifecycle {
		fmt.Fprintf(&b, "\n- lifecycle: %s", mdEscape(l))
	}
	if p.URL != "" {
		fmt.Fprintf(&b, "\n\n[Full plan](%s)\n", p.URL)
	}
	out := b.String()
	if len(out) > MarkdownLimit {
		cut := out[:MarkdownLimit-200]
		cut = cut[:strings.LastIndexByte(cut, '\n')+1]
		out = cut + "\n_The plan is longer than a comment can hold._"
		if p.URL != "" {
			out += fmt.Sprintf(" [Full plan](%s)\n", p.URL)
		}
	}
	return out
}

func mdEscape(s string) string {
	s = textsafe.Clean(s)
	for _, c := range []string{"|", "`", "<", ">"} {
		s = strings.ReplaceAll(s, c, `\`+c)
	}
	return strings.ReplaceAll(s, "\n", " ")
}
