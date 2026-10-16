package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

type accountDoc struct {
	ID           string `json:"id"`
	Service      string `json:"service"`
	Alias        string `json:"alias"`
	Status       string `json:"status"`
	OwnerKind    string `json:"ownerKind"`
	UserID       string `json:"userId"`
	AuthConfigID string `json:"authConfigId"`
	DisplayName  string `json:"displayName"`
	IsPrimary    bool   `json:"isPrimary"`
	CreatedAt    string `json:"createdAt"`
}

// AccountsCmd returns `zuano accounts`: the credentials of one owner for one
// service. Credentials in and out are write-only, like auth config secrets.
func AccountsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "List, rename, import and remove connected accounts",
		Long: `A connected account is one owner's credential for one service, made through an
auth config. An owner is a member, the organisation or an end user of a project.
Several accounts per owner and service are allowed; the primary one is the
default.

Importing brings credentials you already hold: an API key, a bearer token or an
OAuth refresh token (only for a custom OAuth config, because a refresh token is
bound to the app that issued it). They are given as JSON on standard input or in
a file, never in a flag, and are never shown again. Importing and changing
credentials are refused in an agent session (exit 10).`,
	}
	cmd.AddCommand(accListCmd(), accShowCmd(), accUpdateCmd(), accDeleteCmd(), accCredentialsCmd(), accImportCmd(), accImportBatchCmd())
	return cmd
}

func accPath(id string, rest ...string) string {
	p := builderAPI + "/connected-accounts/" + url.PathEscape(id)
	for _, r := range rest {
		p += r
	}
	return p
}

func accRows(list []accountDoc) [][]string {
	var rows [][]string
	for _, a := range list {
		owner := a.OwnerKind
		if a.UserID != "" {
			owner += ":" + a.UserID
		}
		primary := ""
		if a.IsPrimary {
			primary = "primary"
		}
		rows = append(rows, []string{a.ID, a.Service, orDash(a.Alias), a.Status, owner, a.AuthConfigID, primary})
	}
	return rows
}

func accListCmd() *cobra.Command {
	var f approvalFlags
	var user, service, authConfig, status, alias, cursor string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List connected accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items      []accountDoc `json:"items"`
				NextCursor string       `json:"nextCursor"`
			}
			q := map[string]string{"userId": user, "service": service, "authConfigId": authConfig, "status": status, "alias": alias, "cursor": cursor}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(builderAPI+"/connected-accounts", q), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintln(out, "No connected accounts match.")
				return nil
			}
			printRows(out, []string{"ID", "SERVICE", "ALIAS", "STATUS", "OWNER", "AUTH CONFIG", ""}, accRows(doc.Items))
			if doc.NextCursor != "" {
				fmt.Fprintf(out, "\nMore: --cursor %s\n", textsafe.Clean(doc.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	cmd.Flags().StringVar(&user, "user", "", "An end user's external id")
	cmd.Flags().StringVar(&service, "service", "", "Only this service")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "Only accounts made through this auth config")
	cmd.Flags().StringVar(&status, "status", "", "initiated, active, needs_reconnect, suspended, revoked or expired")
	cmd.Flags().StringVar(&alias, "alias", "", "Only the account with this alias")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue from a previous page")
	return cmd
}

func accShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <ca_id>",
		Short: "Show a connected account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ca_", "a connected account id")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Account accountDoc `json:"account"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, accPath(id), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			a := doc.Account
			fmt.Fprintf(out, "%s  %s (%s)\n  status %s, owner %s%s, auth config %s, primary %t\n", textsafe.Clean(a.ID), textsafe.Strict(orDash(a.DisplayName)), textsafe.Clean(a.Service),
				textsafe.Clean(a.Status), textsafe.Clean(a.OwnerKind), textsafe.Strict(map[bool]string{true: ":" + a.UserID}[a.UserID != ""]), textsafe.Clean(a.AuthConfigID), a.IsPrimary)
			if a.Alias != "" {
				fmt.Fprintf(out, "  alias %s\n", textsafe.Strict(a.Alias))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the account as JSON")
	return cmd
}

func accUpdateCmd() *cobra.Command {
	var f approvalFlags
	var alias string
	var primary bool
	cmd := &cobra.Command{
		Use:   "update <ca_id>",
		Short: "Rename an account or make it the primary one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ca_", "a connected account id")
			if err != nil {
				return err
			}
			body := map[string]any{}
			if cmd.Flags().Changed("alias") {
				body["alias"] = alias
			}
			if cmd.Flags().Changed("primary") {
				body["isPrimary"] = primary
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: give --alias or --primary")
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPatch, accPath(id), body, nil)
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
	cmd.Flags().StringVar(&alias, "alias", "", "New alias, unique per owner and service")
	cmd.Flags().BoolVar(&primary, "primary", false, "Make it the owner's default account for the service")
	return cmd
}

func accDeleteCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <ca_id>",
		Short: "Disconnect an account: revoke it at the provider and delete its credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := builderID(args[0], "ca_", "a connected account id")
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Disconnect %s? Its credentials are deleted and calls through it stop.", id)); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodDelete, accPath(id), nil, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Disconnected %s\n", id)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Disconnect without asking")
	return cmd
}

func accCredentialsCmd() *cobra.Command {
	var f approvalFlags
	var fromFile string
	cmd := &cobra.Command{
		Use:   "credentials <ca_id>",
		Short: "Push rotated credentials to an account",
		Long: `Reads a JSON object from standard input or --from-file. Fields you leave out are
kept; a field set to null is removed. Values are never printed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("accounts credentials")); err != nil {
				return err
			}
			id, err := builderID(args[0], "ca_", "a connected account id")
			if err != nil {
				return err
			}
			creds, err := readJSONInput(cmd, fromFile, "the credentials")
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodPatch, accPath(id, "/credentials"), map[string]any{"credentials": creds}, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Updated %s on %s (values not shown)\n", maskedFields(creds), id)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "JSON file with the credentials (or - for standard input)")
	return cmd
}

func accImportCmd() *cobra.Command {
	var f approvalFlags
	var user, service, authConfig, alias, fromFile string
	cmd := &cobra.Command{
		Use:   "import --user ID --service S --auth-config ac_...",
		Short: "Bring in a credential you already hold",
		Long: `Creates an account from credentials given as JSON on standard input or in a file:

  api_key / basic   the fields 'zuano auth-configs fields <service>' lists
  bearer            {"token": "...", "expiresAt": "..."}  (no refresh)
  oauth2            {"refreshToken": "...", "accessToken": "...", "expiresAt": "...", "scope": "..."}
                    only through a custom OAuth config; one refresh validates it`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentChange(brand.Cmd("accounts import")); err != nil {
				return err
			}
			if user == "" || service == "" || authConfig == "" {
				return fmt.Errorf("--user, --service and --auth-config are required")
			}
			creds, err := readJSONInput(cmd, fromFile, "the credentials")
			if err != nil {
				return err
			}
			body := map[string]any{"userId": user, "service": service, "authConfigId": authConfig, "credentials": creds}
			if alias != "" {
				body["alias"] = alias
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Account accountDoc `json:"account"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, builderAPI+"/connected-accounts:import", body, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Imported %s (%s, %s)\n", textsafe.Clean(doc.Account.ID), textsafe.Clean(doc.Account.Service), textsafe.Clean(doc.Account.Status))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().StringVar(&user, "user", "", "The end user's external id")
	cmd.Flags().StringVar(&service, "service", "", "The service")
	cmd.Flags().StringVar(&authConfig, "auth-config", "", "The auth config the account is made through")
	cmd.Flags().StringVar(&alias, "alias", "", "Alias for the account")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "JSON file with the credentials (or - for standard input)")
	return cmd
}

func accImportBatchCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "import-batch <rows.jsonl>",
		Short: "Import up to 1000 accounts, one JSON object per line",
		Long: `Each line is {"userId", "service", "authConfigId", "alias"?, "credentials": {...}}.
The import runs as a job; follow it with 'zuano runs show <job>'. The file holds
credentials, so keep it private. Refused in an agent session (exit 10).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentChange(brand.Cmd("accounts import-batch")); err != nil {
				return err
			}
			fh, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer fh.Close()
			var rows []json.RawMessage
			sc := bufio.NewScanner(fh)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for line := 1; sc.Scan(); line++ {
				text := bytes.TrimSpace(sc.Bytes())
				if len(text) == 0 {
					continue
				}
				var row map[string]any
				if err := json.Unmarshal(text, &row); err != nil || row["userId"] == nil || row["service"] == nil || row["authConfigId"] == nil || row["credentials"] == nil {
					return fmt.Errorf("line %d needs userId, service, authConfigId and credentials", line)
				}
				rows = append(rows, append(json.RawMessage(nil), text...))
			}
			if err := sc.Err(); err != nil {
				return err
			}
			if len(rows) == 0 || len(rows) > 1000 {
				return fmt.Errorf("the file has %d rows; give between 1 and 1000", len(rows))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				JobID string `json:"jobId"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, builderAPI+"/connected-accounts:importBatch", map[string]any{"rows": rows}, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Started import job %s for %s row(s). Follow it with `%s`.\n", textsafe.Clean(doc.JobID), strconv.Itoa(len(rows)), brand.Cmd("runs show "+doc.JobID))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	return cmd
}
