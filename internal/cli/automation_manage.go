package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// openAutomationPage sends the person to a browser page of the gateway;
// everything that gives an automation standing permissions is decided there.
func openAutomationPage(cmd *cobra.Command, gatewayURL, path string, noBrowser bool, hint string) error {
	c, err := approvalsClient(gatewayURL)
	if err != nil {
		return err
	}
	link, err := gatewayLink(c.GatewayURL, path)
	if err != nil {
		return err
	}
	showLink(cmd, link, noBrowser, hint)
	return nil
}

func automationEnableCmd() *cobra.Command {
	var f approvalFlags
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "enable <atm_id>",
		Short: "Open the page where you turn an automation on with your passkey",
		Long:  `Turning an automation on gives it standing permissions, so it is decided in the browser or on your phone with your passkey. zuano only opens the page.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAutomationInAgent(brand.Cmd("automation enable")); err != nil {
				return err
			}
			id, err := automationID(args[0])
			if err != nil {
				return err
			}
			return openAutomationPage(cmd, f.gatewayURL, "/automations/"+url.PathEscape(id)+"/enable", noBrowser, "Turn it on there")
		},
	}
	f.bind(cmd, "Unused: the command only opens a page")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}

func automationPreapproveCmd() *cobra.Command {
	var f approvalFlags
	var noBrowser bool
	var stepPath string
	cmd := &cobra.Command{
		Use:   "preapprove <atm_id> [--step path]",
		Short: "Open the page where you pre-approve an outward step with your passkey",
		Long: `A pre-approval lets one outward step (such as a Slack message) run without an
approval each time, for a pinned target, a daily cap and at most 30 days. It is
signed with your passkey in the browser; zuano only opens the page.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAutomationInAgent(brand.Cmd("automation preapprove")); err != nil {
				return err
			}
			id, err := automationID(args[0])
			if err != nil {
				return err
			}
			path := "/automations/" + url.PathEscape(id) + "/preapprove"
			if stepPath != "" {
				path += "?step=" + url.QueryEscape(stepPath)
			}
			return openAutomationPage(cmd, f.gatewayURL, path, noBrowser, "Pre-approve it there")
		},
	}
	f.bind(cmd, "Unused: the command only opens a page")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	cmd.Flags().StringVar(&stepPath, "step", "", "The step to pre-approve (default: all that can be)")
	return cmd
}

func automationPauseCmd(verb string) *cobra.Command {
	var f approvalFlags
	short := map[string]string{
		"pause":   "Pause an automation: its triggers stay, no runs start",
		"disable": "Turn an automation off and disable its triggers",
	}[verb]
	cmd := &cobra.Command{
		Use:   verb + " <atm_id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := automationID(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, automationPath(id, verb), map[string]any{}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %sd.\n", textsafe.Clean(id), verb)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

type upgradeDiff struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Class   string   `json:"class"`
	Added   []string `json:"permissionsAdded"`
	Removed []string `json:"permissionsRemoved"`
	Connect []string `json:"connectorsAdded"`
	Voided  []string `json:"preapprovalsVoided"`
	Changed []string `json:"stepsChanged"`
}

func automationUpgradeCmd() *cobra.Command {
	var f approvalFlags
	var to string
	var yes, noBrowser bool
	cmd := &cobra.Command{
		Use:   "upgrade <atm_id>",
		Short: "Show what a newer template version changes, and apply it",
		Long: `Prints what the new version adds or removes: permissions with their risk,
connections, changed steps, and the pre-approvals it voids (every active one, since
the manifest changes). A patch or minor upgrade is applied after you confirm. A
major upgrade adds permissions, so it is confirmed in the browser with your passkey.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := automationID(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := automationPath(id, "upgrade")
			if to != "" {
				path += "?to=" + url.QueryEscape(to)
			}
			var doc struct {
				Diff upgradeDiff `json:"diff"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			d := doc.Diff
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
			} else {
				printUpgrade(out, d)
			}
			if d.To == "" {
				return nil
			}
			if d.Class == "major" {
				if err := refuseAutomationInAgent(brand.Cmd("automation upgrade") + " to a major version"); err != nil {
					return err
				}
				return openAutomationPage(cmd, f.gatewayURL, "/automations/"+url.PathEscape(id)+"/upgrade?to="+url.QueryEscape(d.To), noBrowser, "Confirm it there")
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("\nUpgrade to %s?", textsafe.Clean(d.To))); err != nil {
				return err
			}
			_, err = gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, path, map[string]any{"to": d.To}, nil)
			if err == nil && !f.jsonOut {
				fmt.Fprintf(out, "Upgraded to %s.\n", textsafe.Clean(d.To))
				if len(d.Voided) > 0 {
					fmt.Fprintf(out, "Outward steps ask for approval each run until re-approved: `%s`.\n", brand.Cmd("automation preapprove "+id))
				}
			}
			return err
		},
	}
	f.bind(cmd, "Print the diff unchanged")
	cmd.Flags().StringVar(&to, "to", "", "Version to upgrade or roll back to (default: the latest)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Apply a patch or minor upgrade without asking")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "With a major upgrade: print the link instead of opening a browser")
	return cmd
}

func printUpgrade(out io.Writer, d upgradeDiff) {
	if d.To == "" {
		fmt.Fprintln(out, "Already on the latest version.")
		return
	}
	fmt.Fprintf(out, "%s -> %s (%s)\n", textsafe.Clean(d.From), textsafe.Clean(d.To), textsafe.Clean(d.Class))
	for _, g := range []struct {
		label string
		items []string
	}{{"permissions added", d.Added}, {"permissions removed", d.Removed}, {"connections added", d.Connect}, {"steps changed", d.Changed}, {"pre-approvals voided", d.Voided}} {
		if len(g.items) > 0 {
			fmt.Fprintf(out, "  %s: %s\n", g.label, textsafe.Clean(strings.Join(g.items, ", ")))
		}
	}
}

func automationUninstallCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "uninstall <atm_id>",
		Short: "Remove an automation, its triggers, grants and pre-approvals",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAutomationInAgent(brand.Cmd("automation uninstall")); err != nil {
				return err
			}
			id, err := automationID(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Uninstall %s? Its triggers stop and its grants and pre-approvals end.", textsafe.Clean(id))); err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodDelete, automationPath(id), nil, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Uninstalled %s.\n", textsafe.Clean(id))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Uninstall without asking")
	return cmd
}
