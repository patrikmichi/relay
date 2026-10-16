package evals

import (
	"strings"
	"testing"
)

const promptfooConfig = `
prompts:
  - "Answer briefly: {{question}}"
defaultTest:
  assert:
    - type: not-icontains
      value: "as an ai"
tests:
  - description: capital
    vars: { question: "Capital of France?" }
    assert:
      - { type: icontains, value: paris }
      - { type: regex, value: "(?=Par)is" }
      - { type: contains-all, value: [Paris, France] }
      - { type: contains-any, value: [Paris, "Lutetia"] }
      - { type: is-json }
      - { type: llm-rubric, value: "Answers in one sentence" }
      - { type: latency, threshold: 2000 }
      - { type: javascript, value: "output.length < 10" }
      - { type: not-equals, value: "x" }
  - vars: { question: "2+2?" }
    assert:
      - { type: python, value: "x" }
  - "tests/other.yaml"
  - vars: {}
    assert: [{ type: contains, value: a }]
`

func TestImportPromptfooMapsWhatItCanAndListsTheRest(t *testing.T) {
	cases, rep, err := ImportPromptfoo([]byte(promptfooConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || rep.Cases != 2 {
		t.Fatalf("cases = %+v", cases)
	}
	c := cases[0]
	if c.Name != "capital" || c.Input.Text != "Capital of France?" || c.Input.Variables["question"] != "Capital of France?" || c.Expected.Rubric != "Answers in one sentence" {
		t.Errorf("case = %+v", c)
	}
	var types []string
	for _, a := range c.Expected.Assertions {
		types = append(types, a.Type)
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	// icontains, contains x2, contains-any as a regex, is-json, latency, and the default not-icontains.
	if got := strings.Join(types, ","); got != "icontains,contains,contains,regex,json_schema,latency_lte,not_icontains" {
		t.Errorf("assertion types = %s", got)
	}
	reasons := map[string]string{}
	for _, u := range rep.Unmapped {
		reasons[u.Type] = u.Reason
	}
	for _, want := range []string{"regex", "javascript", "not-equals", "python", "test"} {
		if _, ok := reasons[want]; !ok {
			t.Errorf("%s is not listed as unmapped: %v", want, rep.Unmapped)
		}
	}
	if !strings.Contains(reasons["regex"], "RE2") || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "{{question}}") {
		t.Errorf("report = %+v", rep)
	}
}

func TestImportPromptfooErrors(t *testing.T) {
	for name, src := range map[string]string{"yaml": "a: [", "no tests": "prompts: [x]", "none convertible": "tests:\n  - vars: {q: a}\n    assert: [{type: python, value: x}]"} {
		if _, _, err := ImportPromptfoo([]byte(src)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestImportOpenAIWithCriteria(t *testing.T) {
	items := `{"input":[{"role":"system","content":"s"},{"role":"user","content":"2+2?"}],"ideal":"4"}
{"item":{"question":"capital?","expected":"Paris"}}
{"input":"no ideal"}

`
	criteria := `{"testing_criteria":[
	  {"type":"string_check","operation":"eq","input":"{{sample.output_text}}","reference":"{{item.ideal}}"},
	  {"type":"string_check","operation":"ilike","reference":"{{item.expected}}"},
	  {"type":"string_check","operation":"ne","reference":"x"},
	  {"type":"text_similarity","reference":"y"}]}`
	cases, rep, err := ImportOpenAI([]byte(items), []byte(criteria))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[0].Input.Messages[1].Content != "2+2?" || cases[0].Expected.Assertions[0].Type != "equals" || cases[0].Expected.Assertions[0].Value != "4" {
		t.Fatalf("cases = %+v", cases)
	}
	if cases[1].Input.Text != "capital?" || cases[1].Expected.Assertions[0].Type != "icontains" || cases[1].Expected.Assertions[0].Value != "Paris" {
		t.Errorf("second = %+v", cases[1])
	}
	found := map[string]bool{}
	for _, u := range rep.Unmapped {
		found[u.Type+":"+u.Case] = true
	}
	if !found["text_similarity:item 1"] || !found["string_check:item 1"] {
		t.Errorf("unmapped = %+v", rep.Unmapped)
	}
}

func TestImportOpenAIWithoutCriteriaUsesAReferenceRubric(t *testing.T) {
	cases, rep, err := ImportOpenAI([]byte(`{"input":"q","ideal":["a","b"]}`), nil)
	if err != nil || len(cases) != 1 || cases[0].Expected.Reference != "a" || cases[0].Expected.Rubric == "" || len(rep.Notes) != 1 {
		t.Fatalf("cases %+v rep %+v err %v", cases, rep, err)
	}
	for name, args := range map[string][2]string{"bad line": {"{", ""}, "empty": {"", ""}, "bad criteria": {`{"input":"q"}`, "3"}} {
		if _, _, err := ImportOpenAI([]byte(args[0]), []byte(args[1])); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestImportSkillCreator(t *testing.T) {
	src := `{"skill_name":"pdf","evals":[
	  {"id":1,"prompt":"Merge a.pdf and b.pdf","expected_output":"one merged file","expectations":["Calls the merge tool","Names the output merged.pdf"]},
	  {"id":2,"prompt":"Split it","expected_output":"two files","files":["in.pdf"]},
	  {"id":3,"prompt":"","expectations":["x"]}]}`
	cases, rep, err := ImportSkillCreator([]byte(src))
	if err != nil || len(cases) != 2 {
		t.Fatalf("cases %+v err %v", cases, err)
	}
	if cases[0].Key != "eval-1" || !strings.Contains(cases[0].Expected.Rubric, "Calls the merge tool\nNames") || cases[0].Expected.Reference != "one merged file" {
		t.Errorf("first = %+v", cases[0])
	}
	if cases[1].Expected.Rubric == "" {
		t.Errorf("an expected output alone should become a rubric: %+v", cases[1])
	}
	if len(rep.Unmapped) != 2 || rep.Notes[0] != "skill: pdf" {
		t.Errorf("report = %+v", rep)
	}
	for _, bad := range []string{"[", `{"evals":[]}`, `{"evals":[{"id":1}]}`} {
		if _, _, err := ImportSkillCreator([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestImportedCasesAreRunnable(t *testing.T) {
	cases, _, err := ImportPromptfoo([]byte("tests:\n  - vars: {question: hi}\n    assert: [{type: icontains, value: HELLO}]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if err := c.Validate(); err != nil {
			t.Error(err)
		}
	}
	h := stubHarness{fn: func(Case, Cell) Output { return Output{Text: "Hello there"} }}
	run := execute(t, cases, h, cellsOf("anthropic/m1"), 1, nil)
	if run.Results[0].Outcome != OutcomePass {
		t.Errorf("result = %+v", run.Results[0])
	}
}

func TestImportClaudePlugin(t *testing.T) {
	dir := t.TempDir()
	w := func(rel, body string) { writeFile(t, dir, rel, body) }
	w("commit-msg/prompt.md", "---\nname: commit message\ntags: [git]\nallowed_tools: [Read, Skill]\n---\n\nWrite a commit message for renaming getUser to fetchUser.\n")
	w("commit-msg/graders/criteria.md", "---\ntype: llm\nweight: 2\n---\nPASS if the message names fetchUser.\nFAIL otherwise.\n")
	w("commit-msg/graders/format.md", "---\ntype: regex\npattern: '^(feat|fix|refactor)'\nflags: i\n---\n")
	w("commit-msg/graders/skill-fired.md", "---\ntype: tool_used\ntool: Skill\ninput_match: 'commit'\n---\n")
	w("commit-msg/graders/no-bash.md", "---\ntype: tool_used\ntool: Bash\nmin: 0\nmax: 0\n---\n")
	w("commit-msg/graders/order.md", "---\ntype: tool_order\nbefore: Read\nafter: Write\n---\n")
	w("commit-msg/graders/file.md", "---\ntype: file_exists\npath: out.md\n---\n")
	w("commit-msg/graders/trace.md", "---\ntype: regex\npattern: x\ntarget: trace\n---\n")
	w("commit-msg/graders/absent.md", "---\ntype: regex\npattern: x\nmatch: not_contains\n---\n")
	w("commit-msg/graders/js.md", "---\ntype: regex\npattern: '(?=a)b'\n---\n")
	w("group/nested/prompt.md", "Plain prompt without front matter\n")
	w("group/nested/graders/c.md", "---\ntype: llm\nfocus: trace\n---\nbody\n")
	w("scaffolded/prompt.md", "Needs a repo\n")
	w("scaffolded/case.yaml", "schema_version: '1.1'\nname: scaffolded\ncontext:\n  scaffold_script: setup.sh\n")
	w("scaffolded/graders/r.md", "---\ntype: regex\npattern: ok\n---\n")
	w("yaml-only/case.yaml", "schema_version: '1.1'\nname: yaml-only\n")
	w("results/2026/prompt.md", "should be skipped\n")
	w("mocks/srv/prompt.md", "skipped too\n")

	cases, rep, err := ImportClaudePlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || rep.Cases != 2 {
		t.Fatalf("cases = %d: %+v", len(cases), rep)
	}
	var c, s *Case
	for i := range cases {
		switch cases[i].Name {
		case "commit message":
			c = &cases[i]
		case "scaffolded":
			s = &cases[i]
		}
	}
	if c == nil || s == nil {
		t.Fatalf("cases = %+v", cases)
	}
	var types []string
	for _, a := range c.Expected.Assertions {
		types = append(types, a.Type)
	}
	if strings.Join(types, ",") != "regex,no_injection_compliance,tool_sequence" || c.Expected.Assertions[0].Pattern != "(?i)^(feat|fix|refactor)" || c.Expected.Assertions[1].Forbidden[0] != "Bash" {
		t.Errorf("assertions = %+v", c.Expected.Assertions)
	}
	if !strings.Contains(c.Expected.Rubric, "PASS if the message names fetchUser.") || c.Key != "commit-message" || c.Tags[0] != "git" || c.Input.Text != "Write a commit message for renaming getUser to fetchUser." {
		t.Errorf("case = %+v", c)
	}
	reasons := map[string]string{}
	for _, u := range rep.Unmapped {
		reasons[u.Case+"|"+u.Type] = u.Reason
	}
	for _, want := range []string{"commit message|tool_used (skill-fired.md)", "commit message|file_exists (file.md)", "commit message|regex (trace.md)", "commit message|regex (absent.md)", "commit message|regex (js.md)", "nested|llm (c.md)", "scaffolded|case.yaml", "yaml-only|case.yaml", "nested|case"} {
		if _, ok := reasons[want]; !ok {
			t.Errorf("%q is not listed as unmapped: %v", want, reasons)
		}
	}
	notes := strings.Join(rep.Notes, "\n")
	if !strings.Contains(notes, "weights are not carried over") || !strings.Contains(notes, "allowed_tools are not carried over") {
		t.Errorf("notes = %q", notes)
	}
	for _, x := range cases {
		if err := x.Validate(); err != nil {
			t.Error(err)
		}
	}
	if _, _, err := ImportClaudePlugin(t.TempDir()); err == nil {
		t.Error("an empty directory")
	}
	if _, _, err := ImportClaudePlugin(dir + "/missing"); err == nil {
		t.Error("a missing directory")
	}
}
