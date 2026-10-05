package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/config"
)

func writeConfigFile(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".config", "relay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetEmail_PreservesOtherFields(t *testing.T) {
	withTempHome(t)
	if err := config.Save(config.Config{GatewayURL: "https://gw.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := config.SetEmail("user@example.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.GatewayURL != "https://gw.example.com" || got.Email != "user@example.com" {
		t.Errorf("unexpected config after SetEmail: %+v", got)
	}
}

func TestSetEmail_DoesNotOverwriteCorruptConfig(t *testing.T) {
	home := withTempHome(t)
	path := writeConfigFile(t, home, "{broken")

	if err := config.SetEmail("user@example.com"); err == nil {
		t.Fatal("expected SetEmail to fail on a corrupt config")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{broken" {
		t.Errorf("corrupt config was overwritten: %q", raw)
	}
}

func TestResolveEmail(t *testing.T) {
	t.Run("env override wins over config", func(t *testing.T) {
		withTempHome(t)
		if err := config.SetEmail("stored@example.com"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RELAY_EMAIL", "override@example.com")
		got, err := config.ResolveEmail()
		if err != nil || got != "override@example.com" {
			t.Fatalf("got %q, %v; want override@example.com", got, err)
		}
	})
	t.Run("falls back to persisted email", func(t *testing.T) {
		withTempHome(t)
		t.Setenv("RELAY_EMAIL", "")
		if err := config.SetEmail("stored@example.com"); err != nil {
			t.Fatal(err)
		}
		got, err := config.ResolveEmail()
		if err != nil || got != "stored@example.com" {
			t.Fatalf("got %q, %v; want stored@example.com", got, err)
		}
	})
	t.Run("not logged in", func(t *testing.T) {
		withTempHome(t)
		t.Setenv("RELAY_EMAIL", "")
		_, err := config.ResolveEmail()
		if err == nil || !strings.Contains(err.Error(), "relay login") {
			t.Fatalf("expected a not-logged-in error naming `relay login`, got %v", err)
		}
	})
	t.Run("corrupt config is an error", func(t *testing.T) {
		home := withTempHome(t)
		t.Setenv("RELAY_EMAIL", "")
		writeConfigFile(t, home, "[]")
		if _, err := config.ResolveEmail(); err == nil {
			t.Fatal("expected an error for a corrupt config")
		}
	})
}

func TestUnresolvableHomeIsAnError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("GATEWAY_URL", "")
	t.Setenv("RELAY_EMAIL", "")

	if _, err := config.Load(); err == nil {
		t.Error("Load: expected an error without a home directory")
	}
	if err := config.Save(config.Config{}); err == nil {
		t.Error("Save: expected an error without a home directory")
	}
	if _, err := config.GatewayURL(); err == nil {
		t.Error("GatewayURL: expected an error without a home directory")
	}
}

func TestConfigDirUncreatable(t *testing.T) {
	home := withTempHome(t)
	if err := os.WriteFile(filepath.Join(home, ".config"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "create config dir") {
		t.Fatalf("expected a create-config-dir error, got %v", err)
	}
}

func TestLoad_UnreadableConfigIsAnError(t *testing.T) {
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".config", "relay", "config.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "read config file") {
		t.Fatalf("expected a read error, got %v", err)
	}
}

func TestSave_TempFileUnwritable(t *testing.T) {
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".config", "relay", "config.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := config.Save(config.Config{GatewayURL: "https://gw.example.com"})
	if err == nil || !strings.Contains(err.Error(), "write config tmp file") {
		t.Fatalf("expected a temp-file write error, got %v", err)
	}
}

func TestSave_RenameFailureRemovesTempFile(t *testing.T) {
	home := withTempHome(t)
	target := filepath.Join(home, ".config", "relay", "config.json")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := config.Save(config.Config{GatewayURL: "https://gw.example.com"})
	if err == nil || !strings.Contains(err.Error(), "rename config file") {
		t.Fatalf("expected a rename error, got %v", err)
	}
	if _, statErr := os.Stat(target + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("temp file left behind after failed rename: %v", statErr)
	}
}

func TestSave_PrivatePermissions(t *testing.T) {
	home := withTempHome(t)
	if err := config.Save(config.Config{Email: "user@example.com"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".config", "relay")
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "config.json"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}

func TestLoad_ExistingDirIsNotReplacedByLegacyDir(t *testing.T) {
	home := withTempHome(t)
	writeConfigFile(t, home, `{"gatewayUrl":"https://current.example.com"}`)
	legacyDir := filepath.Join(home, ".config", "gw")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{"gatewayUrl":"https://legacy.example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayURL != "https://current.example.com" {
		t.Errorf("GatewayURL: got %q, want the current config", cfg.GatewayURL)
	}
	if _, err := os.Stat(legacyDir); err != nil {
		t.Errorf("legacy dir must stay untouched when the current dir exists: %v", err)
	}
}

func TestGatewayURL_EnvValueIsValidated(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_URL", "http://gw.example.com")
	if _, err := config.GatewayURL(); err == nil {
		t.Fatal("expected a remote plain-HTTP GATEWAY_URL to be rejected")
	}
}

func TestNormalizeGatewayURL_EmptyAndUnparsable(t *testing.T) {
	for _, raw := range []string{"", "http://[::1", "https://gw.example.com/%zz"} {
		if _, err := config.NormalizeGatewayURL(raw); err == nil {
			t.Errorf("NormalizeGatewayURL(%q): expected an error", raw)
		}
	}
}
