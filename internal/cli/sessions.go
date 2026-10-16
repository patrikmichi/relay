package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// sessionDoc mirrors one entry of GET /api/me/sessions.
type sessionDoc struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"`
	AuthMethod        string `json:"auth_method"`
	CreatedAt         string `json:"created_at"`
	LastSeenAt        string `json:"last_seen_at"`
	IdleExpiresAt     string `json:"idle_expires_at"`
	AbsoluteExpiresAt string `json:"absolute_expires_at"`
	IPPrefix          string `json:"ip_prefix"`
	UserAgent         string `json:"user_agent"`
	Current           bool   `json:"current"`
}

// SessionsCmd returns `relay sessions`, the person's browser and CLI sessions
// on the gateway.
func SessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List and revoke your browser and CLI sessions on the gateway",
		Long: `The gateway keeps a server-side session for every browser sign-in and every
CLI login. These commands list them, revoke one, or sign you out everywhere.
Signing out everywhere also revokes your CLI and OAuth refresh tokens, this
session included, and needs a passkey confirmation: relay opens the page for it.`,
	}
	cmd.AddCommand(sessionsListCmd(), sessionsRevokeCmd(), sessionsRevokeAllCmd())
	return cmd
}

func sessionsListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your sessions, newest activity first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var list struct {
				Sessions []sessionDoc `json:"sessions"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/me/sessions", nil, &list)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			printSessions(out, list.Sessions)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func printSessions(out io.Writer, sessions []sessionDoc) {
	if len(sessions) == 0 {
		fmt.Fprintln(out, "No sessions")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tSIGNED IN\tLAST SEEN\tIDLE EXPIRY\tFROM")
	for _, s := range sessions {
		id := textsafe.Clean(s.ID)
		if s.Current {
			id += " (this one)"
		}
		from := textsafe.Clean(s.UserAgent)
		if s.IPPrefix != "" {
			from += " " + textsafe.Clean(s.IPPrefix)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", id, textsafe.Clean(s.Kind), textsafe.Clean(s.CreatedAt), textsafe.Clean(s.LastSeenAt), textsafe.Clean(s.IdleExpiresAt), from)
	}
	_ = tw.Flush()
}

func sessionsRevokeCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke one session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodDelete, "/api/me/sessions/"+url.PathEscape(args[0]), nil, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s\n", textsafe.Clean(args[0]))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func sessionsRevokeAllCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "revoke-all",
		Short: "Sign out everywhere: revoke every session, CLI token and OAuth family (needs a passkey confirmation)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodPost, "/api/me/sessions/revoke-all", map[string]any{}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Signed out everywhere. Run `"+brand.Cmd("login")+"` to sign in again.")
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}
