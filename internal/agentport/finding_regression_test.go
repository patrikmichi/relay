package agentport

// Regression tests derived from the 2026-09-07 deep review (R27) — see
// vault/reviews/relay/2026-09-07/relay-deep-codebase-review.md. The scanner
// previously retained the full matched secret in ScanFinding.Excerpt, and
// every scan output path (CLI print, JSON, logs) renders that field
// verbatim. Safe behavior: the original secret substring must never appear
// in a finding's Excerpt, for both Skill and Agent scanning.

import (
	"strings"
	"testing"
)

const syntheticGithubToken = "ghp_ABCDEFGHIJ0123456789abcdefghij01234567"

func TestR27_SkillScanRedactsSecretExcerpt(t *testing.T) {
	s := &Skill{
		Name:        "leaky-skill",
		Description: "a skill that accidentally hardcodes a credential",
		Body:        "Set your token: " + syntheticGithubToken + "\n",
	}
	result := Scan(s)

	var found bool
	for _, f := range result.Findings {
		if f.Pattern != "github-token" {
			continue
		}
		found = true
		if strings.Contains(f.Excerpt, syntheticGithubToken) {
			t.Fatalf("R27: scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding, got %#v", result.Findings)
	}
}

func TestR27_AgentScanRedactsSecretExcerpt(t *testing.T) {
	a := &Agent{
		Name:        "leaky-agent",
		Description: "an agent that accidentally hardcodes a credential",
		Body:        "Set your token: " + syntheticGithubToken + "\n",
	}
	result := AgentScan(a)

	var found bool
	for _, f := range result.Findings {
		if f.Pattern != "github-token" {
			continue
		}
		found = true
		if strings.Contains(f.Excerpt, syntheticGithubToken) {
			t.Fatalf("R27: agent scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding, got %#v", result.Findings)
	}
}

// TestR27_SkillScanResourceSecretRedacted covers the resource-file scan
// path (not just SKILL.md body) to ensure redaction applies uniformly.
func TestR27_SkillScanResourceSecretRedacted(t *testing.T) {
	s := &Skill{
		Name:        "leaky-skill-resource",
		Description: "a skill whose bundled resource hardcodes a credential",
		Body:        "See scripts/setup.sh\n",
		Resources: map[string][]byte{
			"scripts/setup.sh": []byte("export TOKEN=" + syntheticGithubToken + "\n"),
		},
	}
	result := Scan(s)

	var found bool
	for _, f := range result.Findings {
		if f.Pattern != "github-token" {
			continue
		}
		found = true
		if strings.Contains(f.Excerpt, syntheticGithubToken) {
			t.Fatalf("R27: resource scan finding excerpt contains the raw secret: %q", f.Excerpt)
		}
	}
	if !found {
		t.Fatalf("expected a github-token finding in resource scan, got %#v", result.Findings)
	}
}
