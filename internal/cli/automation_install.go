package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

func automationInstallCmd() *cobra.Command {
	var f planFlags
	var name, runAs string
	var binds []string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "install <template>[@version]",
		Short: "Install a template as a draft and open the page where you test and enable it",
		Long: `Creates a draft automation from a template. A draft does nothing until a
person tests it and turns it on in the browser: that page also asks for the
passkey that gives it its standing permissions and for the pre-approvals of its
outward steps. zuano opens the page, or prints the link and a QR code for your
phone when there is no browser.

--bind service=org|personal chooses whose connection a service uses.`,
		Example: `  zuano automation install fakturoid-invoice-check-slack --param channel=C0123 --bind slack=org`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, version := splitAtVersion(args[0])
			if !templateSlugPattern.MatchString(slug) {
				return fmt.Errorf("%q is not a template slug", textsafe.Clean(args[0]))
			}
			params, err := parseParams(f.params, f.paramsFile)
			if err != nil {
				return err
			}
			bindings := map[string]string{}
			for _, b := range binds {
				svc, scope, ok := strings.Cut(b, "=")
				if !ok || svc == "" || (scope != "org" && scope != "personal") {
					return fmt.Errorf("--bind %q must be service=org or service=personal", textsafe.Clean(b))
				}
				bindings[svc] = scope
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			body := map[string]any{"template": slug, "version": version, "name": name, "params": params, "bindings": bindings}
			if runAs != "" {
				body["runAsEmail"] = runAs
			}
			var res struct {
				Automation automationDoc `json:"automation"`
				WizardURL  string        `json:"wizardUrl"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/automations", body, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if res.Automation.ID == "" || res.WizardURL == "" {
				return errors.New("the gateway did not return the draft or its page")
			}
			fmt.Fprintf(out, "Created draft %s (%s).\n", textsafe.Clean(res.Automation.ID), textsafe.Clean(res.Automation.Name))
			link, err := gatewayLink(c.GatewayURL, res.WizardURL)
			if err != nil {
				return err
			}
			showLink(cmd, link, noBrowser, "Test it and turn it on there")
			fmt.Fprintf(out, "Dry-run it meanwhile with `%s`.\n", brand.Cmd("automation run "+res.Automation.ID+" --dry-run"))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&name, "name", "", "Name of the automation (default: the template's title)")
	cmd.Flags().StringVar(&runAs, "run-as", "", "Run as this member (an administrator's choice; default: you)")
	cmd.Flags().StringArrayVar(&binds, "bind", nil, "Whose connection a service uses, as service=org|personal; repeatable")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}
