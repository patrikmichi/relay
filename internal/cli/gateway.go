// Package cli implements the cobra command definitions.
package cli

import (
	"errors"
	"time"

	"github.com/patrikmichi/relay/internal/config"
)

const defaultOpTimeout = 30 * time.Second

const offlineGuidance = "no gateway configured. Run `relay config set-gateway <url>` then `relay login` to install from the catalog. Local `relay skill install <path>` and `relay skill migrate` work offline"

// OfflineGuidance exposes offlineGuidance to cmd/relay/main.go, which needs the exact same fail-closed message for one case this package's own commands can't produce it for: `relay --offline <unregistered-dynamic-command>`.
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
