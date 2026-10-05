package config_test

// GatewayURL() must not silently degrade to DefaultGatewayURL when the
// config file exists but fails to parse. Corrupt config must return an
// error, never select a fallback host.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
)

func TestGatewayURLFailsOnCorruptConfig(t *testing.T) {
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
		t.Fatalf("expected an error for corrupt config, got url=%q", url)
	}
}
