package evals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodConfig = `
version: 1
prices:
  claude-sonnet-5.5: { input: 3, output: 15 }
suites:
  - id: pr-triage
    target: { kind: skill, path: skills/SKILL.md, catalog: skills/catalog.json }
    dataset: { path: evals/cases.jsonl }
    models: [anthropic/claude-sonnet-5.5, openai/gpt-6-mini]
    repeats: 3
    graders: { judge: { model: google/gemini-3-pro, threshold: 0.7 } }
    gates:
      min_pass_rate: 0.85
      no_regression_vs: main
      fail_on_inconclusive: false
`

func TestSchemaIsPinned(t *testing.T) {
	if SchemaHash() != PinnedSchemaHash() {
		t.Errorf("the embedded schema (%s) differs from the pinned hash (%s); copy it with `make gen-evals-schema`", SchemaHash(), PinnedSchemaHash())
	}
	if len(Schema()) == 0 {
		t.Error("no schema")
	}
}

func TestParseAcceptsTheDocumentedConfig(t *testing.T) {
	cfg, err := Parse([]byte(goodConfig))
	if err != nil {
		t.Fatal(err)
	}
	s, err := cfg.Suite("")
	if err != nil || s.ID != "pr-triage" || s.Repeats != 3 || *s.Gates.MinPassRate != 0.85 || *s.Graders.Judge.Threshold != 0.7 || cfg.Prices["claude-sonnet-5.5"].Output != 15 {
		t.Fatalf("suite = %+v, %v", s, err)
	}
	if cfg.Hash == "" || cfg.Resolve("a/b") != "a/b" {
		t.Errorf("hash %q resolve %q", cfg.Hash, cfg.Resolve("a/b"))
	}
	if _, err := cfg.Suite("nope"); err == nil {
		t.Error("an unknown suite resolved")
	}
}

func TestParseRejectsInvalidConfigs(t *testing.T) {
	for name, src := range map[string]string{
		"not yaml":         "a: [",
		"wrong version":    "version: 2\nsuites: [{id: a, target: {kind: prompt}, dataset: {path: x}}]",
		"no suites":        "version: 1\nsuites: []",
		"unknown key":      "version: 1\nsuites: [{id: a, target: {kind: prompt}, dataset: {path: x}, bogus: 1}]",
		"bad model":        "version: 1\nsuites: [{id: a, target: {kind: prompt}, dataset: {path: x}, models: [gpt]}]",
		"bad kind":         "version: 1\nsuites: [{id: a, target: {kind: toaster}, dataset: {path: x}}]",
		"dataset neither":  "version: 1\nsuites: [{id: a, target: {kind: prompt}, dataset: {}}]",
		"bad id":           "version: 1\nsuites: [{id: 'A b', target: {kind: prompt}, dataset: {path: x}}]",
		"duplicate suites": "version: 1\nsuites: [{id: a, target: {kind: prompt}, dataset: {path: x}}, {id: a, target: {kind: prompt}, dataset: {path: x}}]",
		"bad assertion":    "version: 1\nsuites: [{id: a, target: {kind: prompt}, dataset: {path: x}, graders: {assertions: [{type: regex, pattern: '('}]}}]",
		"too many repeats": "version: 1\nsuites: [{id: a, target: {kind: prompt}, dataset: {path: x}, repeats: 500}]",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := Parse([]byte("version: 1\nsuites: [{id: a, target: {kind: toaster}, dataset: {path: x}}]"))
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Errorf("the problem is not located: %v", err)
	}
}

func TestSuiteSelectionNeedsAnIDWhenAmbiguous(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nsuites: [{id: a, target: {kind: prompt}, dataset: {path: x}}, {id: b, target: {kind: prompt}, dataset: {path: x}}]"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Suite(""); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Errorf("Suite(\"\") = %v", err)
	}
}

func TestLoadResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "evals.yaml"), []byte(goodConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(dir, "evals.yaml"))
	if err != nil || cfg.Resolve("x.jsonl") != filepath.Join(dir, "x.jsonl") {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil || !strings.Contains(err.Error(), "eval init") {
		t.Errorf("missing file: %v", err)
	}
}

func TestParseModel(t *testing.T) {
	if m, err := ParseModel("anthropic/claude-x/1"); err != nil || m.Provider != "anthropic" || m.Model != "claude-x/1" || m.String() != "anthropic/claude-x/1" {
		t.Errorf("ParseModel = %+v, %v", m, err)
	}
	for _, bad := range []string{"gpt", "/x", "x/", "nobody/model"} {
		if _, err := ParseModel(bad); err == nil {
			t.Errorf("ParseModel(%q) succeeded", bad)
		}
	}
}
