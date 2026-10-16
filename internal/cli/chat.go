package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

var chatProviders = map[string]string{"slack": "Slack", "teams": "Microsoft Teams"}

type chatInstallation struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Workspace string `json:"workspace"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
}

type chatIdentity struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	DMOptOut  bool   `json:"dmOptOut"`
}

type chatReachability struct {
	Eligible  int `json:"eligible"`
	Reachable int `json:"reachable"`
}

// ChatCmd returns `zuano chat`, the client for approvals in Slack and Teams.
func ChatCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "See and link the chat accounts approvals are sent to",
		Long: `Your organisation can send approval requests to Slack or Microsoft Teams. A
card there never decides anything: its Review button opens the approval page
where you confirm with your passkey. These commands show what is set up and
open the page that links your chat account to your ` + brand.Product + ` account.`,
	}
	cmd.AddCommand(chatStatusCmd(), chatLinkCmd())
	return cmd
}

// fetchChatState reads the installations and reachability (administrators
// only, so a refusal is left out) and your own links.
func fetchChatState(ctx context.Context, c *client.Client) (insts []chatInstallation, ids []chatIdentity, reach *chatReachability, err error) {
	var ident struct {
		Identities []chatIdentity `json:"identities"`
	}
	if _, err = gatewayJSON(ctx, c, http.MethodGet, "/api/me/chat-identities", nil, &ident); err != nil {
		return nil, nil, nil, err
	}
	var inst struct {
		Installations []chatInstallation `json:"installations"`
	}
	if _, ierr := gatewayJSON(ctx, c, http.MethodGet, "/api/admin/chat/installations", nil, &inst); ierr == nil {
		insts = inst.Installations
		var r chatReachability
		if _, rerr := gatewayJSON(ctx, c, http.MethodGet, "/api/admin/chat/reachability", nil, &r); rerr == nil {
			reach = &r
		}
	}
	return insts, ident.Identities, reach, nil
}

func chatStatusCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the chat apps set up for your organisation and your own links",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			insts, ids, reach, err := fetchChatState(cmd.Context(), c)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return json.NewEncoder(out).Encode(map[string]any{"installations": insts, "identities": ids, "reachability": reach})
			}
			if len(insts) == 0 {
				fmt.Fprintln(out, "No chat app is set up (or you are not an administrator and cannot see them).")
			} else {
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "APP\tWORKSPACE\tSTATUS\tDETAIL")
				for _, i := range insts {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", chatName(i.Provider), textsafe.Clean(i.Workspace), textsafe.Clean(i.Status), orDash(textsafe.Clean(i.Detail)))
				}
				_ = tw.Flush()
			}
			if reach != nil {
				fmt.Fprintf(out, "%d of %d approvers can be reached in chat.\n", reach.Reachable, reach.Eligible)
			}
			if len(ids) == 0 {
				fmt.Fprintf(out, "You have not linked a chat account. Link one with `%s`.\n", brand.Cmd("chat link slack"))
			}
			for _, id := range ids {
				note := ""
				if id.DMOptOut {
					note = " (direct messages off)"
				}
				fmt.Fprintf(out, "Linked: %s as %s in %s%s\n", chatName(id.Provider), textsafe.Clean(id.Name), textsafe.Clean(id.Workspace), note)
			}
			return nil
		},
	}
	f.bind(cmd, "Print installations, your links and reachability as JSON")
	return cmd
}

func chatName(provider string) string {
	if n, ok := chatProviders[provider]; ok {
		return n
	}
	return textsafe.Clean(provider)
}

func chatLinkCmd() *cobra.Command {
	var f approvalFlags
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "link slack|teams",
		Short: "Open the page that links your Slack or Teams account to your account",
		Long: `Linking proves you own both accounts: you sign in to the chat app and then
confirm with your passkey on the gateway's page. zuano only opens the page, or
prints the link and a QR code for your phone.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"slack", "teams"},
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := args[0]
			if _, ok := chatProviders[provider]; !ok {
				return fmt.Errorf("unknown chat app %q: use slack or teams", textsafe.Clean(provider))
			}
			if config.AgentMode() {
				return &ExitError{Code: client.ExitStepUp, Msg: "linking a chat account is confirmed by a person with a passkey; it is refused while ZUANO_AGENT is set"}
			}
			return openAutomationPage(cmd, f.gatewayURL, "/api/chat/link/"+provider+"/start", noBrowser, "Link your "+chatName(provider)+" account there")
		},
	}
	f.bind(cmd, "Unused: the command only opens a page")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}

// doctorChat reports where you can be reached for approvals; a gateway
// without chat apps is not a fault.
func doctorChat(ctx context.Context, c *client.Client, add func(string, bool, string)) {
	insts, ids, _, err := fetchChatState(ctx, c)
	if err != nil {
		add("approvals-chat", true, "chat approvals are not available from this gateway")
		return
	}
	if len(insts) == 0 {
		add("approvals-chat", true, "no chat app installed")
		return
	}
	linked := map[string]bool{}
	for _, id := range ids {
		linked[id.Provider] = true
	}
	var parts []string
	seen := map[string]bool{}
	for _, i := range insts {
		if seen[i.Provider] {
			continue
		}
		seen[i.Provider] = true
		state := "not linked"
		if linked[i.Provider] {
			state = "linked"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", chatName(i.Provider), state))
	}
	add("approvals-chat", true, "approvals reachable in: "+strings.Join(parts, ", "))
}
