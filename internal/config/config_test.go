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

// TestNormalizeGatewayURL_RejectsUnsafeTransport: gateway
// URL validation must reject remote HTTP, userinfo, query, fragment,
// missing host, unsupported scheme, and a non-root path, while permitting
// HTTPS everywhere and plain HTTP only for a literal loopback host.
func TestNormalizeGatewayURL_RejectsUnsafeTransport(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"remote http", "http://gw.example.com"},
		{"userinfo", "https://user:pass@gw.example.com"},
		{"query string", "https://gw.example.com?token=abc"},
		{"fragment", "https://gw.example.com#frag"},
		{"missing host", "https:///path"},
		{"unsupported scheme", "ftp://gw.example.com"},
		{"no scheme", "gw.example.com"},
		{"non-root path", "https://gw.example.com/api"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.NormalizeGatewayURL(tc.raw); err == nil {
				t.Errorf("expected NormalizeGatewayURL(%q) to fail, got nil error", tc.raw)
			}
		})
	}
}

func TestNormalizeGatewayURL_PermitsHTTPSAndLoopbackHTTP(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://gw.example.com", "https://gw.example.com"},
		{"https://gw.example.com/", "https://gw.example.com"},
		{"https://gw.example.com:8443", "https://gw.example.com:8443"},
		{"http://127.0.0.1:4000", "http://127.0.0.1:4000"},
		{"http://localhost:4000", "http://localhost:4000"},
		{"http://[::1]:4000", "http://[::1]:4000"},
	}
	for _, tc := range cases {
		got, err := config.NormalizeGatewayURL(tc.raw)
		if err != nil {
			t.Errorf("NormalizeGatewayURL(%q): unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeGatewayURL(%q): got %q, want %q", tc.raw, got, tc.want)
		}
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
