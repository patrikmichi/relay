package cli

import (
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport"
)

// TestProviderIDsCSV_MatchesLiveAdapterRegistry proves providerIDsCSV is
// derived from agentport.AllAdapters() at call time, not a hardcoded
// literal — the direct regression test for A1/A2 (stale provider-list
// strings across internal/cli).
func TestProviderIDsCSV_MatchesLiveAdapterRegistry(t *testing.T) {
	want := make([]string, 0)
	for _, a := range agentport.AllAdapters() {
		want = append(want, string(a.ID()))
	}
	got := providerIDsCSV()
	if got != strings.Join(want, ", ") {
		t.Errorf("providerIDsCSV() = %q, want %q (from AllAdapters())", got, strings.Join(want, ", "))
	}
	// The 7 currently-shipped skill providers must all be present.
	for _, id := range []string{"claude", "codex", "opencode", "cursor", "cline", "gemini-cli", "windsurf"} {
		if !strings.Contains(got, id) {
			t.Errorf("providerIDsCSV() = %q, missing provider %q", got, id)
		}
	}
}

// TestAgentProviderIDsCSV_MatchesLiveAgentAdapterRegistry is
// TestProviderIDsCSV_MatchesLiveAdapterRegistry's Agent-IR analogue.
func TestAgentProviderIDsCSV_MatchesLiveAgentAdapterRegistry(t *testing.T) {
	want := make([]string, 0)
	for _, a := range agentport.AllAgentAdapters() {
		want = append(want, string(a.ID()))
	}
	got := agentProviderIDsCSV()
	if got != strings.Join(want, ", ") {
		t.Errorf("agentProviderIDsCSV() = %q, want %q (from AllAgentAdapters())", got, strings.Join(want, ", "))
	}
	for _, id := range []string{"claude", "opencode"} {
		if !strings.Contains(got, id) {
			t.Errorf("agentProviderIDsCSV() = %q, missing provider %q", got, id)
		}
	}
}

func TestProviderIDsOxfordOr(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		want string
	}{
		{"empty", nil, ""},
		{"one", []string{"claude"}, "claude"},
		{"two", []string{"claude", "opencode"}, "claude or opencode"},
		{"three", []string{"claude", "codex", "opencode"}, "claude, codex, or opencode"},
		{"seven", []string{"a", "b", "c", "d", "e", "f", "g"}, "a, b, c, d, e, f, or g"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := providerIDsOxfordOr(c.ids); got != c.want {
				t.Errorf("providerIDsOxfordOr(%v) = %q, want %q", c.ids, got, c.want)
			}
		})
	}
}

// TestProvidersCmd_ListsSkillsAndAgentsColumns is the direct regression
// test for A9: `relay providers` must report both skill and agent support
// per provider, derived from the two adapter registries.
func TestProvidersCmd_ListsSkillsAndAgentsColumns(t *testing.T) {
	cmd := ProvidersCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE() = %v, want nil", err)
	}
	got := out.String()

	for _, id := range []string{"claude", "codex", "opencode", "cursor", "cline", "gemini-cli", "windsurf"} {
		if !strings.Contains(got, id) {
			t.Errorf("providers output missing skill provider %q:\n%s", id, got)
		}
	}
	if !strings.Contains(got, "skills: yes") {
		t.Errorf("providers output missing \"skills: yes\":\n%s", got)
	}
	if !strings.Contains(got, "agents: yes") || !strings.Contains(got, "agents: no") {
		t.Errorf("providers output missing an \"agents: yes\"/\"agents: no\" row:\n%s", got)
	}
}
