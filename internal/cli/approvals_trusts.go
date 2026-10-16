package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

const sessionGrantsAPI = "/api/approvals/session-grants"

type sessionGrant struct {
	ID         string `json:"id"`
	Client     string `json:"client"`
	Session    string `json:"session"`
	Tool       string `json:"tool"`
	Class      string `json:"class"`
	Constraint string `json:"constraint"`
	CreatedBy  string `json:"createdBy"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt"`
	Uses       int    `json:"uses"`
}

func approvalsTrustsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trusts",
		Short: "See and end the session trusts that let an agent skip an approval",
		Long: `When a person approves a call they can also trust the same kind of call for the
rest of that session, for a while. These commands list those trusts and end
them. A trust is only ever given on the approval page, with a passkey: there is
no command that creates one. Ending one only tightens things, so an agent
session may do it.`,
	}
	cmd.AddCommand(trustsListCmd(), trustsRevokeCmd(), trustsRevokeAllCmd())
	return cmd
}

func trustsListCmd() *cobra.Command {
	var f approvalFlags
	var org bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the active trusts",
		Long:  "Lists the trusts on your own sessions, with the agent, the tool or class of tool, what the trust is limited to, who gave it, when it ends and how often it was used. --org lists every trust in the organisation and needs an administrator.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			scope := "mine"
			if org {
				scope = "org"
			}
			var doc struct {
				Grants []sessionGrant `json:"grants"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(sessionGrantsAPI, map[string]string{"scope": scope}), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Grants) == 0 {
				fmt.Fprintln(out, "No active trusts.")
				return nil
			}
			var rows [][]string
			for _, g := range doc.Grants {
				what := g.Tool
				if what == "" {
					what = "all " + g.Class + " calls"
				}
				rows = append(rows, []string{g.ID, g.Client, what, orDash(g.Constraint), g.CreatedBy, g.ExpiresAt, strconv.Itoa(g.Uses)})
			}
			printRows(out, []string{"ID", "AGENT", "TRUSTS", "LIMITED TO", "GIVEN BY", "ENDS", "USES"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the trusts as JSON")
	cmd.Flags().BoolVar(&org, "org", false, "Every trust in the organisation (administrators)")
	return cmd
}

func trustsRevokeCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "End one trust now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if id == "" || strings.ContainsAny(id, "/?#% \t\n") {
				return fmt.Errorf("%q is not a trust id", textsafe.Clean(id))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if _, err := builderRequest(cmd, c, f.jsonOut, http.MethodDelete, sessionGrantsAPI+"/"+url.PathEscape(id), nil, nil); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Ended %s. The next call of that kind needs an approval again.\n", textsafe.Clean(id))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	return cmd
}

func trustsRevokeAllCmd() *cobra.Command {
	var f approvalFlags
	var org, yes bool
	cmd := &cobra.Command{
		Use:   "revoke-all",
		Short: "End all your trusts, or all the organisation's with --org",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			what := "all your active trusts"
			path := sessionGrantsAPI + "/revoke-all"
			if org {
				what = "every active trust in the organisation"
				path = withQuery(path, map[string]string{"scope": "org"})
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("End %s?", what)); err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				Revoked int `json:"revoked"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, path, map[string]any{}, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Ended %d trust(s). See what is left with `%s`.\n", res.Revoked, brand.Cmd("approvals trusts list"))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	cmd.Flags().BoolVar(&org, "org", false, "Every trust in the organisation (administrators)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "End them without asking")
	return cmd
}
