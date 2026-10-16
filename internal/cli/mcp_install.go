package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentsync"
)

type mcpInstallFlags struct {
	url        string
	command    string
	args       []string
	env        []string
	bearerEnv  string
	to         []string
	scope      string
	mcpAuth    string
	gatewayURL string
	dryRun     bool
	force      bool
	yes        bool
}

func McpInstallCmd() *cobra.Command {
	var f mcpInstallFlags
	cmd := &cobra.Command{
		Use:   "install <name>",
		Short: "Register an MCP server in your coding agents' config",
		Long: `Register an MCP server in the MCP config of Claude Code, Codex, Cursor,
OpenCode, Gemini CLI, Windsurf, Cline and GitHub Copilot, without touching
servers you configured yourself.

With just a name, <name> is a service in the gateway catalog (see 'relay
connectors'). Catalog services are reached through the gateway's own MCP
server, so this enables that server in every agent; authorize the service
itself with 'relay connect <name>'.

With --url or --command the server is registered directly:

  relay mcp install docs --url https://docs.example.com/mcp --bearer-env DOCS_TOKEN
  relay mcp install files --command npx --arg -y --arg @acme/files-mcp --env ROOT='${HOME}'

Credentials are never written: --bearer-env and ${VAR} references name an
environment variable the agent reads itself. Without --to, every detected
agent is updated.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMcpInstall(cmd, args[0], f)
		},
	}
	addMcpSyncFlags(cmd, &f)
	fl := cmd.Flags()
	fl.StringVar(&f.url, "url", "", "Remote MCP server URL (streamable HTTP)")
	fl.StringVar(&f.command, "command", "", "Local stdio server command")
	fl.StringArrayVar(&f.args, "arg", nil, "Argument for --command (repeatable)")
	fl.StringArrayVar(&f.env, "env", nil, "Environment for --command as KEY=VALUE (repeatable; prefer ${VAR} references)")
	fl.StringVar(&f.bearerEnv, "bearer-env", "", "Environment variable holding a bearer token for --url")
	fl.StringVar(&f.gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	return cmd
}

func McpRemoveCmd() *cobra.Command {
	var f mcpInstallFlags
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an MCP server relay registered in your coding agents' config",
		Long: `Remove a server added with 'relay mcp install' from every agent config relay
wrote it to. Entries you edited or created yourself are left in place.

'relay mcp remove relay' stops syncing the gateway's own MCP server.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMcpRemove(cmd, args[0], f)
		},
	}
	addMcpSyncFlags(cmd, &f)
	return cmd
}

func addMcpSyncFlags(cmd *cobra.Command, f *mcpInstallFlags) {
	fl := cmd.Flags()
	fl.StringSliceVar(&f.to, "to", nil, "Agents to update (default: detected agents; all for every supported agent)")
	fl.StringVar(&f.scope, "scope", "user", "user or project")
	fl.BoolVar(&f.dryRun, "dry-run", false, "Show the diff and write nothing")
	fl.BoolVar(&f.force, "force", false, "Replace same-named entries relay does not manage")
	fl.BoolVar(&f.yes, "yes", false, "Write without asking when a local command is added (review with --dry-run first; refused when RELAY_AGENT is set)")
	fl.StringVar(&f.mcpAuth, "mcp-auth", "auto", "Gateway MCP credentials: auto and oauth (OAuth, marked as an agent's) or api-key (unmarked, bypasses approvals)")
}

func parseSingleScope(s string) (agentsync.Scope, error) {
	switch s {
	case "", "user":
		return agentsync.ScopeUser, nil
	case "project":
		return agentsync.ScopeProject, nil
	}
	return "", fmt.Errorf("invalid --scope %q: use user or project", s)
}

func parseEnvPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --env %q: want KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}

func runMcpInstall(cmd *cobra.Command, name string, f mcpInstallFlags) error {
	if !f.dryRun {
		if err := refuseInAgentMode("relay mcp install"); err != nil {
			return err
		}
	}
	scope, err := parseSingleScope(f.scope)
	if err != nil {
		return err
	}
	env, err := syncEnv()
	if err != nil {
		return err
	}
	src, err := agentsync.LoadMCPSource(env, scope)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	switch {
	case f.url == "" && f.command == "":
		if scope == agentsync.ScopeProject {
			return errors.New("the gateway server is configured at the user scope; use --url or --command for a project server")
		}
		if err := requireCatalogService(cmd, name, f.gatewayURL); err != nil {
			return err
		}
		src.GatewayDisabled = false
		fmt.Fprintf(out, "%s is served by the gateway MCP server %q; authorize it with `relay connect %s` if it needs credentials.\n", name, gatewayServerName, name)
	case f.url != "" && f.command != "":
		return errors.New("use either --url or --command, not both")
	default:
		server := agentsync.MCPServer{Name: name, URL: f.url, BearerEnv: f.bearerEnv, Command: f.command, Args: f.args}
		if server.Env, err = parseEnvPairs(f.env); err != nil {
			return err
		}
		if err := server.Validate(); err != nil {
			return err
		}
		if f.bearerEnv != "" && f.url == "" {
			return errors.New("--bearer-env only applies to --url servers")
		}
		for k, v := range server.Env {
			if !strings.Contains(v, "$") {
				fmt.Fprintf(out, "warning: %s is written as a literal into each agent's config; reference an environment variable like '${%s}' to keep it out of those files\n", k, k)
			}
		}
		replaced := false
		for i, s := range src.Servers {
			if s.Name == name {
				src.Servers[i], replaced = server, true
			}
		}
		if !replaced {
			src.Servers = append(src.Servers, server)
		}
	}
	return syncMCPSource(cmd, env, scope, src, f)
}

func runMcpRemove(cmd *cobra.Command, name string, f mcpInstallFlags) error {
	if !f.dryRun {
		if err := refuseInAgentMode("relay mcp remove"); err != nil {
			return err
		}
	}
	scope, err := parseSingleScope(f.scope)
	if err != nil {
		return err
	}
	env, err := syncEnv()
	if err != nil {
		return err
	}
	src, err := agentsync.LoadMCPSource(env, scope)
	if err != nil {
		return err
	}
	if name == gatewayServerName && scope == agentsync.ScopeUser {
		src.GatewayDisabled = true
	} else {
		kept := src.Servers[:0:0]
		for _, s := range src.Servers {
			if s.Name != name {
				kept = append(kept, s)
			}
		}
		if len(kept) == len(src.Servers) {
			return fmt.Errorf("no MCP server named %q was installed with relay at the %s scope", name, scope)
		}
		src.Servers = kept
	}
	return syncMCPSource(cmd, env, scope, src, f)
}

// syncMCPSource syncs MCP config for the given source, and persists the
// source only when files are actually written.
func syncMCPSource(cmd *cobra.Command, env agentsync.Env, scope agentsync.Scope, src agentsync.MCPSource, f mcpInstallFlags) error {
	run, err := planAgentSync(agentSyncOpts{
		to: f.to, scopes: []string{string(scope)}, kinds: []string{"mcp"}, mcpAuth: f.mcpAuth,
		dryRun: f.dryRun, force: f.force, mcpOverride: map[agentsync.Scope]agentsync.MCPSource{scope: src},
	})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if err := printSyncReport(out, run, f.dryRun, false); err != nil {
		return err
	}
	if f.dryRun {
		return nil
	}
	if err := confirmAgentWrite(cmd, run, "relay mcp install", f.yes); err != nil {
		return err
	}
	if err := run.apply(cmd.Context()); err != nil {
		return err
	}
	return agentsync.SaveMCPSource(env, scope, src)
}

// requireCatalogService checks that name is a service the gateway offers.
func requireCatalogService(cmd *cobra.Command, name, gatewayURL string) error {
	gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
	if err != nil {
		return err
	}
	c, err := resolveClient(gURL)
	if err != nil {
		return err
	}
	info, err := fetchIntegrations(c, cmd.Context())
	if err != nil {
		return err
	}
	if info == nil {
		return errors.New("token expired or revoked — run `relay login` to re-authenticate")
	}
	for _, s := range info.Services {
		if s.ID == name {
			if !s.Accessible {
				fmt.Fprintf(cmd.OutOrStdout(), "warning: your account has no access to %s yet; ask an admin or use the gateway's access request\n", name)
			}
			return nil
		}
	}
	return fmt.Errorf("no service %q in the gateway catalog; see `relay connectors search %s`", name, name)
}
