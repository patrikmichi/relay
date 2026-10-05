package agentport

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hashOf(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func TestInspectManifest_Reasons(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)

	agentDir := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentBody := []byte("agent")
	if err := os.WriteFile(filepath.Join(agentDir, "rev.md"), agentBody, 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(home, ".claude", "skills", "dir-skill")
	if err := os.MkdirAll(filepath.Join(skillDir, "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), agentBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".claude", "skills", "linked")); err != nil {
		t.Fatal(err)
	}

	paths := map[string]string{"SKILL.md": hashOf(agentBody)}
	m := Manifest{Entries: []ManifestEntry{
		{ID: "agent", Name: "rev", Kind: KindAgent, Provider: ProviderClaude, Scope: ScopeUser, TargetPaths: map[string]string{"rev.md": hashOf(agentBody)}},
		{ID: "unknown-provider", Name: "x", Kind: KindSkill, Provider: "nope", Scope: ScopeUser, TargetPaths: paths},
		{ID: "bad-kind", Name: "y", Kind: "widget", Provider: ProviderClaude, Scope: ScopeUser, TargetPaths: paths},
		{ID: "not-regular", Name: "dir-skill", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeUser, TargetPaths: paths},
		{ID: "symlinked", Name: "linked", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeUser, TargetPaths: paths},
		{ID: "elsewhere", Name: "p", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeProject, ProjectRoot: t.TempDir(), TargetPaths: paths},
		{ID: "here", Name: "q", Kind: KindSkill, Provider: ProviderClaude, Scope: ScopeProject, ProjectRoot: cwd, TargetPaths: paths},
	}}

	want := map[string]struct {
		ok, skipped bool
		reason      string
	}{
		"agent":            {ok: true},
		"unknown-provider": {reason: "provider unavailable"},
		"bad-kind":         {reason: "invalid installation manifest entry"},
		"not-regular":      {reason: "not a bounded regular file"},
		"symlinked":        {reason: "unsafe installation path"},
		"elsewhere":        {ok: true, skipped: true, reason: "cannot be checked from this directory"},
		"here":             {reason: "installed file missing"},
	}
	results := InspectManifest(m)
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(results), len(want), results)
	}
	for _, r := range results {
		w := want[r.ID]
		if r.OK != w.ok || r.Skipped != w.skipped || !strings.Contains(r.Reason, w.reason) {
			t.Errorf("%s: got ok=%v skipped=%v reason=%q, want ok=%v skipped=%v reason~%q", r.ID, r.OK, r.Skipped, r.Reason, w.ok, w.skipped, w.reason)
		}
	}
}
