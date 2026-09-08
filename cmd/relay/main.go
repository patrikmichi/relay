// Package main is the entrypoint for the relay CLI (gateway client).
// Auth subcommands: login, logout, whoami, authorize.
// Service subcommands: call, services, tokens, config, help-tools.
// Dynamic service sub-commands are built lazily from the gateway catalog.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
//
// "recover" and "history" were missing here until this fix — both are real
// registered top-level commands (see newRootCmd), so a discovered service
// literally named "recover" or "history" would have silently collided with
// them instead of being caught. This hand-maintained list is only the
// pre-resolution fast-path check now; BuildServiceCommands' own
// commandNameCollides check against the live command tree is the
// authoritative guard (R13) and can't drift the same way.
var staticCommandNames = map[string]bool{
	"login": true, "logout": true, "whoami": true, "authorize": true,
	"call": true, "services": true, "tokens": true, "config": true,
	"help-tools": true, "help": true, "completion": true, "sync": true,
	"publish": true, "skill": true, "agent": true, "mcp": true,
	"providers": true, "recover": true, "history": true,
	"list": true, "revoke": true, // tokens sub-commands
	"set-gateway": true, "get-gateway": true, "show": true, // config sub-commands
}

// shouldBuildDynamicCommands decides whether discovery should run for a
// candidate command name. False for every static command/sub-command
// (cmdName or topName matching staticCommandNames) regardless of offline
// state, and false whenever offline is true — this is the single decision
// point that keeps `--offline` from ever triggering a network dial to
// discover dynamic service/tool sub-commands. Extracted as a pure function
// (no *cobra.Command, no network) so it is unit-testable without
// constructing the full command tree.
func shouldBuildDynamicCommands(cmdName, topName string, offline bool) bool {
	if staticCommandNames[topName] || staticCommandNames[cmdName] {
		return false
	}
	return !offline
}

// discoveryCandidate returns the first non-flag argument — the word Cobra
// would try to resolve as a top-level command — along with whether one was
// found at all. A bare `relay`, `relay --help`, or `relay --offline` all
// have no candidate, so callers correctly skip discovery for them without
// needing a separate "is this a help/offline invocation" check.
//
// A literal "--" ends flag scanning early per POSIX convention: the token
// immediately after it is positional even if it starts with a dash.
func discoveryCandidate(args []string) (string, bool) {
	for i, a := range args {
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, true
	}
	return "", false
}

// prepareDynamicCommands runs gateway discovery BEFORE Cobra resolves the
// command tree for args, registering it on root synchronously. This is the
// fix for R13: Cobra's own Find() rejects an unregistered command before
// any PersistentPreRunE hook can run, so a hook-based call to
// BuildServiceCommands (the previous approach) never actually fires for the
// one case it exists to serve — a not-yet-registered dynamic service/tool
// command. Calling it here, ahead of Execute, means the command already
// exists in the tree by the time Cobra tries to resolve it.
//
// Skips discovery entirely (no gateway dial) whenever: there's no candidate
// command word (bare `relay`, `--help`, `--offline` alone), the candidate is
// a static/reserved name, --offline is present, or no gateway is
// configured — the last of which degrades gracefully rather than erroring,
// matching BuildServiceCommands' own degradation posture. Returns a non-nil
// error only for a real collision (see BuildServiceCommands), which the
// caller must treat as fatal rather than silently continuing.
func prepareDynamicCommands(root *cobra.Command, args []string) error {
	candidate, ok := discoveryCandidate(args)
	if !ok {
		return nil
	}
	offline := offlineFlagPresent(args)
	if !shouldBuildDynamicCommands(candidate, candidate, offline) {
		return nil
	}
	gatewayURL, err := config.GatewayURL()
	if err != nil || gatewayURL == "" {
		return nil // no gateway configured / corrupt config — degrade gracefully, resolved again (with a real error) at RunE time
	}
	return cli.BuildServiceCommands(root, gatewayURL)
}

// offlineFlagPresent reports whether --offline (or --offline=<truthy>) is
// present in args. Used both by prepareDynamicCommands (to skip discovery
// before Cobra resolution) and to decide whether to rewrite cobra's own
// "unknown command" error (see rewriteOfflineUnknownCommandErr) for a
// dynamic command that was never registered because --offline suppressed
// discovery. A plain string scan (not full flag parsing) is enough here:
// --offline takes no argument other than an optional `=value`, so there is
// no "value is the next arg" ambiguity to resolve.
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
// specifically because --offline suppressed discovery (see
// prepareDynamicCommands). Leaves every other error (including a genuinely
// unknown STATIC command typed without --offline) untouched, so this never
// masks an actual typo against the static command set.
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
// sub-command registered. Extracted from main() so tests can Execute() the
// full command tree (e.g. to prove --offline never dials the gateway)
// without needing a built binary. Dynamic service/tool commands are NOT
// registered here — see prepareDynamicCommands, which main() calls before
// Execute (R13: a PersistentPreRunE hook never fires for a command Cobra's
// own Find() couldn't already resolve).
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
		cli.RecoverCmd(),
		cli.HistoryCmd(),
	)

	// PersistentPreRunE only needs to record --offline for every catalog
	// verb's resolveGatewayURLOrFailClosed to consult (internal/cli/gateway.go)
	// — dynamic command discovery happens earlier, before Execute (see
	// prepareDynamicCommands), not here.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		offline, err := cmd.Flags().GetBool("offline")
		if err != nil {
			return err
		}
		cli.SetOffline(offline)
		return nil
	}

	return root
}

func main() {
	root := newRootCmd()

	if err := prepareDynamicCommands(root, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// A SIGINT/SIGTERM cancels cmd.Context() everywhere it's threaded
	// through (login/device-code polling, logout, whoami, refresh, ...) —
	// commands that ignore it entirely still run to completion, but every
	// command wired to cmd.Context() gets clean, immediate cancellation
	// instead of relying solely on its own operation timeout (R23).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		err = rewriteOfflineUnknownCommandErr(err, os.Args[1:])
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
