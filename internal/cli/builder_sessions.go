package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

type sessionRow struct {
	ID            string `json:"id"`
	UserID        string `json:"userId"`
	Identity      string `json:"identity"`
	ConfigVersion int    `json:"configVersion"`
	CreatedAt     string `json:"createdAt"`
	LastUsedAt    string `json:"lastUsedAt"`
	MCP           struct {
		URL string `json:"url"`
	} `json:"mcp"`
}

func sessionPath(id string, rest ...string) string {
	return builderAPI + "/sessions/" + url.PathEscape(id) + strings.Join(rest, "")
}

// sessionPolicyFlags build the body of a session from flags; --config-file
// supplies the whole policy for what the flags do not reach.
type sessionPolicyFlags struct {
	file, configID, identity, preset string
	toolkits, disableToolkits        []string
	riskDisable, requireApproval     []string
	authConfigs, accounts, metadata  []string
	idleDays                         int
	memory, sandbox                  bool
}

func (p *sessionPolicyFlags) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&p.file, "config-file", "", "JSON file with the session policy (the fields below override it)")
	f.StringVar(&p.configID, "config-id", "", "Use a saved session config (scf_...); not combinable with toolkits or risk")
	f.StringSliceVar(&p.toolkits, "toolkits", nil, "Allow only these services (comma-separated)")
	f.StringSliceVar(&p.disableToolkits, "disable-toolkits", nil, "Allow every service except these")
	f.StringSliceVar(&p.riskDisable, "risk-disable", nil, "Risk classes to turn off: read, write, destructive, outward")
	f.StringSliceVar(&p.requireApproval, "require-approval", nil, "Risk classes that need an approval for every call")
	f.StringArrayVar(&p.authConfigs, "auth-config", nil, "service=ac_... auth config to use for a service; repeatable")
	f.StringArrayVar(&p.accounts, "account", nil, "service=ca_... connected account to use for a service; repeatable")
	f.StringVar(&p.identity, "identity", "", "service (default) or delegated_user")
	f.StringVar(&p.preset, "preset", "", "meta (default) or direct")
	f.IntVar(&p.idleDays, "idle-days", 0, "Delete the session after this many idle days")
	f.BoolVar(&p.memory, "memory", false, "Give the session memory")
	f.BoolVar(&p.sandbox, "sandbox", false, "Allow run_code in the session")
	f.StringArrayVar(&p.metadata, "metadata", nil, "key=value kept with the session (at most 4 KB); repeatable")
}

func pairs(list []string, flag string) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range list {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("--%s %q must be service=id", flag, textsafe.Clean(p))
		}
		out[k] = v
	}
	return out, nil
}

func (p *sessionPolicyFlags) body(cmd *cobra.Command) (map[string]any, error) {
	body := map[string]any{}
	if p.file != "" {
		fh, err := os.ReadFile(p.file)
		if err != nil {
			return nil, err
		}
		doc, err := readJSONBytes(fh, "the session policy")
		if err != nil {
			return nil, err
		}
		body = doc
	}
	set := func(k string, v any) { body[k] = v }
	changed := cmd.Flags().Changed
	if p.configID != "" {
		set("configId", p.configID)
	}
	if changed("toolkits") {
		set("toolkits", map[string]any{"enable": p.toolkits})
	}
	if changed("disable-toolkits") {
		set("toolkits", map[string]any{"disable": p.disableToolkits})
	}
	if changed("risk-disable") || changed("require-approval") {
		risk, _ := body["risk"].(map[string]any)
		if risk == nil {
			risk = map[string]any{}
		}
		if changed("risk-disable") {
			risk["disable"] = p.riskDisable
		}
		if changed("require-approval") {
			risk["requireApproval"] = p.requireApproval
		}
		set("risk", risk)
	}
	if len(p.authConfigs) > 0 {
		m, err := pairs(p.authConfigs, "auth-config")
		if err != nil {
			return nil, err
		}
		set("authConfigs", m)
	}
	if len(p.accounts) > 0 {
		m, err := pairs(p.accounts, "account")
		if err != nil {
			return nil, err
		}
		accounts := map[string]any{}
		for k, v := range m {
			accounts[k] = []any{v}
		}
		set("connectedAccounts", accounts)
	}
	if p.identity != "" {
		if p.identity != "service" && p.identity != "delegated_user" {
			return nil, errors.New("--identity must be service or delegated_user")
		}
		set("identity", p.identity)
	}
	if p.preset != "" {
		if p.preset != "meta" && p.preset != "direct" {
			return nil, errors.New("--preset must be meta or direct")
		}
		set("preset", p.preset)
	}
	if p.idleDays > 0 {
		set("ttl", map[string]any{"idleDays": p.idleDays})
	}
	if changed("memory") {
		set("memory", map[string]any{"enable": p.memory})
	}
	if changed("sandbox") {
		set("sandbox", map[string]any{"enable": p.sandbox})
	}
	if len(p.metadata) > 0 {
		meta := map[string]any{}
		for _, m := range p.metadata {
			k, v, ok := strings.Cut(m, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("--metadata %q must be key=value", textsafe.Clean(m))
			}
			meta[k] = v
		}
		set("metadata", meta)
	}
	return body, nil
}

func builderSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Create and manage sessions for end users",
	}
	cmd.AddCommand(bsCreateCmd(), bsListCmd(), bsShowCmd(), bsUpdateCmd(), bsDeleteCmd(), bsHistoryCmd(), bsToolkitsCmd(), bsTokenCmd(), bsRevokeTokenCmd())
	return cmd
}

func bsCreateCmd() *cobra.Command {
	var f approvalFlags
	var p sessionPolicyFlags
	var user string
	cmd := &cobra.Command{
		Use:   "create --user ID",
		Short: "Create a session for an end user",
		Long: `Creates a session for a user: which services and risk classes an agent may use
for them, which accounts it may use, and the state it keeps. It can only narrow
what the organisation's and the project's policy allow; asking for more is
refused (policy_widening, exit 1) with the offending entries.`,
		Example: `  zuano builder sessions create --user user_123 --toolkits gmail,github --risk-disable destructive,outward --require-approval write`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if user == "" {
				return errors.New("--user is the end user's external id")
			}
			body, err := p.body(cmd)
			if err != nil {
				return err
			}
			body["userId"] = user
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Session sessionRow `json:"session"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, builderAPI+"/sessions", body, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "Created %s for %s\n", textsafe.Clean(doc.Session.ID), textsafe.Strict(user))
			if doc.Session.MCP.URL != "" {
				fmt.Fprintf(out, "MCP address: %s\nMake a token for an agent with `%s`.\n", textsafe.Clean(doc.Session.MCP.URL), brand.Cmd("builder sessions token "+doc.Session.ID))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&user, "user", "", "The end user's external id")
	p.bind(cmd)
	return cmd
}

func bsListCmd() *cobra.Command {
	var f approvalFlags
	var user, cursor string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := map[string]string{"userId": user, "cursor": cursor}
			if limit > 0 {
				q["limit"] = strconv.Itoa(limit)
			}
			var doc struct {
				Items      []sessionRow `json:"items"`
				NextCursor string       `json:"nextCursor"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(builderAPI+"/sessions", q), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintln(out, "No sessions.")
				return nil
			}
			var rows [][]string
			for _, s := range doc.Items {
				rows = append(rows, []string{s.ID, s.UserID, orDash(s.Identity), strconv.Itoa(s.ConfigVersion), orDash(s.LastUsedAt)})
			}
			printRows(out, []string{"ID", "USER", "IDENTITY", "VERSION", "LAST USED"}, rows)
			if doc.NextCursor != "" {
				fmt.Fprintf(out, "\nMore: --cursor %s\n", textsafe.Clean(doc.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	cmd.Flags().StringVar(&user, "user", "", "Only this end user's sessions")
	cmd.Flags().IntVar(&limit, "limit", 0, "Page size, at most 100")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue from a previous page")
	return cmd
}

func bsShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <ses_id>",
		Short: "Show a session and its policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ses_", "a session id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Session sessionRow `json:"session"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, sessionPath(id), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			s := doc.Session
			fmt.Fprintf(out, "%s  user %s, identity %s, config version %d\n  created %s, last used %s\n", textsafe.Clean(s.ID), textsafe.Strict(s.UserID), orDash(s.Identity), s.ConfigVersion, orDash(s.CreatedAt), orDash(s.LastUsedAt))
			if s.MCP.URL != "" {
				fmt.Fprintf(out, "  MCP address: %s\n", textsafe.Clean(s.MCP.URL))
			}
			fmt.Fprintf(out, "Use --json for the whole policy.\n")
			return nil
		},
	}
	f.bind(cmd, "Print the session and its policy as JSON")
	return cmd
}

func bsUpdateCmd() *cobra.Command {
	var f approvalFlags
	var p sessionPolicyFlags
	var expected int
	cmd := &cobra.Command{
		Use:   "update <ses_id> --expected-version N",
		Short: "Change a session's policy",
		Long: `Maps (accounts, auth configs, per-service tools) and policy blocks (toolkits,
risk) are replaced; memory, sandbox and similar blocks merge. A version that no
longer matches --expected-version is a conflict (exit 1) and nothing is written;
read the session again and retry. The identity mode cannot change.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ses_", "a session id")
			if err != nil {
				return err
			}
			if expected <= 0 {
				return errors.New("--expected-version is the config version you read, so two writers cannot overwrite each other")
			}
			if p.identity != "" {
				return errors.New("the identity mode cannot change after a session is created")
			}
			body, err := p.body(cmd)
			if err != nil {
				return err
			}
			body["expectedConfigVersion"] = expected
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPatch, sessionPath(id), body, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %s\n", id)
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().IntVar(&expected, "expected-version", 0, "The config version this change is based on")
	p.bind(cmd)
	return cmd
}

func bsDeleteCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <ses_id>",
		Short: "Delete a session: its state, its tokens and its pending approvals",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ses_", "a session id")
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete session %s? Its state is lost and its tokens stop working.", id)); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodDelete, sessionPath(id), nil, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", id)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without asking")
	return cmd
}

func bsHistoryCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "history <ses_id>",
		Short: "Show the versions of a session's policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ses_", "a session id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items []struct {
					Version int    `json:"version"`
					At      string `json:"at"`
					By      string `json:"by"`
					Summary string `json:"summary"`
				} `json:"items"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, sessionPath(id, "/config-history"), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows [][]string
			for _, h := range doc.Items {
				rows = append(rows, []string{strconv.Itoa(h.Version), h.At, orDash(h.By), h.Summary})
			}
			printRows(out, []string{"VERSION", "WHEN", "BY", "CHANGE"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the history as JSON")
	return cmd
}

func bsToolkitsCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "toolkits <ses_id>",
		Short: "Show which services a session may use and whether each is connected",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ses_", "a session id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items []struct {
					Service   string   `json:"service"`
					Connected bool     `json:"connected"`
					Accounts  []string `json:"accounts"`
				} `json:"items"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, sessionPath(id, "/toolkits"), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows [][]string
			for _, t := range doc.Items {
				state := "not connected"
				if t.Connected {
					state = "connected"
				}
				rows = append(rows, []string{t.Service, state, strings.Join(t.Accounts, ", ")})
			}
			printRows(out, []string{"SERVICE", "STATE", "ACCOUNTS"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	return cmd
}

func bsTokenCmd() *cobra.Command {
	var f approvalFlags
	var expires string
	cmd := &cobra.Command{
		Use:   "token <ses_id>",
		Short: "Make a short-lived token an agent connects to the session with",
		Long: `Prints the token once, on standard output, and the MCP address it works at; the
gateway does not show it again. It expires in an hour unless --expires-in says
otherwise (at most 24h). Refused in an agent session (exit 10).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("builder sessions token")); err != nil {
				return err
			}
			id, err := builderID(args[0], "ses_", "a session id")
			if err != nil {
				return err
			}
			body := map[string]any{}
			if expires != "" {
				d, err := time.ParseDuration(expires)
				if err != nil || d < time.Minute || d > 24*time.Hour {
					return errors.New("--expires-in must be a duration between 1m and 24h")
				}
				body["expiresIn"] = int(d.Seconds())
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Token     string `json:"token"`
				TokenID   string `json:"tokenId"`
				ExpiresAt string `json:"expiresAt"`
				MCP       struct {
					URL string `json:"url"`
				} `json:"mcp"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, sessionPath(id, "/tokens"), body, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Token %s, expires %s, MCP address %s\nShown once; revoke it with `%s`.\n",
				textsafe.Clean(doc.TokenID), textsafe.Clean(doc.ExpiresAt), textsafe.Clean(doc.MCP.URL), brand.Cmd("builder sessions revoke-token "+id+" "+doc.TokenID))
			fmt.Fprintln(out, doc.Token)
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer, token included, as JSON")
	cmd.Flags().StringVar(&expires, "expires-in", "", "Lifetime, such as 30m (1m to 24h; default 1h)")
	return cmd
}

func bsRevokeTokenCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "revoke-token <ses_id> <token_id>",
		Short: "Revoke a session token",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ses_", "a session id")
			if err != nil {
				return err
			}
			if args[1] == "" || strings.ContainsAny(args[1], "/?#% ") {
				return fmt.Errorf("%q is not a token id", textsafe.Clean(args[1]))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodDelete, sessionPath(id, "/tokens/", url.PathEscape(args[1])), nil, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s\n", args[1])
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	return cmd
}

func builderSessionConfigsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session-configs",
		Short: "Saved session policies that sessions can use by id",
	}
	cmd.AddCommand(scListCmd(), scShowCmd(), scSaveCmd("create"), scSaveCmd("update"), scDeleteCmd())
	return cmd
}

func scPath(id string) string { return builderAPI + "/session-configs/" + url.PathEscape(id) }

func scListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List saved session configs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items []struct {
					ID      string `json:"id"`
					Name    string `json:"name"`
					Version int    `json:"version"`
				} `json:"items"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, builderAPI+"/session-configs", nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows [][]string
			for _, s := range doc.Items {
				rows = append(rows, []string{s.ID, s.Name, strconv.Itoa(s.Version)})
			}
			printRows(out, []string{"ID", "NAME", "VERSION"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	return cmd
}

func scShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <scf_id>",
		Short: "Show a saved session config as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "scf_", "a session config id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := builderRequest(cmd, c, true, http.MethodGet, scPath(id), nil, nil)
			if err != nil {
				return err
			}
			printRaw(cmd.OutOrStdout(), raw)
			return nil
		},
	}
	f.bind(cmd, "Unused: the config is always printed as JSON")
	_ = cmd.Flags().MarkHidden("json")
	return cmd
}

func scSaveCmd(verb string) *cobra.Command {
	var f approvalFlags
	var p sessionPolicyFlags
	var name string
	var propagate bool
	use := "create --name N"
	if verb == "update" {
		use = "update <scf_id>"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: map[string]string{"create": "Save a session policy under a name", "update": "Save a new version of a session config"}[verb],
		Args:  map[string]cobra.PositionalArgs{"create": cobra.NoArgs, "update": cobra.ExactArgs(1)}[verb],
		RunE: func(cmd *cobra.Command, args []string) error {
			policy, err := p.body(cmd)
			if err != nil {
				return err
			}
			body := map[string]any{"policy": policy}
			method, path := http.MethodPost, builderAPI+"/session-configs"
			if verb == "create" {
				if name == "" {
					return errors.New("--name is required")
				}
				body["name"] = name
			} else {
				id, err := builderID(args[0], "scf_", "a session config id")
				if err != nil {
					return err
				}
				method, path = http.MethodPatch, scPath(id)
				if name != "" {
					body["name"] = name
				}
				if propagate {
					body["propagate"] = true
				}
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				ID      string `json:"id"`
				Version int    `json:"version"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, method, path, body, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s as version %d\n", textsafe.Clean(doc.ID), doc.Version)
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&name, "name", "", "Name of the config")
	if verb == "update" {
		cmd.Flags().BoolVar(&propagate, "propagate", false, "Apply it to every session that uses the config")
	}
	p.bind(cmd)
	_ = cmd.Flags().MarkHidden("config-id")
	return cmd
}

func scDeleteCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <scf_id>",
		Short: "Delete a saved session config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "scf_", "a session config id")
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete session config %s?", id)); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodDelete, scPath(id), nil, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", id)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without asking")
	return cmd
}
