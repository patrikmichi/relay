package packs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPack = `
format: zuano.pack/v1
name: finance-invoicing
version: 1.2.0
displayName: Finance and invoicing
summary: Invoice and chase payments.
params:
  - { key: currency, label: Currency, type: enum, options: [EUR, USD], default: EUR }
  - { key: finance_group, label: Group, type: group }
connectors:
  - { slug: stripe, required: true, credentialScope: org }
groups:
  - { key: finance, from: params.finance_group }
skills:
  - { ref: "invoice-reconcile@2.1.0", assign: [finance] }
rules:
  - name: money-safety
    alwaysApply: true
    body: Amounts are in {{params.currency}}.
risk:
  matrix:
    - { service: stripe, risk: write, mode: approval }
automations:
  - key: check
    name: Check
    source: { kind: endpoint }
    action: { kind: skill_run, skill: invoice-reconcile, input: "Check {{event.id}} in {{params.currency}}" }
`

func TestParseAcceptsAValidPack(t *testing.T) {
	m, err := Parse([]byte(validPack))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := m.JSON()
	if err != nil || !bytes.Contains(raw, []byte(`"name":"finance-invoicing"`)) {
		t.Fatalf("JSON = %s, %v", raw, err)
	}
	if n, v := SplitRef("a@1.0.0"); n != "a" || v != "1.0.0" {
		t.Errorf("SplitRef = %q %q", n, v)
	}
}

func TestParseRejectsUnknownFieldsAndOversizedFiles(t *testing.T) {
	if _, err := Parse([]byte("format: zuano.pack/v1\nbogus: 1\n")); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := Parse(bytes.Repeat([]byte("a"), MaxManifestBytes+1)); err == nil {
		t.Error("oversized file accepted")
	}
}

func TestValidateReportsEachProblem(t *testing.T) {
	bad := strings.NewReplacer(
		"format: zuano.pack/v1", "format: other/v1",
		"name: finance-invoicing", "name: Finance",
		"version: 1.2.0", "version: one",
		"from: params.finance_group", "from: params.missing",
		"invoice-reconcile@2.1.0", "invoice-reconcile",
		"{{params.currency}}.", "{{params.nope}}.",
		"kind: skill_run", "kind: run_code",
		"{{event.id}}", "{{secret.x}}",
		"{ service: stripe, risk: write, mode: approval }", "{ service: '*', risk: destructive, mode: approval }",
	).Replace(validPack)
	m, err := Parse([]byte(bad))
	if err != nil {
		t.Fatal(err)
	}
	err = m.Validate()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"format must be", "kebab-case", "must be semver", "not a param", "exact version", "unknown placeholder {{params.nope}}", "run_code", "unknown placeholder {{secret.x}}", "only cover read and write"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestValidateChecksParamsAndRules(t *testing.T) {
	m := &Manifest{Format: Format, Name: "p", Version: "1.0.0", DisplayName: "P", Summary: "s",
		Params: []Param{{Key: "A", Type: "enum"}, {Key: "b", Type: "bogus"}, {Key: "c", Type: "enum"}, {Key: "d", Type: "string"}, {Key: "d", Type: "string"}},
		Rules:  []Rule{{Name: "r", Body: "x"}, {Name: "r", Body: "x"}, {Name: "big", Body: strings.Repeat("a", MaxRuleBytes+1)}, {Name: "empty", Body: " "}},
	}
	for i := 0; i < MaxRules; i++ {
		m.Rules = append(m.Rules, Rule{Name: "x", Body: "y"})
	}
	err := m.Validate().Error()
	for _, want := range []string{"lower_snake_case", "unknown type", "needs options", "defined twice", "rules; the limit", "bytes; the limit", "has no body"} {
		if !strings.Contains(err, want) {
			t.Errorf("error lacks %q:\n%s", want, err)
		}
	}
	if (&Manifest{Format: Format, Name: "p", Version: "1.0.0"}).Validate() == nil {
		t.Error("a pack without displayName and summary is valid")
	}
}

func TestPlanRenderGroupsByAreaAndMarksConflictsAndSkips(t *testing.T) {
	var p Plan
	doc := `{"planId":"ppl_1","planHash":"h","mode":"install","pack":{"slug":"finance","version":"1.0.0"},
	 "ops":[
	  {"seq":1,"kind":"connector.enable","target":{"slug":"stripe"},"before":null,"after":{"enabled":true},"status":"apply","loosens":false},
	  {"seq":2,"kind":"risk.cell","target":{"service":"stripe","risk":"outward"},"before":"off","after":"approval","status":"conflict","loosens":true,"reason":"edited by an admin"},
	  {"seq":3,"kind":"group.create","target":{"name":"Finance"},"status":"skipped_plan","reason":"groups need a higher plan","requires":{"requiredPlan":"pro"}},
	  {"seq":4,"kind":"resource.enable","target":{"skill":"x"},"status":"noop"},
	  {"seq":5,"kind":"mystery","target":{},"status":"apply"}],
	 "summary":{"apply":2,"noop":1,"conflict":1,"skipped":1,"loosening":1}}`
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p.Render(&out)
	for _, want := range []string{"install finance 1.0.0", "Connectors", "+ 1", "(none) -> {\"enabled\":true}", "Risk and approvals", "! 2", "[loosens]", "keep your value or take the pack's (edited by an admin)", "Groups", "skipped: groups need a higher plan (needs the pro plan)", "Skills and bundles", "= 4", "Other", "2 to apply, 1 unchanged, 1 conflicts, 1 skipped, 1 loosen a control"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("render lacks %q:\n%s", want, out.String())
		}
	}
	if got := p.Conflicts(); len(got) != 1 || got[0] != 2 {
		t.Errorf("Conflicts = %v", got)
	}
	if !p.Loosens() {
		t.Error("Loosens = false")
	}
	p.Ops[1].Status = StatusNoop
	if p.Loosens() {
		t.Error("an unchanged operation counts as loosening")
	}
}

func TestStateRoundTripsAndIgnoresOtherGateways(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync", "assignments.json")
	if s, err := LoadState(path, "https://a"); err != nil || s.Revision != 0 {
		t.Fatalf("missing = %+v, %v", s, err)
	}
	s := State{Gateway: "https://a", Revision: 4, Assignments: Assignments{Revision: 4, Profiles: []Profile{{Name: "p"}}}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path, "https://a")
	if err != nil || got.Revision != 4 || len(got.Assignments.Profiles) != 1 {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if other, _ := LoadState(path, "https://b"); other.Revision != 0 {
		t.Error("state of another gateway was used")
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadState(path, "https://a"); err != nil || got.Revision != 0 {
		t.Errorf("corrupt = %+v, %v", got, err)
	}
	if _, err := LoadState(t.TempDir(), "x"); err == nil {
		t.Error("a directory was read as a file")
	}
}

func TestStatePathIsUnderTheConfigDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, err := StatePath()
	if err != nil || !strings.HasSuffix(filepath.ToSlash(p), ".config/zuano/sync/assignments.json") {
		t.Errorf("StatePath = %q, %v", p, err)
	}
}
