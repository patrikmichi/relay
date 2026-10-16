package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentsync"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/textsafe"
)

// refuseInAgentMode stops a command that changes coding agent configuration
// when RELAY_AGENT selects the agent profile: an agent must not widen its
// own tools or hooks.
func refuseInAgentMode(what string) error {
	if config.AgentMode() {
		return fmt.Errorf("%s changes coding agent configuration, which is refused while RELAY_AGENT is set (--yes does not override this); ask the person who runs this machine to do it", what)
	}
	return nil
}

// execItems lists the planned entries that make an agent run code: hooks and
// stdio MCP servers that are added or changed.
func (r *agentSyncRun) execItems() []agentsync.Item {
	var out []agentsync.Item
	for _, it := range r.items {
		if it.Status != agentsync.StatusAdded && it.Status != agentsync.StatusUpdated {
			continue
		}
		switch it.Kind {
		case agentsync.KindHooks:
			out = append(out, it)
		case agentsync.KindMCP:
			for name := range r.stdio {
				if it.Name == name || strings.HasPrefix(it.Name, "mcp_servers."+name) {
					out = append(out, it)
					break
				}
			}
		}
	}
	return out
}

// confirmAgentWrite gates a write to agent configuration. It refuses in agent
// mode. When the write adds or changes hooks or stdio MCP commands, or points
// the gateway entry at a gateway other than the saved one, a person must
// approve the full diff in a terminal, or pass --yes after reviewing it with
// --dry-run.
func confirmAgentWrite(cmd *cobra.Command, run *agentSyncRun, what string, yes bool) error {
	if err := refuseInAgentMode(what); err != nil {
		return err
	}
	risky := run.execItems()
	if len(risky) == 0 && run.gatewayURL == "" {
		return nil
	}
	if yes {
		return nil
	}
	in := cmd.InOrStdin()
	if !canPrompt(in) {
		return fmt.Errorf("%s adds or changes hooks, local commands or the gateway URL in agent configuration. Review it with --dry-run, then re-run in a terminal to approve it, or pass --yes", what)
	}

	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "\n%s will change what your coding agents run:\n", what)
	for _, it := range risky {
		fmt.Fprintf(out, "  %s %s (%s, %s): %s\n", it.Status, it.Kind, it.Agent, it.Scope, textsafe.Reveal(it.Name))
	}
	if run.gatewayURL != "" {
		fmt.Fprintf(out, "  the gateway MCP entry will point at %s, not the gateway saved in your config\n", run.gatewayURL)
	}
	if run.plan != nil {
		for _, e := range run.plan.Edits {
			fmt.Fprintf(out, "\n%s", textsafe.Reveal(e.Diff()))
		}
	}
	if !askNo(bufio.NewReader(in), out, "\nWrite these changes?") {
		return errors.New("not confirmed; nothing was written")
	}
	return nil
}

// askNo asks a yes/no question that defaults to no.
func askNo(r *bufio.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [y/N] ", question)
	line, _ := r.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
