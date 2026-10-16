package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/keychain"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// principalKinds are the credential kinds the gateway's risk policy knows.
var principalKinds = []string{"browser", "cli_personal", "cli_agent", "mcp_client", "api_key_personal", "api_key_agent", "run"}

// policyPreview mirrors GET /api/admin/risk-policy/preview.
type policyPreview struct {
	Service   string `json:"service"`
	Tool      string `json:"tool"`
	Risk      string `json:"risk"`
	Principal string `json:"principal"`
	Decision  struct {
		Mode         string  `json:"mode"`
		Source       string  `json:"source"`
		RuleID       *string `json:"ruleId"`
		Predicate    *string `json:"predicate"`
		ChangeableBy string  `json:"changeableBy"`
	} `json:"decision"`
}

// PolicyCmd returns `zuano policy`: a read-only view of the organisation's
// risk policy. Changing it is done in the dashboard, where it needs a passkey.
func PolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Explain what the organisation's risk policy does with a tool call",
	}
	cmd.AddCommand(policyExplainCmd())
	return cmd
}

func policyExplainCmd() *cobra.Command {
	var f approvalFlags
	var rawArgs, jsonArgs []string
	var principal string
	cmd := &cobra.Command{
		Use:   "explain <service> <tool>",
		Short: "Show whether a tool call is off, needs approval or runs, and why",
		Long: `Asks the gateway to evaluate the organisation's risk matrix, the tool's
overrides and the platform blocks for one call and prints the result: the mode
(off, approval or allowed), where it comes from and who can change it.
--arg and --arg-json give the call's arguments, for an override whose
threshold depends on them (an amount at most 100, say).

The decision is for the principal that makes the call. By default that is this
session's: cli_agent in an agent session, cli_personal otherwise.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if principal == "" {
				principal = "cli_personal"
				if client.ActiveProfile() == keychain.ProfileAgent {
					principal = "cli_agent"
				}
			}
			if !slices.Contains(principalKinds, principal) {
				return fmt.Errorf("invalid --principal %q: use one of %s", principal, strings.Join(principalKinds, ", "))
			}
			params, err := buildToolArguments(rawArgs, jsonArgs)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(params)
			if err != nil {
				return err
			}
			q := url.Values{"service": {args[0]}, "tool": {args[1]}, "params": {string(encoded)}, "principal": {principal}}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var p policyPreview
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/admin/risk-policy/preview?"+q.Encode(), nil, &p)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			printPolicy(cmd.OutOrStdout(), p)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringArrayVar(&rawArgs, "arg", nil, "Tool argument as key=value; the value is always a string (repeatable)")
	cmd.Flags().StringArrayVar(&jsonArgs, "arg-json", nil, "Tool argument as key=<json>, for numbers, booleans, objects and arrays (repeatable)")
	cmd.Flags().StringVar(&principal, "principal", "", "Evaluate for another principal: "+strings.Join(principalKinds, ", "))
	return cmd
}

var policyModes = map[string]string{"off": "Off", "approval": "Approval required", "allowed": "Allowed"}

var policySources = map[string]string{
	"platform_block":    "a platform block",
	"org_tool_override": "an organisation tool override",
	"org_matrix":        "the organisation's risk matrix",
	"default":           "the built-in default",
}

var policyOwners = map[string]string{"platform": "platform admins", "org_admin": "org admins"}

func printPolicy(out io.Writer, p policyPreview) {
	clean := textsafe.Clean
	call := fmt.Sprintf("%s.%s (%s)", clean(p.Service), clean(p.Tool), clean(p.Risk))
	mode, ok := policyModes[p.Decision.Mode]
	if !ok {
		mode = clean(p.Decision.Mode)
	}
	source, ok := policySources[p.Decision.Source]
	if !ok {
		source = clean(p.Decision.Source)
	}
	fmt.Fprintf(out, "%s is %s for %s\n", call, mode, clean(p.Principal))
	detail := "from " + source
	if p.Decision.RuleID != nil && *p.Decision.RuleID != "" {
		detail += " " + clean(*p.Decision.RuleID)
	}
	if p.Decision.Predicate != nil && *p.Decision.Predicate != "" {
		detail += " when " + clean(*p.Decision.Predicate)
	}
	fmt.Fprintf(out, "  %s\n", detail)
	owner, ok := policyOwners[p.Decision.ChangeableBy]
	if !ok {
		owner = clean(p.Decision.ChangeableBy)
	}
	fmt.Fprintf(out, "  Can be changed by: %s\n", owner)
}
