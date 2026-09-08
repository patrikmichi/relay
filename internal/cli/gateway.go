// Package cli implements the cobra command definitions.
package cli

import (
	"errors"
	"time"

	"github.com/patrikmichi/relay/internal/config"
)

// defaultOpTimeout bounds a single quick gateway request issued directly by
// a CLI command (whoami, logout/tokens revoke, tokens list). Long-running
// operations (search, download, tool execution, sync/publish) define their
// own larger timeouts near their own call sites — this is deliberately not
// one blanket timeout for every gateway interaction, since those are a
// different operation class (R23).
const defaultOpTimeout = 30 * time.Second

// offlineGuidance is the fail-closed degradation message shown whenever a
// gateway-touching (catalog) command is attempted with no gateway reachable
// — no gateway configured, --offline, or an auth failure client.Resolve
// can't recover from. Mirrors the design spec's §6.1/§6.6 degradation
// table. Every catalog verb (skill install <catalog-id>, skill search,
// publish, sync, services, call, help-tools, tokens, whoami, login,
// logout, authorize) surfaces this single message so the guidance is
// consistent CLI-wide. Local-only verbs (skill install <path>, skill
// migrate) never reference it.
const offlineGuidance = "no gateway configured. Run `relay config set-gateway <url>` then `relay login` to install from the catalog. Local `relay skill install <path>` and `relay skill migrate` work offline."

// OfflineGuidance exposes offlineGuidance to cmd/relay/main.go, which needs
// the exact same fail-closed message for one case this package's own
// commands can't produce it for: `relay --offline <unregistered-dynamic-command>`.
// With dynamic service/tool sub-command registration skipped under
// --offline (main.go's PersistentPreRunE), cobra's own command resolution
// fails the request with a generic "unknown command" error before any of
// this package's RunE functions ever run — main.go rewrites that specific
// error into this same offline guidance so the caller sees one consistent
// message instead of two different-looking failures for the same root
// cause. See main.go's rewriteOfflineUnknownCommandErr.
func OfflineGuidance() string { return offlineGuidance }

// offlineFlag holds the process-wide state of the root `--offline`
// persistent flag. Set once via SetOffline from the cobra root's
// PersistentPreRunE after flags are parsed; read via Offline() by every
// catalog-touching verb's gateway resolution path
// (resolveGatewayURLOrFailClosed). A package-level var rather than a value
// threaded through every command's opts struct — --offline is a
// cross-cutting root flag, not something specific to any one subcommand.
var offlineFlag bool

// SetOffline sets the process-wide --offline state. Called once from
// cmd/relay/main.go's root PersistentPreRunE.
func SetOffline(v bool) { offlineFlag = v }

// Offline reports whether --offline was passed at the root. Exported so
// tests can simulate the flag without constructing the full root cobra
// command tree (a bare `SkillInstallCmd()` — or any other catalog verb's
// constructor — executed standalone never runs the root's
// PersistentPreRunE, so tests call SetOffline(true) directly instead).
func Offline() bool { return offlineFlag }

// resolveGatewayURLOrFailClosed resolves the gateway URL a catalog-touching
// command should dial: the explicit override (typically a --gateway-url
// flag value) if non-empty, otherwise config.GatewayURL()'s resolution order
// (env var -> config file -> config.DefaultGatewayURL). Every candidate is
// normalized and policy-checked (config.NormalizeGatewayURL). If --offline
// was passed at the root, config.GatewayURL() returned an error (e.g. a
// corrupt config file — that error is returned as-is, never papered over
// with a fallback host), or the resolved value is still empty, this returns
// an error wrapping offlineGuidance instead of silently dialing an
// empty/relative URL or a gateway the caller explicitly asked to skip.
// Every catalog verb must route its gateway URL resolution through this
// helper so the fail-closed behavior — and its exact user-facing message —
// is identical everywhere.
func resolveGatewayURLOrFailClosed(override string) (string, error) {
	if Offline() {
		return "", errors.New(offlineGuidance)
	}
	if override != "" {
		return config.NormalizeGatewayURL(override)
	}
	gURL, err := config.GatewayURL()
	if err != nil {
		return "", err
	}
	if gURL == "" {
		return "", errors.New(offlineGuidance)
	}
	return gURL, nil
}

// resolveGatewayIdentityLocal resolves and normalizes the gateway identity
// from an explicit override or local config only — it never consults
// --offline and never dials the network, because identifying which locally
// cached session to remove is a filesystem-only operation. Used by
// logout/tokens revoke's offline-safe local-delete path.
func resolveGatewayIdentityLocal(override string) (string, error) {
	if override != "" {
		return config.NormalizeGatewayURL(override)
	}
	return config.GatewayURL()
}
