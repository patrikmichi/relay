package evals

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestConformanceGraders(t *testing.T) {
	raw, err := os.ReadFile("testdata/conformance/graders.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Name      string
		Assertion Assertion
		Output    Output
		Pass      bool
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			if err := v.Assertion.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := v.Assertion.Check(v.Output); got.Pass != v.Pass {
				t.Errorf("pass = %v (%s), want %v", got.Pass, got.Reason, v.Pass)
			}
		})
	}
}

func TestSecretAndPersonalDataGraders(t *testing.T) {
	// Built at run time so no secret-shaped literal sits in the source.
	token := "gh" + "p_" + strings.Repeat("a1B2", 9)
	secret := Assertion{Type: "no_secret"}
	if secret.Check(Output{Text: "your token is " + token}).Pass {
		t.Error("a token passed no_secret")
	}
	if !secret.Check(Output{Text: "nothing here"}).Pass {
		t.Error("clean text failed no_secret")
	}
	if secret.Check(Output{ToolCalls: []ToolCall{{Name: "post", Args: map[string]any{"auth": token}}}}).Pass {
		t.Error("a token in tool arguments passed")
	}
	pii := Assertion{Type: "no_pii"}
	if pii.Check(Output{Text: "write to jane.doe@example.com"}).Pass {
		t.Error("an address passed no_pii")
	}
	if !(Assertion{Type: "no_pii", Classes: []string{"phone"}}).Check(Output{Text: "jane.doe@example.com"}).Pass {
		t.Error("the class filter was ignored")
	}
}

func TestAssertionValidation(t *testing.T) {
	one := 1.0
	for _, bad := range []Assertion{
		{Type: "nope"}, {Type: "regex", Pattern: "("}, {Type: "json_schema"}, {Type: "json_schema", Schema: map[string]any{"type": 5}},
		{Type: "json_path"}, {Type: "tool_sequence"}, {Type: "cost_lte"},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v validated", bad)
		}
	}
	if err := (Assertion{Type: "latency_lte", Max: &one}).Validate(); err != nil {
		t.Error(err)
	}
	if r := (Assertion{Type: "regex", Pattern: "("}).Check(Output{}); r.Pass {
		t.Error("a bad regex passed")
	}
	if r := (Assertion{Type: "json_schema", Schema: map[string]any{"type": "object"}}).Check(Output{Text: "plain"}); r.Pass {
		t.Error("text without JSON passed a schema")
	}
	if r := (Assertion{Type: "wat"}).Check(Output{}); r.Pass {
		t.Error("an unknown assertion passed")
	}
}

func TestExtractJSONAndPaths(t *testing.T) {
	for in, ok := range map[string]bool{`{"a":1}`: true, "x ```json\n[1]\n``` y": true, `say {"a":2} now`: true, "none": false, "```\nbad\n```": false} {
		if _, got := ExtractJSON(in); got != ok {
			t.Errorf("ExtractJSON(%q) = %v", in, got)
		}
	}
	doc, _ := ExtractJSON(`{"a":[{"b":"x"}]}`)
	if v, ok := ReadPath(doc, "a[0].b"); !ok || v != "x" {
		t.Errorf("ReadPath = %v %v", v, ok)
	}
	if _, ok := ReadPath(doc, "a.5"); ok {
		t.Error("an index past the end resolved")
	}
	if _, ok := ReadPath(doc, "a.b.c"); ok {
		t.Error("a path through an array by name resolved")
	}
	if !IsSubset(map[string]any{"x": 1.0}, map[string]any{"x": 1.0, "y": 2.0}) || IsSubset(map[string]any{"x": 2.0}, map[string]any{"x": 1.0}) {
		t.Error("IsSubset")
	}
}

func TestTriggerGrading(t *testing.T) {
	yes, no := true, false
	pdf, other := "pdf", "docx"
	cases := []struct {
		c    Case
		o    Output
		want bool
	}{
		{Case{ShouldTrigger: &yes}, Output{Choice: &pdf}, true},
		{Case{ShouldTrigger: &yes}, Output{}, false},
		{Case{ShouldTrigger: &no}, Output{Choice: &other}, true},
		{Case{ShouldTrigger: &no}, Output{Choice: &pdf}, false},
		{Case{ExpectedChoice: &other}, Output{Choice: &other}, true},
		{Case{ExpectedChoice: &other}, Output{}, false},
	}
	for i, tc := range cases {
		if got := CheckTrigger(tc.c, "pdf", tc.o); got.Pass != tc.want {
			t.Errorf("case %d: %+v", i, got)
		}
	}
}

func TestParseJudge(t *testing.T) {
	if s, why, ok := ParseJudge("```json\n{\"score\": 0.8, \"reason\": \"fine\"}\n```"); !ok || s != 0.8 || why != "fine" {
		t.Errorf("ParseJudge = %v %q %v", s, why, ok)
	}
	for _, bad := range []string{"", "no", `{"score": 3}`, `{"score": "x"}`, `[1]`} {
		if _, _, ok := ParseJudge(bad); ok {
			t.Errorf("ParseJudge(%q) succeeded", bad)
		}
	}
}
