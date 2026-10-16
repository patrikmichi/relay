package cli

import (
	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/config"
)

// maxResponseBytes is the per-response read cap (10 MiB).
// Prevents a malicious/compromised gateway from OOM-ing the client.
const maxResponseBytes = 10 << 20

// SyncCmd returns the `zuano sync` cobra command.
func SyncCmd() *cobra.Command {
	var (
		o       agentSyncOpts
		matrix  bool
		noPacks bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync MCP servers, hooks, rules, commands, skills and subagents across coding agents",
		Args:  cobra.NoArgs,
		Long: `Write what zuano manages into the config of every coding agent: Claude Code,
Codex, Cursor, OpenCode, Gemini CLI, Windsurf, Cline and GitHub Copilot.

Sources
  MCP servers   the gateway (when configured) plus servers added with
                'zuano mcp install'
  hooks, rules, commands
                ~/.config/zuano/sync/{hooks.json,rules/*.md,commands/*.md}
                for the user scope and <project>/.zuano/ for the project scope
  skills, subagents
                everything installed for --from (default claude), copied to
                the other agents through the same engine as 'zuano skill
                migrate' and 'zuano agent migrate'

Agent profile
  The sync also sets ZUANO_AGENT=1 where an agent has an environment setting
  (Claude Code settings.json "env", Codex shell_environment_policy.set; the
  user scope, kind env) and in the environment of stdio MCP servers that run
  zuano. A zuano the agent starts then uses the agent profile, whose writes
  wait for approval, instead of your own session. Sign it in with 'zuano login
  --agent'. A login without --agent on the same machine stays unrestricted for
  anything that can run it: this narrows what agents do by default, it is not
  a sandbox. Leave the env kind out (--kind mcp,rules) to keep the old behaviour.

What zuano writes is recorded in a hash ledger (comment markers only show
where a TOML or Markdown block begins and ends) so a later sync can update or
remove it, and notices when you edited it.
Entries you wrote are never replaced unless --force is given. Every write
is one transaction: a failure restores the files already changed.

Without --to, every agent detected on this machine is synced. Kinds an agent
has no format for are reported as unsupported, never skipped silently; see
'zuano sync --matrix'.

Starter packs
  When a gateway is configured and you are signed in, the sync also fetches
  what your organisation's installed packs assign you: their rules are written
  with the others, and their skills and plugin bundles are installed. The list
  is signed by the gateway and checked first. Without a gateway, or with
  --no-packs, nothing is fetched. A pack that needs a connection you have not
  made is listed with the command that makes it.

Plugin bundles from the gateway are installed with 'zuano plugin install'.

Examples:
  zuano sync --dry-run
  zuano sync --to claude,codex --scope all
  zuano sync --kind mcp,rules --to all`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if matrix {
				return printSyncMatrix(out)
			}
			if !o.dryRun {
				if err := refuseInAgentMode(brand.Cmd("sync")); err != nil {
					return err
				}
			}
			var ps *packSync
			if !noPacks {
				var err error
				if ps, err = packSyncLoader(cmd, o.gatewayURL); err != nil {
					return err
				}
				if ps != nil {
					o.extraRules = ps.rules
				}
			}
			run, err := planSync(cmd, o)
			if err != nil {
				return err
			}
			if len(run.agents) == 0 {
				return nil
			}
			if err := printSyncReport(out, run, o.dryRun, o.jsonOut); err != nil {
				return err
			}
			if o.dryRun {
				if ps != nil && !o.jsonOut {
					ps.preview(out)
				}
				return nil
			}
			if err := confirmAgentWrite(cmd, run, brand.Cmd("sync"), o.yes); err != nil {
				return err
			}
			if err := run.apply(cmd.Context()); err != nil {
				return err
			}
			if ps != nil {
				return ps.apply(cmd)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringSliceVar(&o.to, "to", nil, "Agents to sync: claude, codex, cursor, opencode, gemini-cli, windsurf, cline, copilot, or all (default: detected agents)")
	f.StringSliceVar(&o.scopes, "scope", nil, "user, project or all (default: user; project uses the current directory)")
	f.StringSliceVar(&o.kinds, "kind", nil, "Limit to mcp, env, hooks, rules, commands, skills, subagents (default: all)")
	f.StringVar(&o.from, "from", "", "Provider whose skills and subagents are copied to the other agents (default: claude)")
	f.StringVar(&o.mcpAuth, "mcp-auth", "auto", "Gateway MCP credentials: auto and oauth (the agent signs in with OAuth, which the gateway marks as an agent's), or api-key (references $ZUANO_API_KEY, which is not marked and bypasses approvals)")
	f.BoolVar(&o.dryRun, "dry-run", false, "Show what would change as a diff and write nothing")
	f.BoolVar(&o.force, "force", false, "Replace same-named entries zuano does not manage")
	f.BoolVar(&o.yes, "yes", false, "Write without asking when hooks or local commands change (review with --dry-run first; refused when ZUANO_AGENT is set)")
	f.BoolVar(&o.acceptLoss, "accept-loss", false, "Copy skills and subagents even when a target drops non-security fields")
	f.BoolVar(&o.jsonOut, "json", false, "Print the report as JSON")
	f.BoolVar(&noPacks, "no-packs", false, "Do not fetch or write the rules, skills and bundles your organisation's starter packs assign")
	f.BoolVar(&matrix, "matrix", false, "Print the agent by artifact support matrix and exit")
	f.StringVar(&o.gatewayURL, "gateway-url", "", "Gateway URL for the gateway MCP server (default: $ZUANO_URL, then the config file)")
	cmd.AddCommand(SyncRoutesCmd())
	return cmd
}

// planSync follows the saved routes unless --to, --from or --kind ask for
// one ordinary sync.
func planSync(cmd *cobra.Command, o agentSyncOpts) (*agentSyncRun, error) {
	fl := cmd.Flags()
	if !fl.Changed("to") && !fl.Changed("from") && !fl.Changed("kind") {
		s, err := config.SyncConfig()
		if err != nil {
			return nil, err
		}
		if len(s.Routes) > 0 || len(s.Defaults) > 0 {
			return planRoutedSync(o, s)
		}
	}
	return planAgentSync(o)
}
