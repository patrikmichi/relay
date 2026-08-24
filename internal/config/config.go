// Package config provides persistent CLI configuration stored as JSON.
// Config file: ~/.config/relay/config.json (auto-migrated from ~/.config/gw)
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultGatewayURL is the compiled-in fallback gateway URL, used when no
// GATEWAY_URL env var and no config file value are set. It is a var (not a
// const) so it can be overridden per-build via
// -ldflags "-X github.com/patrikmichi/relay/internal/config.DefaultGatewayURL=...".
// Public builds may set this to "" — every consumer of GatewayURL() (and,
// downstream, every catalog verb) must treat an empty result as "no gateway
// configured" and fail closed rather than silently dialing an empty/relative
// URL. See resolveGatewayURLOrFailClosed in internal/cli/gateway.go.
var DefaultGatewayURL = "https://gw.atlashub.dev"

// Config holds all persisted CLI settings.
type Config struct {
	GatewayURL string `json:"gatewayUrl,omitempty"`
	// Email is the identity of the last successful `relay login` (or
	// `relay login --device`), persisted so subsequent commands don't
	// require the RELAY_EMAIL environment variable to find the right
	// keychain entry. RELAY_EMAIL, when set, still takes precedence — see
	// ResolveEmail — so multi-account users can override without
	// re-running `relay login`.
	Email string `json:"email,omitempty"`
}

// configDir returns ~/.config/relay, creating it if necessary.
// A pre-rename ~/.config/gw dir is migrated in place on first use.
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "relay")
	if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		legacy := filepath.Join(home, ".config", "gw")
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			_ = os.Rename(legacy, dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create config dir %s: %w", dir, err)
	}
	return dir, nil
}

// configPath returns the path to the config file.
func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config file. Returns a zero-value Config if the file does not exist.
func Load() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config file %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the config to disk atomically (write temp file, rename).
func Save(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	raw = append(raw, '\n')

	// Write to a temp file alongside the target for atomic rename.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write config tmp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename config file: %w", err)
	}
	return nil
}

// GatewayURL returns the configured gateway URL, or the compiled-in default
// if not set. Resolution order: env var → config file → compiled default.
//
// When DefaultGatewayURL is empty (a public build with no baked-in gateway)
// and neither the env var nor the config file supply one, this returns ""
// with a nil error — it never fabricates a URL. Callers that dial the
// gateway must treat an empty result as "no gateway configured" and fail
// closed (see internal/cli/gateway.go's resolveGatewayURLOrFailClosed).
func GatewayURL() (string, error) {
	// Env var takes precedence over the config file.
	if v := os.Getenv("GATEWAY_URL"); v != "" {
		return v, nil
	}
	cfg, err := Load()
	if err != nil {
		return DefaultGatewayURL, nil // degrade gracefully
	}
	if cfg.GatewayURL != "" {
		return cfg.GatewayURL, nil
	}
	return DefaultGatewayURL, nil
}

// SetEmail persists the given email as the current logged-in identity,
// preserving any other existing config fields. Called by `relay login` /
// `relay login --device` on success so later commands don't require
// RELAY_EMAIL to be set.
func SetEmail(email string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.Email = email
	return Save(cfg)
}

// ResolveEmail resolves the current CLI identity used to look up the
// keychain OAuth session.
//
// Resolution order:
//  1. RELAY_EMAIL env var — explicit override (e.g. switching accounts
//     without re-running `relay login`, or scripting against a specific
//     identity). Always wins when set.
//  2. The email persisted by the most recent successful `relay login` /
//     `relay login --device` (see SetEmail).
//
// Returns an error (mentioning `relay login`) when neither is available.
func ResolveEmail() (string, error) {
	if v := os.Getenv("RELAY_EMAIL"); v != "" {
		return v, nil
	}
	cfg, err := Load()
	if err != nil {
		return "", err
	}
	if cfg.Email == "" {
		return "", errors.New("not logged in — run `relay login` first, or set RELAY_EMAIL")
	}
	return cfg.Email, nil
}
