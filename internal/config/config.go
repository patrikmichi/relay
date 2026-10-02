// Package config provides persistent CLI configuration stored as JSON.
// Config file: ~/.config/relay/config.json (auto-migrated from ~/.config/gw)
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

// DefaultGatewayURL is the compiled-in fallback gateway URL, used when no GATEWAY_URL env var and no config file value are set.
var DefaultGatewayURL = ""

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

// GatewayURL returns the normalized, policy-checked gateway URL, or "" if none is set.
func GatewayURL() (string, error) {
	// Env var takes precedence over the config file.
	if v := os.Getenv("GATEWAY_URL"); v != "" {
		return NormalizeGatewayURL(v)
	}
	cfg, err := Load()
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	if cfg.GatewayURL != "" {
		return NormalizeGatewayURL(cfg.GatewayURL)
	}
	if DefaultGatewayURL == "" {
		return "", nil
	}
	return NormalizeGatewayURL(DefaultGatewayURL)
}

// NormalizeGatewayURL parses raw into a canonical gateway endpoint identity:
// scheme + host[:port], with no userinfo, query, fragment, or path component
// — path-prefix gateways are not supported, so a non-root path is rejected
// rather than silently dropped. Plain HTTP is permitted only for a literal
// loopback host (localhost/127.0.0.1/::1); every other host must use HTTPS.
func NormalizeGatewayURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("gateway URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse gateway URL %q: %w", raw, err)
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("gateway URL %q has no scheme (expected https://...)", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("gateway URL %q must not contain userinfo", raw)
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("gateway URL %q must not contain a query string", raw)
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("gateway URL %q must not contain a fragment", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("gateway URL %q has no host", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("gateway URL %q must not include a path — path-prefix gateways are not supported", raw)
	}
	switch u.Scheme {
	case "https":
		// ok
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("gateway URL %q uses plain HTTP for a non-loopback host — remote gateways require https", raw)
		}
	default:
		return "", fmt.Errorf("gateway URL %q has unsupported scheme %q — only https (or http for loopback) is allowed", raw, u.Scheme)
	}
	return u.Scheme + "://" + u.Host, nil
}

// isLoopbackHost reports whether host (a URL's Hostname(), no port/brackets)
// is a literal loopback address — the sole exception to the HTTPS-only
// transport policy, intended for local gateway development only.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// ResolveEmail resolves the current CLI identity used to look up the keychain OAuth session.
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
