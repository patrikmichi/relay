package config_test

// Regression test for R31 (2026-09-07 deep review): GatewayURL() silently
// degrades to DefaultGatewayURL when the config file exists but fails to
// parse, instead of propagating the error. Safe behavior (plan D1/P1.1):
// corrupt config must return an error, never select a fallback host.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
)

func TestR31_GatewayURLFailsOnCorruptConfig(t *testing.T) {
	dir := withTempHome(t)
	t.Setenv("GATEWAY_URL", "")

	cfgDir := filepath.Join(dir, ".config", "relay")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	url, err := config.GatewayURL()
	if err == nil {
		t.Fatalf("R31: expected an error for corrupt config, got url=%q", url)
	}
}
