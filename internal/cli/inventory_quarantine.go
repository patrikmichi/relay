package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const shadowQuarantineAPI = "/api/agents/shadow-mcp/quarantine"

// quarantineSync runs after a sync; a variable so tests that sync against a
// gateway they do not mean to ask never dial it.
var quarantineSync = syncQuarantine

// savedQuarantine is a server taken out of an agent's config and kept so it
// can be put back.
type savedQuarantine struct {
	ID        string                `json:"id"`
	At        time.Time             `json:"at"`
	Item      agentsync.Quarantined `json:"item"`
	BackupDir string                `json:"backupDir"`
}

type quarantineState struct {
	Items []savedQuarantine `json:"items"`
}

func quarantineDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "quarantine"), nil
}

func loadQuarantineState() (quarantineState, string, error) {
	dir, err := quarantineDir()
	if err != nil {
		return quarantineState{}, "", err
	}
	path := filepath.Join(dir, "state.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return quarantineState{}, path, nil
	}
	if err != nil {
		return quarantineState{}, path, err
	}
	var st quarantineState
	if err := json.Unmarshal(raw, &st); err != nil {
		return quarantineState{}, path, fmt.Errorf("%s is damaged: %w", path, err)
	}
	return st, path, nil
}

func (st quarantineState) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var nonIDChars = regexp.MustCompile(`[^a-z0-9]+`)

func quarantineID(at time.Time, agent, name string) string {
	slug := func(s string) string { return strings.Trim(nonIDChars.ReplaceAllString(strings.ToLower(s), "-"), "-") }
	return "qrt_" + at.UTC().Format("20060102T150405") + "_" + slug(agent) + "_" + slug(name)
}

type shadowList struct {
	Action  string                       `json:"action"`
	Entries []agentsync.QuarantineTarget `json:"entries"`
}

func fetchShadowList(ctx context.Context, c *client.Client) (shadowList, error) {
	var doc shadowList
	_, err := gatewayJSON(ctx, c, http.MethodGet, shadowQuarantineAPI, nil, &doc)
	return doc, err
}

type quarantineAction struct {
	Agent  string `json:"agent"`
	Scope  string `json:"scope"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Hash   string `json:"hash"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// homeRelative keeps the person's user name out of what is reported.
func homeRelative(home, path string) string {
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(filepath.Join("~", rel))
	}
	return filepath.ToSlash(path)
}

func reportQuarantine(ctx context.Context, c *client.Client, env agentsync.Env, items []agentsync.Quarantined) error {
	in, err := config.LoadInstall()
	if err != nil {
		return err
	}
	var actions []quarantineAction
	for _, q := range items {
		actions = append(actions, quarantineAction{Agent: string(q.Target.Agent), Scope: string(q.Target.Scope), Name: q.Target.Name, Path: homeRelative(env.Home, q.Path), Hash: q.Hash, Result: q.Status, Detail: q.Detail})
	}
	_, err = gatewayJSON(ctx, c, http.MethodPost, shadowQuarantineAPI+"/actions", map[string]any{"installId": in.InstallID, "actions": actions}, nil)
	return err
}

// applyQuarantine takes the listed servers out of the agents' configs, keeps
// a copy of each config file as it was, and tells the gateway what it did.
func applyQuarantine(ctx context.Context, out io.Writer, c *client.Client, env agentsync.Env, targets []agentsync.QuarantineTarget, dryRun bool) error {
	plan, results, err := agentsync.PlanQuarantine(env, targets)
	if err != nil {
		return err
	}
	var done []agentsync.Quarantined
	for _, q := range results {
		switch q.Status {
		case agentsync.StatusQuarantined:
			verb := "Quarantined"
			if dryRun {
				verb = "[dry-run] would quarantine"
			}
			fmt.Fprintf(out, "%s %s %s (%s)\n", verb, textsafe.Clean(string(q.Target.Agent)), textsafe.Strict(q.Target.Name), homeRelative(env.Home, q.Path))
		default:
			fmt.Fprintf(out, "Left %s %s: %s\n", textsafe.Clean(string(q.Target.Agent)), textsafe.Strict(q.Target.Name), textsafe.Strict(q.Detail))
		}
		done = append(done, q)
	}
	if dryRun || !plan.Changed() {
		return nil
	}
	st, statePath, err := loadQuarantineState()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	byPath := map[string][]byte{}
	for _, e := range plan.Edits {
		byPath[e.Path] = e.Before
	}
	var saved []savedQuarantine
	for _, q := range done {
		if q.Status != agentsync.StatusQuarantined {
			continue
		}
		id := quarantineID(now, string(q.Target.Agent), q.Target.Name)
		dir, err := quarantineDir()
		if err != nil {
			return err
		}
		backup := filepath.Join(dir, id)
		if err := os.MkdirAll(backup, 0o700); err != nil {
			return err
		}
		if before, ok := byPath[q.Path]; ok {
			if err := os.WriteFile(filepath.Join(backup, "config.before"), before, 0o600); err != nil {
				return err
			}
		}
		saved = append(saved, savedQuarantine{ID: id, At: now, Item: q, BackupDir: backup})
	}
	st.Items = append(st.Items, saved...)
	if err := st.save(statePath); err != nil {
		return err
	}
	if err := plan.Apply(ctx); err != nil {
		return err
	}
	if c != nil {
		if err := reportQuarantine(ctx, c, env, done); err != nil {
			fmt.Fprintf(out, "warning: the gateway was not told what was quarantined: %v\n", err)
		}
	}
	return nil
}

// syncQuarantine applies the organisation's quarantine list after a sync. It
// does nothing unless the organisation chose to quarantine, and never fails
// the sync.
func syncQuarantine(cmd *cobra.Command, gatewayURL string, dryRun bool) {
	if Offline() || config.AgentMode() {
		return
	}
	gw, err := config.GatewayURL()
	if err != nil || gw == "" {
		return
	}
	if gatewayURL != "" {
		if gw, err = config.NormalizeGatewayURL(gatewayURL); err != nil {
			return
		}
	}
	ctx := nonNilContext(cmd.Context())
	c, err := client.Resolve(gw)
	if err != nil {
		return
	}
	list, err := fetchShadowList(ctx, c)
	if err != nil || list.Action != "quarantine" || len(list.Entries) == 0 {
		return
	}
	env, err := syncEnv()
	if err != nil {
		return
	}
	if err := applyQuarantine(ctx, cmd.OutOrStdout(), c, env, list.Entries, dryRun); err != nil {
		cmd.PrintErrf("warning: unreviewed MCP servers were not quarantined: %v\n", err)
	}
}

func agentsQuarantineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quarantine",
		Short: "MCP servers on this machine that your organisation switched off",
		Long: `MCP servers that someone added to an agent on their own, and that are neither
the organisation's connectors nor routed through the firewall, show up in the
inventory as shadow MCP. An administrator can choose, org-wide and with a
passkey, to quarantine them instead of only reporting them. 'zuano sync' then
takes each listed server out of the agent's config: the server is kept (with a
copy of the config file as it was) and can be put back, nothing is deleted
silently, and the gateway is told what was done and to which file. A server
that comes back is found again by the next report.

Only servers you wrote are touched, never one zuano manages. All of this is
for a person; it is refused while ZUANO_AGENT is set (exit 10). The copies hold
the servers as they were, environment values included, in a private directory.`,
	}
	cmd.AddCommand(quarantineApplyCmd(), quarantineListCmd(), quarantineRestoreCmd())
	return cmd
}

func quarantineApplyCmd() *cobra.Command {
	var f approvalFlags
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Quarantine the servers the organisation listed, now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("agents quarantine apply")); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			list, err := fetchShadowList(cmd.Context(), c)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if list.Action != "quarantine" {
				fmt.Fprintln(out, "Your organisation only reports shadow MCP servers; nothing is quarantined.")
				return nil
			}
			if len(list.Entries) == 0 {
				fmt.Fprintln(out, "No server is listed for quarantine.")
				return nil
			}
			env, err := syncEnv()
			if err != nil {
				return err
			}
			return applyQuarantine(cmd.Context(), out, c, env, list.Entries, dryRun)
		},
	}
	f.bind(cmd, "Unused: the command prints what it did")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be quarantined and change nothing")
	return cmd
}

func quarantineListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the servers quarantined on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, _, err := loadQuarantineState()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				raw, _ := json.MarshalIndent(st, "", "  ")
				fmt.Fprintln(out, string(raw))
				return nil
			}
			if len(st.Items) == 0 {
				fmt.Fprintln(out, "Nothing is quarantined on this machine.")
				return nil
			}
			var rows [][]string
			for _, s := range st.Items {
				rows = append(rows, []string{s.ID, string(s.Item.Target.Agent), s.Item.Target.Name, s.Item.Path, s.At.Format("2006-01-02 15:04")})
			}
			printRows(out, []string{"ID", "AGENT", "SERVER", "CONFIG", "WHEN"}, rows)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the list, entries included, as JSON")
	return cmd
}

func quarantineRestoreCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "restore [id]",
		Short: "Put a quarantined server back in its agent's config",
		Long: `Puts the server back as it was. While your organisation still lists it, the next
sync quarantines it again; ask an administrator to release it first.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("agents quarantine restore")); err != nil {
				return err
			}
			if all == (len(args) == 1) {
				return errors.New("name one quarantined server by id, or pass --all")
			}
			st, statePath, err := loadQuarantineState()
			if err != nil {
				return err
			}
			var picked []savedQuarantine
			var rest []savedQuarantine
			for _, s := range st.Items {
				if all || s.ID == args[0] {
					picked = append(picked, s)
				} else {
					rest = append(rest, s)
				}
			}
			if len(picked) == 0 {
				return fmt.Errorf("nothing quarantined has the id %q (see `%s`)", textsafe.Clean(strings.Join(args, "")), brand.Cmd("agents quarantine list"))
			}
			env, err := syncEnv()
			if err != nil {
				return err
			}
			items := make([]agentsync.Quarantined, len(picked))
			for i, s := range picked {
				items[i] = s.Item
			}
			plan, results, err := agentsync.PlanRestore(env, items)
			if err != nil {
				return err
			}
			if err := plan.Apply(cmd.Context()); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var kept []savedQuarantine
			var reported []agentsync.Quarantined
			for i, r := range results {
				if r.Status == agentsync.StatusRestored {
					fmt.Fprintf(out, "Restored %s %s\n", textsafe.Clean(string(r.Target.Agent)), textsafe.Strict(r.Target.Name))
					_ = os.RemoveAll(picked[i].BackupDir)
					reported = append(reported, r)
					continue
				}
				fmt.Fprintf(out, "Left %s %s in quarantine: %s\n", textsafe.Clean(string(r.Target.Agent)), textsafe.Strict(r.Target.Name), textsafe.Strict(r.Detail))
				kept = append(kept, picked[i])
			}
			st.Items = append(rest, kept...)
			if err := st.save(statePath); err != nil {
				return err
			}
			if gw, gerr := config.GatewayURL(); gerr == nil && gw != "" && !Offline() && len(reported) > 0 {
				if c, cerr := client.Resolve(gw); cerr == nil {
					if rerr := reportQuarantine(cmd.Context(), c, env, reported); rerr != nil {
						fmt.Fprintf(out, "warning: the gateway was not told: %v\n", rerr)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Restore everything quarantined on this machine")
	return cmd
}
