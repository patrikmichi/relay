package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

var (
	automationIDPattern  = regexp.MustCompile(`^atm_[A-Za-z0-9_-]{4,40}$`)
	automationRunPattern = regexp.MustCompile(`^atr_[A-Za-z0-9_-]{4,40}$`)
	templateSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
)

// AutomationCmd returns `zuano automation`, the client for automation
// templates and the automations installed from them.
func AutomationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "automation",
		Short: "Install, run and follow automations built from templates",
		Long: `An automation is a template (a trigger and steps) installed for your
organisation with your parameters and connections. 'install' creates a draft
and sends you to the page where you test it; turning it on, and the standing
permissions that gives it, are decided in the browser with your passkey.

An id that starts with atm_ is an installed automation, one that starts with
atr_ is one run of it. A template is named by its slug, with an optional
@version.

An agent session may list, show, read logs, install a draft, dry-run and run;
enable, preapprove, uninstall and a major upgrade are refused (exit 14).

run and logs exit status
  0   the run succeeded
  10  a step was denied
  11  a step's approval expired
  12  the run waits for an approval, or --wait ran out
  20  the run failed
  13  rate limited
  14  a person must do this in the browser
  30-33  the plan does not allow it, see 'zuano billing'`,
	}
	cmd.AddCommand(automationListCmd(), automationShowCmd(), automationInstallCmd(), automationRunCmd(), automationLogsCmd(),
		automationEnableCmd(), automationPreapproveCmd(), automationPauseCmd("pause"), automationPauseCmd("disable"),
		automationUpgradeCmd(), automationUninstallCmd(), automationValidateCmd(), automationPublishCmd())
	return cmd
}

// refuseAutomationInAgent stops a command that creates a standing permission:
// that is decided by a person, in the browser.
func refuseAutomationInAgent(what string) error {
	if config.AgentMode() {
		return &ExitError{Code: client.ExitStepUp, Msg: fmt.Sprintf("%s gives an automation standing permissions, which a person decides in the browser with a passkey; it is refused while ZUANO_AGENT is set. Ask the person who runs this machine to run it", what)}
	}
	return nil
}

type preapprovalDoc struct {
	ID             string `json:"id"`
	StepPath       string `json:"stepPath"`
	Status         string `json:"status"`
	VoidReason     string `json:"voidReason"`
	ExpiresAt      string `json:"expiresAt"`
	Approver       string `json:"approver"`
	CallsToday     int    `json:"callsToday"`
	MaxCallsPerDay int    `json:"maxCallsPerDay"`
}

type automationDoc struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	PausedReason string `json:"pausedReason"`
	AutoUpdate   string `json:"autoUpdate"`
	Template     struct {
		Slug    string `json:"slug"`
		Version string `json:"version"`
	} `json:"template"`
	LastRun *struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		StartedAt string `json:"startedAt"`
	} `json:"lastRun"`
	ReapprovalNeeded int `json:"reapprovalNeeded"`
}

func automationID(arg string) (string, error) {
	if !automationIDPattern.MatchString(arg) {
		return "", fmt.Errorf("%q is not an automation id (expected atm_...)", textsafe.Clean(arg))
	}
	return arg, nil
}

func automationPath(id string, rest ...string) string {
	return strings.Join(append([]string{"/api/automations", url.PathEscape(id)}, rest...), "/")
}

func automationListCmd() *cobra.Command {
	var f approvalFlags
	var gallery bool
	var category, service string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your organisation's automations, or the templates you can install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if gallery {
				return listTemplates(cmd.Context(), c, out, f.jsonOut, category, service)
			}
			var doc struct {
				Automations []automationDoc `json:"automations"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/automations", nil, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Automations) == 0 {
				fmt.Fprintf(out, "No automations installed. Browse templates with `%s`.\n", brand.Cmd("automation list --gallery"))
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tTEMPLATE\tSTATUS\tLAST RUN\tNOTE")
			for _, a := range doc.Automations {
				last, note := "-", ""
				if a.LastRun != nil {
					last = textsafe.Clean(a.LastRun.Status)
				}
				if a.PausedReason != "" {
					note = "paused: " + textsafe.Clean(a.PausedReason)
				}
				if a.ReapprovalNeeded > 0 {
					note = strings.TrimSpace(note + fmt.Sprintf(" %d step(s) need re-approval", a.ReapprovalNeeded))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s@%s\t%s\t%s\t%s\n", textsafe.Clean(a.ID), textsafe.Clean(a.Name), textsafe.Clean(a.Template.Slug), textsafe.Clean(a.Template.Version), textsafe.Clean(a.Status), last, note)
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&gallery, "gallery", false, "List templates you can install instead of installed automations")
	cmd.Flags().StringVar(&category, "category", "", "With --gallery: only this category")
	cmd.Flags().StringVar(&service, "service", "", "With --gallery: only templates that use this service")
	return cmd
}

type templateDoc struct {
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Category string `json:"category"`
	Version  string `json:"version"`
	Ready    *bool  `json:"ready"`
}

func listTemplates(ctx context.Context, c *client.Client, out io.Writer, jsonOut bool, category, service string) error {
	q := url.Values{}
	if category != "" {
		q.Set("category", category)
	}
	if service != "" {
		q.Set("service", service)
	}
	path := "/api/automations/templates"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var doc struct {
		Templates []templateDoc `json:"templates"`
	}
	raw, err := gatewayJSON(ctx, c, http.MethodGet, path, nil, &doc)
	if err != nil {
		return err
	}
	if jsonOut {
		printRaw(out, raw)
		return nil
	}
	if len(doc.Templates) == 0 {
		fmt.Fprintln(out, "No templates.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TEMPLATE\tVERSION\tCATEGORY\tREADY\tSUMMARY")
	for _, t := range doc.Templates {
		ready := "-"
		if t.Ready != nil {
			ready = map[bool]string{true: "yes", false: "needs connections"}[*t.Ready]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(t.Slug), textsafe.Clean(t.Version), orDash(textsafe.Clean(t.Category)), ready, textsafe.Clean(t.Summary))
	}
	return tw.Flush()
}

func automationShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <atm_id|template[@version]>",
		Short: "Show an automation with its pre-approvals, or a template with the permissions it needs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if strings.HasPrefix(args[0], "atm_") {
				return showInstalledAutomation(cmd, c, args[0], f.jsonOut)
			}
			slug, version := splitAtVersion(args[0])
			if !templateSlugPattern.MatchString(slug) {
				return fmt.Errorf("%q is neither an automation id (atm_...) nor a template slug", textsafe.Clean(args[0]))
			}
			path := "/api/automations/templates/" + url.PathEscape(slug)
			if version != "" {
				path += "/versions/" + url.PathEscape(version)
			}
			var doc struct {
				Template struct {
					templateDoc
					Permissions []struct {
						Service string `json:"service"`
						Tool    string `json:"tool"`
						Risk    string `json:"risk"`
					} `json:"permissions"`
					Connectors  []string `json:"connectors"`
					MinimumPlan string   `json:"minimumPlan"`
					Preapprove  []string `json:"preapprovableSteps"`
				} `json:"template"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			t := doc.Template
			fmt.Fprintf(out, "%s %s (%s)\n%s\n", textsafe.Clean(t.Title), textsafe.Clean(t.Version), textsafe.Clean(t.Slug), textsafe.Clean(t.Summary))
			if len(t.Connectors) > 0 {
				fmt.Fprintf(out, "Connections: %s\n", textsafe.Clean(strings.Join(t.Connectors, ", ")))
			}
			if t.MinimumPlan != "" {
				fmt.Fprintf(out, "Plan: %s or higher\n", textsafe.Clean(t.MinimumPlan))
			}
			for _, p := range t.Permissions {
				fmt.Fprintf(out, "  %-12s %s.%s\n", textsafe.Clean(p.Risk), textsafe.Clean(p.Service), textsafe.Clean(p.Tool))
			}
			if len(t.Preapprove) > 0 {
				fmt.Fprintf(out, "Steps you can pre-approve: %s\n", textsafe.Clean(strings.Join(t.Preapprove, ", ")))
			}
			fmt.Fprintf(out, "\nInstall it with `%s`.\n", brand.Cmd("automation install "+t.Slug))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

// doctorAutomations reports your automations that are paused and the steps
// whose pre-approval ended; a gateway without automations is not a fault.
func doctorAutomations(ctx context.Context, c *client.Client, add func(string, bool, string)) {
	var doc struct {
		Automations []automationDoc `json:"automations"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, "/api/automations?owner=me", nil, &doc); err != nil {
		add("automations", true, "automations are not available from this gateway")
		return
	}
	var paused, reapprove []string
	firstPaused := ""
	for _, a := range doc.Automations {
		if a.Status == "paused" {
			if firstPaused == "" {
				firstPaused = a.ID
			}
			paused = append(paused, fmt.Sprintf("%s (%s)", a.ID, a.PausedReason))
		}
		if a.ReapprovalNeeded > 0 {
			reapprove = append(reapprove, a.ID)
		}
	}
	switch {
	case len(paused) > 0:
		add("automations", false, fmt.Sprintf("paused: %s; see `%s`", textsafe.Clean(strings.Join(paused, ", ")), brand.Cmd("automation show "+firstPaused)))
	case len(reapprove) > 0:
		add("automations", false, fmt.Sprintf("steps need re-approval in: %s; run `%s`", textsafe.Clean(strings.Join(reapprove, ", ")), brand.Cmd("automation preapprove <id>")))
	default:
		add("automations", true, fmt.Sprintf("%d automation(s), none paused", len(doc.Automations)))
	}
}

func splitAtVersion(ref string) (string, string) {
	name, version, _ := strings.Cut(ref, "@")
	return name, version
}

func showInstalledAutomation(cmd *cobra.Command, c *client.Client, id string, jsonOut bool) error {
	if _, err := automationID(id); err != nil {
		return err
	}
	var doc struct {
		Automation automationDoc `json:"automation"`
	}
	raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, automationPath(id), nil, &doc)
	if err != nil {
		return err
	}
	var pre struct {
		Preapprovals []preapprovalDoc `json:"preapprovals"`
	}
	praw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, automationPath(id, "preapprovals"), nil, &pre)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if jsonOut {
		printRaw(out, raw)
		printRaw(out, praw)
		return nil
	}
	a := doc.Automation
	fmt.Fprintf(out, "%s %s (%s)\n  template %s@%s\n", textsafe.Clean(a.ID), textsafe.Clean(a.Name), textsafe.Clean(a.Status), textsafe.Clean(a.Template.Slug), textsafe.Clean(a.Template.Version))
	if a.PausedReason != "" {
		fmt.Fprintf(out, "  paused: %s\n", textsafe.Clean(a.PausedReason))
	}
	if a.AutoUpdate != "" {
		fmt.Fprintf(out, "  auto-update: %s\n", textsafe.Clean(a.AutoUpdate))
	}
	if len(pre.Preapprovals) == 0 {
		fmt.Fprintln(out, "  no pre-approvals")
	}
	for _, p := range pre.Preapprovals {
		line := fmt.Sprintf("  pre-approval %s step %s: %s", textsafe.Clean(p.ID), textsafe.Clean(p.StepPath), textsafe.Clean(p.Status))
		switch {
		case p.Status == "void":
			line += fmt.Sprintf(" (%s); re-approve with `%s`", textsafe.Clean(p.VoidReason), brand.Cmd("automation preapprove "+a.ID+" --step "+p.StepPath))
		case p.MaxCallsPerDay > 0:
			line += fmt.Sprintf(", %d of %d calls today, expires %s", p.CallsToday, p.MaxCallsPerDay, textsafe.Clean(p.ExpiresAt))
		}
		fmt.Fprintln(out, line)
	}
	return nil
}
