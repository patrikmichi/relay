// Package main is the entrypoint for the relay CLI (gateway client).
// Auth subcommands: login, logout, whoami, authorize.
// Service subcommands: call, services, tokens, config, help-tools.
// Dynamic service sub-commands are built lazily from the gateway catalog.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/cli"
	"github.com/patrikmichi/relay/internal/config"
)

// version, commit, and date are injected at build time via -ldflags (see
// Makefile's -X main.version=... -X main.commit=... -X main.date=...). They
// default to "dev"/"none"/"unknown" for `go run`/`go build` without ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// buildVersionString formats the version/commit/date triple for the cobra
// root command's --version output. Extracted as a pure function so it can be
// unit-tested without invoking the CLI.
func buildVersionString(version, commit, date string) string {
	return fmt.Sprintf("%s (%s, %s)", version, commit, date)
}

// staticCommandNames are every static (non-dynamic) top-level command name
// plus the handful of nested sub-command names (tokens' list/revoke,
// config's set-gateway/get-gateway/show) that also need to skip dynamic
// registration. Anything NOT in this set is assumed to be a dynamic
// service/tool command name (or an attempt at one).
var staticCommandNames = map[string]bool{
	"login": true, "logout": true, "whoami": true, "authorize": true,
	"call": true, "services": true, "tokens": true, "config": true,
	"help-tools": true, "help": true, "completion": true, "sync": true,
	"publish": true, "skill": true, "agent": true, "mcp": true,
	"providers": true,
	"list":      true, "revoke": true, // tokens sub-commands
	"set-gateway": true, "get-gateway": true, "show": true, // config sub-commands
}

// shouldBuildDynamicCommands decides whether PersistentPreRunE should call
// BuildServiceCommands for the resolved command. False for every static
// command/sub-command (cmdName or topName matching staticCommandNames)
// regardless of offline state, and false whenever offline is true — this is
// the single decision point that keeps `--offline` from ever triggering a
// network dial to discover dynamic service/tool sub-commands. Extracted as
// a pure function (no *cobra.Command, no network) so it is unit-testable
// without constructing the full command tree.
func shouldBuildDynamicCommands(cmdName, topName string, offline bool) bool {
	if staticCommandNames[topName] || staticCommandNames[cmdName] {
		return false
	}
	return !offline
}

// offlineFlagPresent reports whether --offline (or --offline=<truthy>) is
// present in args. Used ONLY to decide whether to rewrite cobra's own
// "unknown command" error (see rewriteOfflineUnknownCommandErr) — the
// normal --offline enforcement path is cli.Offline() / shouldBuildDynamicCommands,
// driven from PersistentPreRunE, which never runs for an unresolved dynamic
// command (cobra's Find() rejects it before any Run hook fires — see
// newRootCmd's PersistentPreRunE doc comment on skipping
// BuildServiceCommands). A plain string scan (not full flag parsing) is
// enough here: --offline takes no argument other than an optional
// `=value`, so there is no "value is the next arg" ambiguity to resolve.
func offlineFlagPresent(args []string) bool {
	for _, a := range args {
		if a == "--offline" {
			return true
		}
		if v, ok := strings.CutPrefix(a, "--offline="); ok {
			return v != "false" && v != "0"
		}
	}
	return false
}

// rewriteOfflineUnknownCommandErr replaces cobra's generic "unknown
// command %q for %q" error with the same offline fail-closed guidance
// every other gateway-touching command surfaces, when the failed command
// looks like it was an unregistered DYNAMIC service/tool command skipped
// specifically because --offline suppressed BuildServiceCommands (see
// newRootCmd's PersistentPreRunE). Leaves every other error (including a
// genuinely unknown STATIC command typed without --offline) untouched, so
// this never masks an actual typo against the static command set.
func rewriteOfflineUnknownCommandErr(err error, args []string) error {
	if err == nil || !offlineFlagPresent(args) {
		return err
	}
	if !strings.Contains(err.Error(), "unknown command") {
		return err
	}
	return errors.New(cli.OfflineGuidance())
}

// newRootCmd builds the relay root cobra command with every static
// sub-command registered and the dynamic-registration PersistentPreRunE
// wired up. Extracted from main() so tests can Execute() the full command
// tree (e.g. to prove --offline never dials the gateway) without needing a
// built binary.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "relay",
		Short:   "Gateway CLI — unified access to integrated services",
		Version: buildVersionString(version, commit, date),
		// Don't print usage on error (cleaner output for auth errors).
		SilenceUsage: true,
	}

	// --offline is a cross-cutting root flag: every catalog-touching verb's
	// gateway resolution (internal/cli/gateway.go's
	// resolveGatewayURLOrFailClosed) checks cli.Offline() and fails closed
	// with the offline guidance regardless of what gateway URL would
	// otherwise resolve. Offline-only verbs (skill migrate, local skill
	// install) never consult it.
	root.PersistentFlags().Bool("offline", false, "Refuse any command that would dial the gateway, even if one is configured")

	// Register static commands.
	root.AddCommand(
		cli.LoginCmd(),
		cli.LogoutCmd(),
		cli.WhoamiCmd(),
		cli.AuthorizeCmd(),
		cli.CallCmd(),
		cli.ServicesCmd(),
		cli.TokensCmd(),
		cli.ConfigCmd(),
		cli.HelpToolsCmd(),
		cli.SyncCmd(),
		cli.PublishCmd(),
		cli.SkillCmd(),
		cli.AgentCmd(),
		cli.MCPCmd(),
		cli.ProvidersCmd(),
	)

	// Register dynamic service sub-commands lazily.
	// We use PersistentPreRunE so we only hit the gateway when actually executing
	// a command — not on every --help invocation.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		offline, err := cmd.Flags().GetBool("offline")
		if err != nil {
			return err
		}
		cli.SetOffline(offline)

		// Check the current command and its parent names.
		top := cmd
		for top.HasParent() && top.Parent() != root {
			top = top.Parent()
		}

		// --offline must block dynamic registration too: BuildServiceCommands
		// dials GET /api/integrations to discover services/tools, and doing
		// that here would defeat --offline for exactly the commands it exists
		// to guard (an unregistered dynamic service verb) — the caller would
		// see a network dial (or hang) instead of the fail-closed offline
		// guidance. With registration skipped, an unresolved dynamic command
		// falls through to cobra's own "unknown command" error — Execute's
		// caller below (rewriteOfflineUnknownCommandErr) rewrites that
		// specific error into the same offline guidance every other
		// gateway-touching command surfaces, so the caller sees one
		// consistent message instead of a bare "unknown command".
		if !shouldBuildDynamicCommands(cmd.Name(), top.Name(), cli.Offline()) {
			return nil
		}

		gatewayURL, err := config.GatewayURL()
		if err != nil {
			return nil // degrade gracefully
		}
		cli.BuildServiceCommands(root, gatewayURL)
		return nil
	}

	return root
}

func main() {
	root := newRootCmd()

	if err := root.Execute(); err != nil {
		err = rewriteOfflineUnknownCommandErr(err, os.Args[1:])
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
