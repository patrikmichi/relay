package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
	"github.com/patrikmichi/relay/internal/catalog"
	"github.com/patrikmichi/relay/internal/client"
)

// SkillInstallCmd returns the `relay skill install <path|catalog-id>` cobra
// command — loads a skill either from an arbitrary LOCAL directory (or a
// direct path to a SKILL.md file), or from the gateway CATALOG by resource
// id/slug, and projects it into one or more --to providers, printing a
// fidelity-loss report for each. Unless --dry-run, files are written and a
// manifest entry is recorded — the same engine as 'relay skill migrate'.
//
// If --to is omitted, installs to every provider detected as installed on
// this machine.
func SkillInstallCmd() *cobra.Command {
	var (
		toFlags    []string
		scopeFlag  string
		dryRun     bool
		strict     bool
		version    string
		channel    string
		gatewayURL string
	)

	cmd := &cobra.Command{
		Use:   "install <path|catalog-id>",
		Short: "Install a skill from a local directory or the gateway catalog",
		Long: `Load a skill either from an arbitrary local directory (or a direct path
to a SKILL.md file), or — when the argument is not an existing local path —
from the gateway catalog by resource id (res_…) or slug, and project it into
one or more --to providers, printing a fidelity-loss report for each. Unless
--dry-run, files are written and a manifest entry is recorded — the same
engine as 'relay skill migrate'.

Source disambiguation: if <path|catalog-id> exists on disk, it is treated as
a LOCAL path (agentport.LoadGenericSkill). Otherwise it is treated as a
gateway CATALOG id/slug (catalog.FetchSkill). An argument that merely LOOKS
like a path (contains a path separator) but doesn't exist on disk is
reported as a missing local path rather than guessed at as a catalog id.

A catalog-id install requires a reachable, authenticated gateway
(GATEWAY_URL/GATEWAY_API_KEY env vars, or a prior 'relay login') and fails
closed — no partial writes — on: no gateway configured / --offline, a
content-hash mismatch, a missing/failed scan verdict, or a malformed bundle.

If --to is omitted, installs to every provider detected as installed on
this machine.

Examples:
  relay skill install ./my-skill --to claude
  relay skill install ./my-skill/SKILL.md --to codex --to cursor
  relay skill install res_abc123 --to claude
  relay skill install pr-triage --version 1.4.2 --channel beta --to claude`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillInstall(cmd, args[0], skillInstallOpts{
				to:         toFlags,
				scope:      scopeFlag,
				dryRun:     dryRun,
				strict:     strict,
				version:    version,
				channel:    channel,
				gatewayURL: gatewayURL,
			})
		},
	}

	cmd.Flags().StringSliceVar(&toFlags, "to", nil, "Target provider(s); repeatable. Default: all detected providers")
	cmd.Flags().StringVar(&scopeFlag, "scope", "user", "Scope to write to: user or project")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the fidelity report; do not write files")
	cmd.Flags().BoolVar(&strict, "strict", false, "Abort if any field would be dropped")
	cmd.Flags().StringVar(&version, "version", "", "Catalog install only: exact semver (default: latest on --channel)")
	cmd.Flags().StringVar(&channel, "channel", "", "Catalog install only: stable or beta (default: gateway's default channel)")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL for a catalog install (default: $GATEWAY_URL, config, or built-in default)")

	return cmd
}

type skillInstallOpts struct {
	to         []string
	scope      string
	dryRun     bool
	strict     bool
	version    string
	channel    string
	gatewayURL string
}

func runSkillInstall(cmd *cobra.Command, arg string, opts skillInstallOpts) error {
	scope, err := parseScope(opts.scope)
	if err != nil {
		return err
	}

	isLocal, classifyErr := classifyInstallSource(arg)
	if classifyErr != nil {
		return classifyErr
	}

	var src *agentport.Skill
	fromLabel := "local"
	if isLocal {
		// s1 (Go review suggestion): a stray same-named local dir would
		// otherwise silently shadow a catalog id/slug with the same name —
		// note it so the user isn't surprised the catalog was never checked.
		fmt.Fprintf(cmd.ErrOrStderr(), "note: installing local path %q (not checking the catalog)\n", arg)
		src, err = agentport.LoadGenericSkill(arg)
		if err != nil {
			return fmt.Errorf("load %s: %w", arg, err)
		}
	} else {
		fromLabel = "gateway"
		src, err = fetchFromGateway(arg, opts)
		if err != nil {
			return err
		}
	}

	targets, err := resolveInstallTargets(opts.to)
	if err != nil {
		return err
	}

	return applyMigrationToTargets(cmd.OutOrStdout(), src, fromLabel, targets, scope, opts.dryRun, opts.strict)
}

// classifyInstallSource decides whether arg is a local filesystem path or a
// gateway catalog id/slug, per the documented rule: exists on disk -> local.
// Otherwise, if arg merely LOOKS like a path (contains a path separator),
// it's reported as a missing local path rather than guessed at as a catalog
// id — catalog ids/slugs (res_abc123, pr-triage) never contain a path
// separator, so this never produces a false "not found" for a genuine
// catalog id.
func classifyInstallSource(arg string) (isLocal bool, err error) {
	if _, statErr := os.Stat(arg); statErr == nil {
		return true, nil
	}
	if strings.ContainsRune(arg, '/') || (filepath.Separator != '/' && strings.ContainsRune(arg, filepath.Separator)) {
		return false, fmt.Errorf("path %q does not exist", arg)
	}
	return false, nil
}

// fetchFromGateway resolves an authenticated gateway client and fetches
// catalogID via catalog.FetchSkill, translating auth/offline/gateway-error
// failures into the fail-closed degradation guidance (design spec
// §6.1/§6.6). No files are ever written on any of these paths.
// resolveGatewayURLOrFailClosed itself checks the root --offline flag
// (cli.Offline()) and refuses with offlineGuidance before this ever reaches
// client.Resolve.
func fetchFromGateway(catalogID string, opts skillInstallOpts) (*agentport.Skill, error) {
	gURL, err := resolveGatewayURLOrFailClosed(opts.gatewayURL)
	if err != nil {
		return nil, err
	}

	c, err := client.Resolve(gURL)
	if err != nil {
		if errors.Is(err, client.ErrNotLoggedIn) {
			return nil, fmt.Errorf("%s", offlineGuidance)
		}
		return nil, err
	}

	skill, err := catalog.FetchSkill(c, catalogID, opts.version, opts.channel)
	if err != nil {
		return nil, translateGatewayFetchError(catalogID, err)
	}
	return skill, nil
}

// translateGatewayFetchError maps catalog.FetchSkill's sentinel errors to
// the user-facing guidance from the design spec's §6.6 degradation table.
func translateGatewayFetchError(catalogID string, err error) error {
	switch {
	case errors.Is(err, catalog.ErrNoAccess):
		return fmt.Errorf("you don't have access to %q — request access via the catalog UI or gateway.request_access: %w", catalogID, err)
	case errors.Is(err, catalog.ErrNotFound):
		return fmt.Errorf("catalog resource %q not found (or no downloadable version) — try `relay skill search` or an explicit --version: %w", catalogID, err)
	case errors.Is(err, catalog.ErrNotDownloadable):
		return fmt.Errorf("catalog resource %q's resolved version is not downloadable — try `relay skill search` or an explicit --version: %w", catalogID, err)
	case errors.Is(err, catalog.ErrUnauthorized):
		return fmt.Errorf("%w — %s", err, offlineGuidance)
	default:
		return err
	}
}

// resolveInstallTargets resolves the --to flag values to adapters, or — if
// --to was omitted — every detected provider. Unlike
// resolveMigrateTargets, there's no --from adapter to exclude: an install's
// source is an arbitrary local path or a gateway catalog id, never one of
// the 4 providers itself.
func resolveInstallTargets(to []string) ([]agentport.Adapter, error) {
	if len(to) == 0 {
		targets := agentport.DetectedProviders()
		if len(targets) == 0 {
			return nil, fmt.Errorf("no providers detected on this machine — specify --to explicitly")
		}
		return targets, nil
	}
	targets := make([]agentport.Adapter, 0, len(to))
	for _, t := range to {
		a, ok := agentport.AdapterByID(agentport.ProviderID(t))
		if !ok {
			return nil, fmt.Errorf("unknown --to provider %q", t)
		}
		targets = append(targets, a)
	}
	return targets, nil
}
