package agentport

import (
	"path/filepath"
	"testing"
)

func TestScan_FlagsDangerousScript(t *testing.T) {
	a := NewClaudeAdapter()
	s, err := a.Load(filepath.Join("testdata", "dangerous", "risky-skill"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	result := Scan(s)
	if len(result.Findings) == 0 {
		t.Fatalf("expected findings for a skill with curl|bash and rm -rf, got none")
	}

	var sawCurlPipe, sawRmRf bool
	for _, f := range result.Findings {
		if f.Pattern == "curl-pipe-shell" {
			sawCurlPipe = true
			if f.Severity != "high" {
				t.Errorf("curl-pipe-shell severity = %s, want high", f.Severity)
			}
		}
		if f.Pattern == "rm-rf" {
			sawRmRf = true
		}
	}
	if !sawCurlPipe {
		t.Errorf("expected a curl-pipe-shell finding, got %#v", result.Findings)
	}
	if !sawRmRf {
		t.Errorf("expected a rm-rf finding, got %#v", result.Findings)
	}

	if result.Score >= 50 {
		t.Errorf("Score = %d, want a low score for a skill with dangerous findings", result.Score)
	}
}

func TestScan_CleanSkillPasses(t *testing.T) {
	a := NewClaudeAdapter()
	s, err := a.Load(filepath.Join("testdata", "clean", "clean-skill"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	result := Scan(s)
	if len(result.Findings) != 0 {
		t.Fatalf("expected no findings for a clean skill, got %#v", result.Findings)
	}
	if result.Score < 70 {
		t.Errorf("Score = %d, want a high score for a well-documented clean skill", result.Score)
	}
}

func TestScan_FlagsHardcodedSecret(t *testing.T) {
	s := &Skill{
		Name:        "leaky-skill",
		Description: "a skill that accidentally hardcodes a credential",
		Body:        "Set your key: sk-abcdefghijklmnopqrstuvwxyz012345\n",
	}
	result := Scan(s)

	var sawSecret bool
	for _, f := range result.Findings {
		if f.Pattern == "openai-api-key" {
			sawSecret = true
		}
	}
	if !sawSecret {
		t.Fatalf("expected an openai-api-key finding, got %#v", result.Findings)
	}
}

func TestScan_IgnoresBinaryResources(t *testing.T) {
	s := &Skill{
		Name:        "binary-asset-skill",
		Description: "a skill bundling a binary asset that happens to contain NUL bytes",
		Body:        "See assets/logo.png\n",
		Resources: map[string][]byte{
			"assets/logo.png": {0x89, 0x50, 0x4e, 0x47, 0x00, 0x0d, 0x0a},
		},
	}
	result := Scan(s)
	if len(result.Findings) != 0 {
		t.Fatalf("expected binary resources to be skipped, got %#v", result.Findings)
	}
}
