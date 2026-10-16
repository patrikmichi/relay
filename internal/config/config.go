// Package config provides persistent CLI configuration stored as JSON.
// Config file: ~/.config/zuano/config.json
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/patrikmichi/zuano/internal/brand"
)

// Config holds all persisted CLI settings.
type Config struct {
	GatewayURL string `json:"gatewayUrl,omitempty"`
	// Email is the identity of the last successful `zuano login` (or
	// `zuano login --device`), persisted so subsequent commands don't
	// require the ZUANO_EMAIL environment variable to find the right
	// keychain entry. ZUANO_EMAIL, when set, still takes precedence — see
	// ResolveEmail — so multi-account users can override without
	// re-running `zuano login`.
	Email string `json:"email,omitempty"`
	// AgentEmail is the identity of the last `zuano login --agent`, kept apart
	// from Email so an agent login never changes which person's session the
	// unmarked commands use.
	AgentEmail string `json:"agentEmail,omitempty"`
	// SigningKeys are the gateway's content-signing public keys, pinned per
	// gateway origin the first time they are seen (see `zuano config
	// trust-key`).
	SigningKeys map[string][]SigningKey `json:"signingKeys,omitempty"`
	// Sync holds the saved routes `zuano sync` follows.
	Sync *SyncSettings `json:"sync,omitempty"`
}

// SyncRoute copies Kinds from one source to the agents in To. From names the
// provider whose skills and subagents are read.
type SyncRoute struct {
	From  string   `json:"from"`
	To    []string `json:"to"`
	Kinds []string `json:"kinds"`
}

// SyncSettings are the saved sync routes and, per artifact kind, the agents
// that receive it when no route covers that kind.
type SyncSettings struct {
	Routes   []SyncRoute         `json:"routes,omitempty"`
	Defaults map[string][]string `json:"defaults,omitempty"`
}

// SyncConfig returns the saved sync settings, or the zero value.
func SyncConfig() (SyncSettings, error) {
	cfg, err := Load()
	if err != nil || cfg.Sync == nil {
		return SyncSettings{}, err
	}
	return *cfg.Sync, nil
}

// UpdateSync applies fn to the saved sync settings under the config lock.
func UpdateSync(fn func(*SyncSettings) error) error {
	var ferr error
	err := modify(func(cfg *Config) {
		s := SyncSettings{}
		if cfg.Sync != nil {
			// Copy the containers: fn may edit them in place and then fail,
			// and the unchanged settings are still written back.
			s.Routes = slices.Clone(cfg.Sync.Routes)
			s.Defaults = maps.Clone(cfg.Sync.Defaults)
			for k, v := range s.Defaults {
				s.Defaults[k] = slices.Clone(v)
			}
		}
		if ferr = fn(&s); ferr != nil {
			return
		}
		if len(s.Routes) == 0 && len(s.Defaults) == 0 {
			cfg.Sync = nil
			return
		}
		cfg.Sync = &s
	}, true)
	if err != nil {
		return err
	}
	return ferr
}

// SigningKey is one ed25519 public key a gateway signs downloads with.
type SigningKey struct {
	KID       string `json:"kid"`
	Alg       string `json:"alg"`
	PublicKey string `json:"publicKey"`
}

// PinnedSigningKeys returns the keys pinned for a gateway origin.
func PinnedSigningKeys(origin string) ([]SigningKey, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	return cfg.SigningKeys[origin], nil
}

// PinSigningKeys replaces the keys pinned for a gateway origin.
func PinSigningKeys(origin string, keys []SigningKey) error {
	return modify(func(cfg *Config) {
		if cfg.SigningKeys == nil {
			cfg.SigningKeys = map[string][]SigningKey{}
		}
		cfg.SigningKeys[origin] = keys
	}, true)
}

// AgentMode reports whether ZUANO_AGENT selects the agent profile, the
// session marked as an autonomous agent's, for this process.
func AgentMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZUANO_AGENT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// configDir returns ~/.config/zuano, creating it if necessary.
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", brand.Name)
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

// Save replaces the config file. Prefer a setter: Save overwrites whatever
// another process wrote since the config was loaded.
func Save(cfg Config) error {
	return modify(func(c *Config) { *c = cfg }, false)
}

// modify applies fn to the stored config under the config lock, so two zuano
// processes changing different fields never lose each other's update. load
// reads the current file first; Save skips it because it replaces everything.
func modify(fn func(*Config), load bool) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	unlock, err := lockConfig(dir)
	if err != nil {
		return err
	}
	defer unlock()

	var cfg Config
	if load {
		if cfg, err = Load(); err != nil {
			return err
		}
	}
	fn(&cfg)
	return writeConfig(filepath.Join(dir, "config.json"), cfg)
}

// writeConfig writes the file through a synced temp file and a rename, so a
// crash leaves either the old or the new config, never a torn one.
func writeConfig(path string, cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), "config-*.tmp")
	if err != nil {
		return fmt.Errorf("create config tmp file: %w", err)
	}
	fail := func(step string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("%s: %w", step, err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return fail("write config tmp file", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync config tmp file", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close config tmp file", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename config file: %w", err)
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// GatewayURL returns the normalized, policy-checked gateway URL from the
// ZUANO_URL env var, else the config file. No gateway URL is compiled into
// the binary: with neither set it returns "" and callers must fail closed (see
// internal/cli/gateway.go's resolveGatewayURLOrFailClosed).
//
// A corrupt or unreadable config file is an error rather than "no gateway",
// so zuano never silently dials a different host than the configured one.
func GatewayURL() (string, error) {
	if v := os.Getenv("ZUANO_URL"); v != "" {
		return NormalizeGatewayURL(v)
	}
	cfg, err := Load()
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	if cfg.GatewayURL != "" {
		return NormalizeGatewayURL(cfg.GatewayURL)
	}
	return "", nil
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
	// The origin is a session and API-key binding key: two spellings of one
	// host must not become two identities.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

// IsLoopbackOrigin reports whether a normalized gateway origin is a local
// development gateway.
func IsLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && isLoopbackHost(u.Hostname())
}

// APIKeyOrigin is the one gateway ZUANO_API_KEY may be sent to:
// ZUANO_API_KEY_ORIGIN when set, else the gateway saved in the config file.
// It is "" when neither exists.
func APIKeyOrigin() (string, error) {
	raw := os.Getenv("ZUANO_API_KEY_ORIGIN")
	if raw == "" {
		cfg, err := Load()
		if err != nil {
			return "", fmt.Errorf("load config: %w", err)
		}
		raw = cfg.GatewayURL
	}
	if raw == "" {
		return "", nil
	}
	return NormalizeGatewayURL(raw)
}

// isLoopbackHost reports whether host (a URL's Hostname(), no port/brackets)
// is a literal loopback address — the sole exception to the HTTPS-only
// transport policy, intended for local gateway development only.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SetEmail persists the given email as the current logged-in identity,
// preserving any other existing config fields. Called by `zuano login` /
// `zuano login --device` on success so later commands don't require
// ZUANO_EMAIL to be set.
func SetEmail(email string) error {
	return modify(func(cfg *Config) { cfg.Email = email }, true)
}

// SetGatewayURL persists the (already normalized) gateway URL.
func SetGatewayURL(url string) error {
	return modify(func(cfg *Config) { cfg.GatewayURL = url }, true)
}

// SetAgentEmail persists the identity of the agent profile, leaving Email
// alone.
func SetAgentEmail(email string) error {
	return modify(func(cfg *Config) { cfg.AgentEmail = email }, true)
}

// ResolveAgentEmail resolves the identity of the agent profile: ZUANO_EMAIL,
// then the last `zuano login --agent`, then the human identity, since an
// agent normally acts for the person who is signed in.
func ResolveAgentEmail() (string, error) {
	if v := os.Getenv("ZUANO_EMAIL"); v != "" {
		return v, nil
	}
	cfg, err := Load()
	if err != nil {
		return "", err
	}
	if cfg.AgentEmail != "" {
		return cfg.AgentEmail, nil
	}
	if cfg.Email != "" {
		return cfg.Email, nil
	}
	return "", errors.New("no agent session — run `" + brand.Cmd("login --agent") + "`, or set ZUANO_EMAIL")
}

// ResolveEmail resolves the current CLI identity used to look up the
// keychain OAuth session.
//
// Resolution order:
//  1. ZUANO_EMAIL env var — explicit override (e.g. switching accounts
//     without re-running `zuano login`, or scripting against a specific
//     identity). Always wins when set.
//  2. The email persisted by the most recent successful `zuano login` /
//     `zuano login --device` (see SetEmail).
//
// Returns an error (mentioning `zuano login`) when neither is available.
func ResolveEmail() (string, error) {
	if v := os.Getenv("ZUANO_EMAIL"); v != "" {
		return v, nil
	}
	cfg, err := Load()
	if err != nil {
		return "", err
	}
	if cfg.Email == "" {
		return "", errors.New("not logged in — run `" + brand.Cmd("login") + "` first, or set ZUANO_EMAIL")
	}
	return cfg.Email, nil
}

// Dir returns the zuano config directory (~/.config/zuano), creating it if necessary.
func Dir() (string, error) { return configDir() }
