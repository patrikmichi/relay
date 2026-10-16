package redact

import (
	"encoding/json"

	"github.com/patrikmichi/zuano/internal/transcripts"
)

// Session redacts every text of a session in place and records what was
// removed in its report. Argument values under a key that looks like a secret
// are replaced whatever they look like.
func Session(s *transcripts.Session, terms []string) {
	r := New(terms)
	s.Title = r.Text(s.Title)
	for i := range s.Turns {
		t := &s.Turns[i]
		t.Text = r.Text(t.Text)
		for j := range t.ToolCalls {
			c := &t.ToolCalls[j]
			c.ArgsPreview = r.args(c.ArgsPreview)
			c.ResultPreview = r.Text(c.ResultPreview)
		}
	}
	placeholders := 0
	for _, m := range r.nums {
		placeholders += len(m)
	}
	counts := map[string]int{}
	for _, class := range []string{ClassSecret, ClassEmail, ClassPhone, ClassIBAN, ClassCard, ClassNationalID, ClassIP, ClassURL, ClassPath, ClassTerm} {
		counts[class] = r.counts[class]
	}
	s.Redaction = transcripts.Redaction{RulesVersion: rule.version, ClientRedacted: true, Counts: counts, Placeholders: placeholders}
}

// args redacts a JSON arguments preview structurally when it parses, and as
// text when it was cut short.
func (r *Redactor) args(preview string) string {
	var v any
	if json.Unmarshal([]byte(preview), &v) != nil {
		return r.Text(preview)
	}
	out, err := json.Marshal(r.value(v))
	if err != nil {
		return r.Text(preview)
	}
	return string(out)
}

func (r *Redactor) value(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if rule.secretKey.MatchString(k) && x != nil {
				raw, _ := json.Marshal(x)
				t[k] = r.placeholder(ClassSecret, string(raw))
				continue
			}
			t[k] = r.value(x)
		}
	case []any:
		for i, x := range t {
			t[i] = r.value(x)
		}
	case string:
		return r.Text(t)
	}
	return v
}
