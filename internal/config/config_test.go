package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
)

// overrideHomeDir temporarily redirects os.UserHomeDir by setting $HOME.
// Returns a cleanup function.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestLoad_EmptyWhenNoFile(t *testing.T) {
	withTempHome(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if cfg.GatewayURL != "" {
		t.Errorf("expected empty GatewayURL, got %q", cfg.GatewayURL)
	}
}

func TestSaveAndLoad(t *testing.T) {
	withTempHome(t)

	want := config.Config{GatewayURL: "https://custom.example.com"}
	if err := config.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GatewayURL != want.GatewayURL {
		t.Errorf("GatewayURL: got %q, want %q", got.GatewayURL, want.GatewayURL)
	}
}

func TestSave_CreatesDir(t *testing.T) {
	home := withTempHome(t)

	cfg := config.Config{GatewayURL: "https://new.example.com"}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfgPath := filepath.Join(home, ".config", "relay", "config.json")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		t.Errorf("config file not created at %s", cfgPath)
	}
}

func TestGatewayURL_EmptyWithoutEnvOrConfig(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_URL", "")

	url, err := config.GatewayURL()
	if err != nil {
		t.Fatalf("GatewayURL: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty gateway URL when no env/config is set, got %q", url)
	}
}

func TestGatewayURL_EmptyWhenConfigUnreadable(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("GATEWAY_URL", "")
	dir := filepath.Join(home, ".config", "relay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	url, err := config.GatewayURL()
	if err != nil {
		t.Fatalf("GatewayURL: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty gateway URL for an unreadable config, got %q", url)
	}
}

func TestGatewayURL_EnvVarOverride(t *testing.T) {
	withTempHome(t)
	const override = "https://override.example.com"
	t.Setenv("GATEWAY_URL", override)

	url, err := config.GatewayURL()
	if err != nil {
		t.Fatalf("GatewayURL: %v", err)
	}
	if url != override {
		t.Errorf("env var override: got %q, want %q", url, override)
	}
}

func TestGatewayURL_FromConfig(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_URL", "") // ensure env var doesn't interfere

	const customURL = "https://my-gateway.example.com"
	cfg := config.Config{GatewayURL: customURL}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	url, err := config.GatewayURL()
	if err != nil {
		t.Fatalf("GatewayURL: %v", err)
	}
	if url != customURL {
		t.Errorf("from config: got %q, want %q", url, customURL)
	}
}

func TestSave_Atomic(t *testing.T) {
	withTempHome(t)

	// First write.
	if err := config.Save(config.Config{GatewayURL: "https://v1.example.com"}); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	// Second write overwrites.
	if err := config.Save(config.Config{GatewayURL: "https://v2.example.com"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load after second Save: %v", err)
	}
	if got.GatewayURL != "https://v2.example.com" {
		t.Errorf("expected v2 URL, got %q", got.GatewayURL)
	}
}

func TestLoad_MigratesLegacyGwDir(t *testing.T) {
	home := withTempHome(t)

	legacyDir := filepath.Join(home, ".config", "gw")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("create legacy dir: %v", err)
	}
	legacyCfg := []byte(`{"gatewayUrl":"https://legacy.example.com"}`)
	if err := os.WriteFile(filepath.Join(legacyDir, "config.json"), legacyCfg, 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://legacy.example.com" {
		t.Errorf("GatewayURL: got %q, want legacy value", cfg.GatewayURL)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "relay", "config.json")); err != nil {
		t.Errorf("migrated config not found at ~/.config/relay: %v", err)
	}
	if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
		t.Errorf("legacy ~/.config/gw dir still present after migration")
	}
}
