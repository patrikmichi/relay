package policyfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/brand"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const rootFile = `version: 1
org: acme
risk:
  matrix:
    "*":    { write: approval, destructive: off, outward: off }
    stripe: { write: approval, destructive: approval }
  overrides:
    - id: small-refunds
      tool: stripe.create_refund
      mode: approval
      when: { all: [ { param: amount, op: lte, value: 100 } ] }
`

func TestLoadMergesFilesBySection(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "policy.yaml", rootFile)
	write(t, dir, "teams/groups.yml", "groups:\n  finance: { description: Finance, members: scim }\n")
	write(t, dir, ".hidden/ignored.yaml", "x: 1\n")
	write(t, dir, "notes.txt", "not yaml")
	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Files[0].Path != "policy.yaml" || d.Files[1].Path != "teams/groups.yml" {
		t.Errorf("files = %+v", d.Files)
	}
	if d.Value["org"] != "acme" || d.Value["groups"] == nil {
		t.Errorf("value = %v", d.Value)
	}
	diags, err := d.Validate()
	if err != nil || len(diags) != 0 {
		t.Errorf("diags = %v, %v", diags, err)
	}
	canon, err := d.CanonicalJSON()
	if err != nil || !strings.HasPrefix(string(canon), `{"groups":`) {
		t.Errorf("canonical = %.40s, %v", canon, err)
	}
	if f, l := d.Where("/risk/overrides/0/mode"); f != "policy.yaml" || l != 10 {
		t.Errorf("where = %s:%d", f, l)
	}
	if f, l := d.Where("/risk/matrix/nothing/here"); f != "policy.yaml" || l == 0 {
		t.Errorf("nearest parent = %s:%d", f, l)
	}
	if f, _ := d.Where("/zzz"); f != "" {
		t.Errorf("unknown pointer = %q", f)
	}
}

func TestLoadRefusesDuplicateSectionsAndBadFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "version: 1\norg: acme\n")
	write(t, dir, "b.yaml", "org: other\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), `b.yaml:1: section "org" is already defined in a.yaml:2`) {
		t.Errorf("duplicate: %v", err)
	}
	for name, content := range map[string]string{"list": "- a\n- b\n", "syntax": "a: [\n", "scalar": "hello\n"} {
		d := t.TempDir()
		write(t, d, "p.yaml", content)
		if _, err := Load(d); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("an empty directory loaded")
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing directory loaded")
	}
	big := t.TempDir()
	write(t, big, "p.yaml", "version: 1\norg: acme\nnote: "+strings.Repeat("a", MaxDocumentBytes)+"\n")
	if _, err := Load(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big: %v", err)
	}
	many := t.TempDir()
	for i := 0; i <= maxFiles; i++ {
		write(t, many, "f"+strings.Repeat("a", i%7)+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('a'+(i/676)%26))+".yaml", "x"+strings.Repeat("y", i)+": 1\n")
	}
	if _, err := Load(many); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("many: %v", err)
	}
	empty := t.TempDir()
	write(t, empty, "e.yaml", "# only a comment\n")
	write(t, empty, "p.yaml", "version: 1\norg: acme\n")
	if _, err := Load(empty); err != nil {
		t.Errorf("a comment-only file: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "a.yaml"), filepath.Join(empty, "link.yaml")); err == nil {
		d, err := Load(empty)
		if err != nil || len(d.Files) != 2 {
			t.Errorf("a symlink was followed: %v", err)
		}
	}
}

func TestValidateReportsFileAndLine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "policy.yaml", `version: 1
org: Acme Corp
risk:
  matrix:
    stripe: { write: sometimes }
  overrides:
    - id: x
      tool: stripe.create_refund
      mode: approval
      bogus: true
unknown_section: 1
security:
  session_idle_minutes: 1
groups:
  g: { members: [not-an-email] }
`)
	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	diags, err := d.Validate()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, x := range diags {
		lines = append(lines, x.String())
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{"policy.yaml:2:", "policy.yaml:5:", "policy.yaml:10:", "policy.yaml:11:", "policy.yaml:13:"} {
		if !strings.Contains(all, want) {
			t.Errorf("no problem at %s:\n%s", want, all)
		}
	}
	for i := 1; i < len(diags); i++ {
		if diags[i-1].Line > diags[i].Line {
			t.Errorf("not ordered by line:\n%s", all)
		}
	}
	if (Diag{Message: "m"}).String() != "m" || (Diag{File: "f", Message: "m"}).String() != "f: m" {
		t.Error("Diag.String")
	}
}

func TestFormatIsCanonicalAndIdempotent(t *testing.T) {
	src := "# yaml-language-server: $schema=x\nversion: 1\norg:   acme   # the org\nrisk:\n      matrix:\n            stripe: {write: approval}\n"
	got, err := Format([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "# yaml-language-server") || !strings.Contains(string(got), "# the org") || !strings.Contains(string(got), "\n  matrix:\n    stripe:") {
		t.Errorf("formatted:\n%s", got)
	}
	again, err := Format(got)
	if err != nil || string(again) != string(got) {
		t.Errorf("not idempotent:\n%s\n---\n%s\n%v", got, again, err)
	}
	if out, err := Format([]byte("# nothing\n")); err != nil || string(out) != "# nothing\n" {
		t.Errorf("comment only: %q %v", out, err)
	}
	if _, err := Format([]byte("a: [")); err == nil {
		t.Error("invalid YAML formatted")
	}
}

func TestPlanRendering(t *testing.T) {
	p := Plan{ID: "pln_1", Classification: "mixed", URL: "https://gw/policy/plans/pln_1",
		Changes:   []Change{{Section: "risk", Op: "added", Path: "risk.matrix.stripe", File: "policy.yaml", Line: 5}, {Op: "removed", Path: "groups.old"}, {Op: "changed", Path: "security.strict_mode", File: "policy.yaml", Line: 9}},
		Impact:    []Impact{{Service: "stripe", Risk: "write", From: "off", To: "approval", Identities: 4}, {Service: "hubspot", Risk: "outward", From: "off", To: "allowed", Identities: 2, Loosening: true}},
		Loosening: []string{"hubspot outward off -> allowed"}, Lifecycle: []string{"3 keys would expire now"}, Packs: []string{"finance 2.3.0 -> 2.4.0"}}
	var sb strings.Builder
	p.RenderTable(&sb)
	for _, want := range []string{"Policy plan (mixed): 3 change(s)", "+ risk.matrix.stripe  policy.yaml:5", "- groups.old", "~ security.strict_mode", "Packs: finance", "hubspot outward: off -> allowed for 2 identities  [loosens]", "lifecycle: 3 keys", "Full plan: https://gw/policy/plans/pln_1"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, sb.String())
		}
	}
	if strings.Index(sb.String(), "hubspot") > strings.Index(sb.String(), "stripe write") {
		t.Error("loosening is not listed first")
	}
	md := p.RenderMarkdown()
	for _, want := range []string{"### Policy plan: mixed", "**Loosens the policy:**", "| + | `risk.matrix.stripe` | `policy.yaml:5` |", "hubspot | outward | off to allowed (loosens) | 2", "[Full plan](https://gw/policy/plans/pln_1)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if !p.HasChanges() || !p.IsLoosening() || (Plan{Classification: "tightening"}).IsLoosening() || (Plan{}).HasChanges() {
		t.Error("classification helpers")
	}
	var empty strings.Builder
	(Plan{}).RenderTable(&empty)
	if !strings.Contains(empty.String(), "No changes") || !strings.Contains((Plan{}).RenderMarkdown(), "No changes") {
		t.Error("empty plan")
	}
	if !strings.Contains((Plan{Changes: []Change{{Path: "a"}}}).RenderMarkdown(), "no classification") {
		t.Error("unclassified plan")
	}
}

func TestMarkdownIsCutToTheCommentLimit(t *testing.T) {
	p := Plan{Classification: "neutral", URL: "https://gw/p"}
	for i := 0; i < 3000; i++ {
		p.Changes = append(p.Changes, Change{Op: "added", Path: "risk.overrides." + strings.Repeat("x", 30), File: "policy.yaml", Line: i + 1})
	}
	md := p.RenderMarkdown()
	if len(md) > MarkdownLimit || !strings.Contains(md, "longer than a comment can hold") || !strings.Contains(md, "[Full plan](https://gw/p)") {
		t.Errorf("len %d, tail %q", len(md), md[len(md)-120:])
	}
	p.URL = ""
	if md := p.RenderMarkdown(); len(md) > MarkdownLimit || strings.Contains(md, "Full plan") {
		t.Errorf("no URL: len %d", len(md))
	}
	if got := mdEscape("a|b`c<d>\ne"); got != `a\|b\`+"`"+`c\<d\>e` {
		t.Errorf("mdEscape = %q", got)
	}
}

func TestCIPipelines(t *testing.T) {
	name, body, err := CI("github", "policy-dir")
	if err != nil || name != ".github/workflows/"+brand.Name+"-policy.yml" {
		t.Fatalf("github = %q, %v", name, err)
	}
	for _, want := range []string{"id-token: write", `paths: ["policy-dir/**"]`, brand.Name + " policy plan --dir policy-dir --format markdown", brand.Name + " policy apply --dir policy-dir --wait 600 --auto-approve", "vars." + strings.ToUpper(brand.Name) + "_URL"} {
		if !strings.Contains(body, want) {
			t.Errorf("workflow lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "{{CMD}}") || strings.Contains(body, "{{DIR}}") || strings.Contains(body, "{{ENV}}") {
		t.Error("placeholders left in the workflow")
	}
	name, body, err = CI("gitlab", "pol")
	if err != nil || name != ".gitlab/"+brand.Name+"-policy.yml" || !strings.Contains(body, "id_tokens:") || !strings.Contains(body, "aud: $"+strings.ToUpper(brand.Name)+"_URL") {
		t.Errorf("gitlab = %q, %v\n%s", name, err, body)
	}
	if _, _, err := CI("bitbucket", "x"); err == nil {
		t.Error("an unknown provider was accepted")
	}
}

func TestPublishedPipelinesAreWhatInitWrites(t *testing.T) {
	for provider, file := range map[string]string{"github": "github-policy.yml", "gitlab": "gitlab-policy.yml"} {
		_, want, err := CI(provider, brand.Name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join("..", "..", "docs", "ci", file))
		if err != nil || string(got) != want {
			t.Errorf("docs/ci/%s differs from what `policy init --ci %s` writes (%v)", file, provider, err)
		}
	}
}

func TestEmbeddedSchemaMatchesTheGatewaysWhenProvided(t *testing.T) {
	dir := os.Getenv("ZUANO_CONTRACT_FIXTURES")
	if dir == "" {
		t.Skip("no gateway fixtures")
	}
	theirs, err := os.ReadFile(filepath.Join(dir, "policy-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(theirs) != string(SchemaJSON()) {
		t.Error("the embedded policy schema differs from the gateway's")
	}
}
