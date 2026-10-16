package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
)

func TestUpdateSyncPersistsRoutesAndDropsEmptySettings(t *testing.T) {
	home := withTempHome(t)
	if s, err := config.SyncConfig(); err != nil || s.Routes != nil {
		t.Fatalf("fresh SyncConfig = %+v, %v", s, err)
	}

	route := config.SyncRoute{From: "claude", To: []string{"codex"}, Kinds: []string{"skills"}}
	err := config.UpdateSync(func(s *config.SyncSettings) error {
		s.Routes = append(s.Routes, route)
		s.Defaults = map[string][]string{"rules": {"claude"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.SyncConfig()
	if err != nil || len(got.Routes) != 1 || !reflect.DeepEqual(got.Routes[0], route) || got.Defaults["rules"][0] != "claude" {
		t.Fatalf("SyncConfig = %+v, %v", got, err)
	}

	if err := config.UpdateSync(func(s *config.SyncSettings) error { *s = config.SyncSettings{}; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".config", "zuano", "config.json"))
	if strings.Contains(string(raw), "sync") {
		t.Errorf("empty sync settings were written: %s", raw)
	}
}

func TestUpdateSyncKeepsTheStoredSettingsWhenTheCallbackFails(t *testing.T) {
	withTempHome(t)
	_ = config.UpdateSync(func(s *config.SyncSettings) error {
		s.Defaults = map[string][]string{"hooks": {"codex"}}
		return nil
	})
	boom := errors.New("refused")
	err := config.UpdateSync(func(s *config.SyncSettings) error {
		s.Defaults["hooks"] = nil
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if s, _ := config.SyncConfig(); len(s.Defaults["hooks"]) != 1 {
		t.Errorf("a failed update changed the stored settings: %+v", s)
	}
}

func TestSyncSettingsReportACorruptConfig(t *testing.T) {
	home := withTempHome(t)
	writeConfigFile(t, home, "{broken")
	if _, err := config.SyncConfig(); err == nil {
		t.Error("SyncConfig read a corrupt config")
	}
	if err := config.UpdateSync(func(*config.SyncSettings) error { return nil }); err == nil {
		t.Error("UpdateSync wrote over a corrupt config")
	}
	if _, err := config.PinnedSigningKeys("https://gw.example.com"); err == nil {
		t.Error("PinnedSigningKeys read a corrupt config")
	}
}

func TestSigningKeysArePinnedPerGatewayOrigin(t *testing.T) {
	withTempHome(t)
	a := []config.SigningKey{{KID: "k1", Alg: "ed25519", PublicKey: "AAAA"}}
	b := []config.SigningKey{{KID: "k2", Alg: "ed25519", PublicKey: "BBBB"}}
	if err := config.PinSigningKeys("https://a.example.com", a); err != nil {
		t.Fatal(err)
	}
	if err := config.PinSigningKeys("https://b.example.com", b); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.PinnedSigningKeys("https://a.example.com"); !reflect.DeepEqual(got, a) {
		t.Errorf("a = %+v", got)
	}
	if got, _ := config.PinnedSigningKeys("https://c.example.com"); got != nil {
		t.Errorf("an unpinned origin has keys: %+v", got)
	}

	// Re-pinning replaces the set, which is how a rotation is accepted.
	if err := config.PinSigningKeys("https://a.example.com", b); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.PinnedSigningKeys("https://a.example.com"); !reflect.DeepEqual(got, b) {
		t.Errorf("after rotation = %+v", got)
	}
}

func TestSetGatewayURLKeepsTheIdentity(t *testing.T) {
	withTempHome(t)
	if err := config.SetEmail("me@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetGatewayURL("https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.GatewayURL != "https://gw.example.com" || cfg.Email != "me@example.com" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestIsLoopbackOrigin(t *testing.T) {
	for origin, want := range map[string]bool{
		"http://localhost:8080":      true,
		"http://127.0.0.1:3000":      true,
		"http://[::1]:3000":          true,
		"https://gw.example.com":     false,
		"http://192.168.1.10":        false,
		"http://localhost.evil.test": false,
		"://bad":                     false,
	} {
		if got := config.IsLoopbackOrigin(origin); got != want {
			t.Errorf("IsLoopbackOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestAPIKeyOriginPrefersTheEnvOverride(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("ZUANO_API_KEY_ORIGIN", "")
	if got, err := config.APIKeyOrigin(); err != nil || got != "" {
		t.Fatalf("no gateway: %q, %v", got, err)
	}

	if err := config.SetGatewayURL("https://Saved.Example.com:443"); err != nil {
		t.Fatal(err)
	}
	if got, err := config.APIKeyOrigin(); err != nil || got != "https://saved.example.com" {
		t.Errorf("saved gateway: %q, %v", got, err)
	}

	t.Setenv("ZUANO_API_KEY_ORIGIN", "http://localhost:9000")
	if got, err := config.APIKeyOrigin(); err != nil || got != "http://localhost:9000" {
		t.Errorf("env override: %q, %v", got, err)
	}

	t.Setenv("ZUANO_API_KEY_ORIGIN", "http://remote.example.com")
	if _, err := config.APIKeyOrigin(); err == nil {
		t.Error("a plain-HTTP remote origin was accepted")
	}

	t.Setenv("ZUANO_API_KEY_ORIGIN", "")
	writeConfigFile(t, home, "{broken")
	if _, err := config.APIKeyOrigin(); err == nil || !strings.Contains(err.Error(), "load config") {
		t.Errorf("corrupt config: %v", err)
	}
}

func TestDirIsThePrivateConfigDirectory(t *testing.T) {
	home := withTempHome(t)
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "zuano"); dir != want {
		t.Errorf("Dir = %q, want %q", dir, want)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Errorf("config dir not private: %v %v", info, err)
	}
}

func TestSettersFailWhenTheConfigFileIsUnwritable(t *testing.T) {
	home := withTempHome(t)
	// A directory where the file belongs makes the final rename fail.
	dir := filepath.Join(home, ".config", "zuano", "config.json")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SetGatewayURL("https://gw.example.com"); err == nil {
		t.Error("a config path blocked by a directory was written")
	}
	if err := config.PinSigningKeys("https://gw.example.com", nil); err == nil {
		t.Error("pinning succeeded against an unwritable config")
	}
}
