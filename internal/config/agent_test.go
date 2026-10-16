package config_test

import (
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
)

func TestAgentMode(t *testing.T) {
	for value, want := range map[string]bool{
		"": false, "0": false, "false": false, "no": false, "agent": false,
		"1": true, "true": true, "TRUE": true, " yes ": true, "on": true,
	} {
		t.Setenv("RELAY_AGENT", value)
		if got := config.AgentMode(); got != want {
			t.Errorf("RELAY_AGENT=%q: AgentMode = %v, want %v", value, got, want)
		}
	}
}

func TestResolveAgentEmailOrder(t *testing.T) {
	withTempHome(t)
	t.Setenv("RELAY_EMAIL", "")

	if _, err := config.ResolveAgentEmail(); err == nil {
		t.Fatal("expected an error with no identity at all")
	}

	if err := config.SetEmail("human@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.ResolveAgentEmail(); got != "human@example.com" {
		t.Errorf("falls back to the human identity, got %q", got)
	}

	if err := config.SetAgentEmail("agent@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.ResolveAgentEmail(); got != "agent@example.com" {
		t.Errorf("agent identity wins over the human one, got %q", got)
	}
	if got, _ := config.ResolveEmail(); got != "human@example.com" {
		t.Errorf("an agent login changed the human identity to %q", got)
	}

	t.Setenv("RELAY_EMAIL", "override@example.com")
	if got, _ := config.ResolveAgentEmail(); got != "override@example.com" {
		t.Errorf("RELAY_EMAIL wins, got %q", got)
	}
}
