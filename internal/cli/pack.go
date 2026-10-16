package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/packs"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// packApplyWait bounds how long an install is followed.
const packApplyWait = 10 * time.Minute

// PackCmd returns `zuano pack`, the client for starter packs.
func PackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Install starter packs that set your organisation up for one kind of work",
		Long: `A starter pack is a versioned, signed set of connectors, skills, plugin bundles,
rules for coding agents, risk and approval presets and automation templates.

Installing is plan, preview, apply: 'zuano pack plan' shows exactly what would
change, and 'zuano pack install' applies that plan after you confirm it. The
plan is signed by the gateway and checked against the keys pinned for it; a
plan that does not verify exits with status 4 and nothing is applied. Members
receive a pack's rules, skills and bundles on their next 'zuano sync'.

Installing, updating and removing a pack is for a person: it is refused when
ZUANO_AGENT is set (exit 10). An agent uses the gateway's pack tools, which
ask a person to approve. A plan that loosens a control needs a passkey
confirmation (exit 14 when there is no terminal to ask on).

install, update, uninstall and rollback exit status
  0   done
  1   failed
  4   the plan's signature did not verify
  10  refused: an agent session cannot do this
  14  a passkey confirmation is needed and nobody can give it
  15  the plan changed since it was shown: nothing was applied, plan again
  30-33  the plan does not allow it, see 'zuano billing'`,
	}
	cmd.AddCommand(packListCmd(), packShowCmd(), packPlanCmd(), packInstallCmd(), packUpdateCmd(), packUninstallCmd(), packRollbackCmd(), packStatusCmd(), packInitCmd(), packValidateCmd(), packPublishCmd())
	return cmd
}

// refuseAgentPack stops the commands that change an organisation's setup
// when the agent profile is selected.
func refuseAgentPack(what string) error {
	if config.AgentMode() {
		return &ExitError{Code: exitDenied, Msg: fmt.Sprintf("%s changes your organisation's setup and is for a person: it is refused while ZUANO_AGENT is set; an agent uses the gateway's pack tools, which ask a person to approve", what)}
	}
	return nil
}

type packRef struct{ slug, version string }

func parsePackRef(arg string) (packRef, error) {
	slug, version := packs.SplitRef(arg)
	if slug == "" || strings.ContainsAny(slug, "/ ?#") {
		return packRef{}, fmt.Errorf("%q is not a pack name (expected <pack> or <pack>@<version>)", textsafe.Clean(arg))
	}
	return packRef{slug, version}, nil
}

// parseParams reads --param k=v pairs and a --params-file.
func parseParams(pairs []string, file string) (map[string]any, error) {
	params := map[string]any{}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", textsafe.Clean(file), err)
		}
		if err := yaml.Unmarshal(data, &params); err != nil {
			return nil, fmt.Errorf("%s is not a YAML map of parameters: %w", textsafe.Clean(file), err)
		}
	}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--param %q must be key=value", textsafe.Clean(p))
		}
		params[k] = typedValue(v)
	}
	return params, nil
}

func typedValue(v string) any {
	if b, err := strconv.ParseBool(v); err == nil && (v == "true" || v == "false") {
		return b
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}

// parseDecisions combines --keep-conflicts, --take-conflicts and --decide.
func parseDecisions(plan *packs.Plan, keep, take bool, decide []string) (map[string]string, error) {
	if keep && take {
		return nil, errors.New("--keep-conflicts and --take-conflicts cannot be used together")
	}
	out := map[string]string{}
	conflicts := plan.Conflicts()
	for _, seq := range conflicts {
		switch {
		case keep:
			out[strconv.Itoa(seq)] = "keep"
		case take:
			out[strconv.Itoa(seq)] = "take"
		}
	}
	for _, d := range decide {
		seq, choice, ok := strings.Cut(d, "=")
		if _, err := strconv.Atoi(seq); !ok || err != nil || (choice != "keep" && choice != "take") {
			return nil, fmt.Errorf("--decide %q must be <seq>=keep or <seq>=take", textsafe.Clean(d))
		}
		out[seq] = choice
	}
	for _, seq := range conflicts {
		if out[strconv.Itoa(seq)] == "" {
			return nil, fmt.Errorf("operation %d conflicts with a change made since: choose --keep-conflicts, --take-conflicts or --decide %d=keep|take", seq, seq)
		}
	}
	return out, nil
}

// fetchPackPlan asks for a plan and checks its signature before anything
// reads it.
func fetchPackPlan(ctx context.Context, c *client.Client, path string, body any) (*packs.Plan, []byte, error) {
	r, err := gatewayExchange(ctx, c, http.MethodPost, path, body, nil)
	if err != nil {
		return nil, nil, err
	}
	if !r.ok() {
		return nil, nil, r.failure()
	}
	if err := r.verify(ctx, c); err != nil {
		return nil, nil, err
	}
	var plan packs.Plan
	if err := json.Unmarshal(r.Body, &plan); err != nil || plan.PlanID == "" {
		return nil, nil, fmt.Errorf("the gateway's plan could not be read: %w", errors.Join(err, errors.New("no plan id")))
	}
	return &plan, r.Body, nil
}

func packPath(slug string, rest ...string) string {
	return strings.Join(append([]string{"/api/packs", url.PathEscape(slug)}, rest...), "/")
}

func installsPath(id string, rest ...string) string {
	return strings.Join(append([]string{"/api/packs/installs", url.PathEscape(id)}, rest...), "/")
}

type packInstall struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Channel         string `json:"channel"`
	InstalledAt     string `json:"installedAt"`
	UpdateAvailable string `json:"updateAvailable"`
	Pack            struct {
		Slug    string `json:"slug"`
		Version string `json:"version"`
	} `json:"pack"`
	Ops []struct {
		ApplySeq int               `json:"applySeq"`
		Seq      int               `json:"seq"`
		Kind     string            `json:"kind"`
		Target   map[string]string `json:"target"`
		Status   string            `json:"status"`
		Reason   string            `json:"reason"`
	} `json:"ops"`
}

func listPackInstalls(ctx context.Context, c *client.Client) ([]packInstall, []byte, error) {
	var doc struct {
		Installs []packInstall `json:"installs"`
	}
	raw, err := gatewayJSON(ctx, c, http.MethodGet, "/api/packs/installs", nil, &doc)
	return doc.Installs, raw, err
}

// installOf finds the active install of a pack.
func installOf(ctx context.Context, c *client.Client, slug string) (packInstall, error) {
	installs, _, err := listPackInstalls(ctx, c)
	if err != nil {
		return packInstall{}, err
	}
	for _, in := range installs {
		if in.Pack.Slug == slug && in.Status != "uninstalled" {
			return in, nil
		}
	}
	return packInstall{}, fmt.Errorf("%s is not installed; see `%s`", textsafe.Clean(slug), brand.Cmd("pack status"))
}

func packListCmd() *cobra.Command {
	var f approvalFlags
	var category string
	var installed bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the starter packs you can install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := url.Values{}
			if category != "" {
				q.Set("category", category)
			}
			if installed {
				q.Set("source", "installed")
			}
			path := "/api/packs"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var doc struct {
				Packs []struct {
					Slug        string `json:"slug"`
					DisplayName string `json:"displayName"`
					Version     string `json:"version"`
					Category    string `json:"category"`
					Source      string `json:"source"`
					Summary     string `json:"summary"`
					Installed   bool   `json:"installed"`
					Fit         string `json:"fit"`
				} `json:"packs"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Packs) == 0 {
				fmt.Fprintln(out, "No packs.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PACK\tVERSION\tCATEGORY\tSOURCE\tSTATE\tSUMMARY")
			for _, p := range doc.Packs {
				state := orDash(textsafe.Clean(p.Fit))
				if p.Installed {
					state = "installed"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(p.Slug), textsafe.Clean(p.Version), orDash(textsafe.Clean(p.Category)), textsafe.Clean(p.Source), state, textsafe.Clean(p.Summary))
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().StringVar(&category, "category", "", "Only packs in this category")
	cmd.Flags().BoolVar(&installed, "installed", false, "Only packs installed in your organisation")
	return cmd
}

func packShowCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "show <pack>[@version]",
		Short: "Show what a pack contains and changes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parsePackRef(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			path := packPath(ref.slug)
			if ref.version != "" {
				path += "?version=" + url.QueryEscape(ref.version)
			}
			var doc struct {
				Pack struct {
					Slug        string   `json:"slug"`
					DisplayName string   `json:"displayName"`
					Version     string   `json:"version"`
					Summary     string   `json:"summary"`
					Category    string   `json:"category"`
					Versions    []string `json:"versions"`
					Connectors  []struct {
						Slug     string `json:"slug"`
						Required bool   `json:"required"`
						Scope    string `json:"credentialScope"`
						Why      string `json:"why"`
					} `json:"connectors"`
					Skills  []string `json:"skills"`
					Bundles []string `json:"bundles"`
					Rules   []string `json:"rules"`
					Readme  string   `json:"readme"`
				} `json:"pack"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			p := doc.Pack
			fmt.Fprintf(out, "%s %s (%s)\n%s\n", textsafe.Clean(p.DisplayName), textsafe.Clean(p.Version), textsafe.Clean(p.Slug), textsafe.Clean(p.Summary))
			if len(p.Versions) > 0 {
				fmt.Fprintf(out, "Versions: %s\n", textsafe.Clean(strings.Join(p.Versions, ", ")))
			}
			for _, cn := range p.Connectors {
				need := "optional"
				if cn.Required {
					need = "required"
				}
				fmt.Fprintf(out, "  connector %s (%s, %s): %s\n", textsafe.Clean(cn.Slug), need, orDash(textsafe.Clean(cn.Scope)), textsafe.Clean(cn.Why))
			}
			for _, group := range []struct {
				label string
				items []string
			}{{"skills", p.Skills}, {"bundles", p.Bundles}, {"rules", p.Rules}} {
				if len(group.items) > 0 {
					fmt.Fprintf(out, "  %s: %s\n", group.label, textsafe.Clean(strings.Join(group.items, ", ")))
				}
			}
			fmt.Fprintf(out, "\nSee exactly what it would change here with `%s`.\n", brand.Cmd("pack plan "+p.Slug))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

type planFlags struct {
	approvalFlags
	params     []string
	paramsFile string
}

func (f *planFlags) bind(cmd *cobra.Command, jsonHelp string) {
	f.approvalFlags.bind(cmd, jsonHelp)
	cmd.Flags().StringArrayVar(&f.params, "param", nil, "Pack parameter as key=value; repeatable")
	cmd.Flags().StringVar(&f.paramsFile, "params-file", "", "YAML file of pack parameters")
}

func packPlanCmd() *cobra.Command {
	var f planFlags
	cmd := &cobra.Command{
		Use:   "plan <pack>[@version]",
		Short: "Show what installing a pack would change, without changing anything",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parsePackRef(args[0])
			if err != nil {
				return err
			}
			params, err := parseParams(f.params, f.paramsFile)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			plan, raw, err := fetchPackPlan(cmd.Context(), c, packPath(ref.slug, "plans"), map[string]any{"version": ref.version, "params": params, "mode": "install"})
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			plan.Render(cmd.OutOrStdout())
			return nil
		},
	}
	f.bind(cmd, "Print the signed plan unchanged")
	return cmd
}

type applyFlags struct {
	planFlags
	keep, take, yes, noConnect bool
	decide                     []string
}

func (f *applyFlags) bind(cmd *cobra.Command, withParams bool) {
	if withParams {
		f.planFlags.bind(cmd, "Print the result as JSON")
	} else {
		f.approvalFlags.bind(cmd, "Print the result as JSON")
	}
	cmd.Flags().BoolVar(&f.keep, "keep-conflicts", false, "Keep your value for every conflict")
	cmd.Flags().BoolVar(&f.take, "take-conflicts", false, "Take the pack's value for every conflict")
	cmd.Flags().StringArrayVar(&f.decide, "decide", nil, "Decide one conflict as <seq>=keep or <seq>=take; repeatable")
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "Apply without asking")
	cmd.Flags().BoolVar(&f.noConnect, "no-connect", false, "Do not walk through the connections the pack needs")
}

// applyPackPlan shows a verified plan, asks, applies it, follows the job and
// walks the connections that are still missing.
func applyPackPlan(cmd *cobra.Command, c *client.Client, plan *packs.Plan, raw []byte, f applyFlags) error {
	out := cmd.OutOrStdout()
	if f.jsonOut {
		printRaw(out, raw)
	} else {
		plan.Render(out)
	}
	if plan.Summary.Apply == 0 && plan.Summary.Conflict == 0 {
		if !f.jsonOut {
			fmt.Fprintln(out, "\nNothing to change.")
		}
		return nil
	}
	decisions, err := parseDecisions(plan, f.keep, f.take, f.decide)
	if err != nil {
		return err
	}
	question := "\nApply this to your organisation?"
	if plan.Loosens() {
		question = "\nThis loosens a control and needs a passkey confirmation. Apply it to your organisation?"
	}
	if err := confirmOrYes(cmd, f.yes, question); err != nil {
		return err
	}
	r, err := gatewayExchange(cmd.Context(), c, http.MethodPost, "/api/packs/plans/"+url.PathEscape(plan.PlanID)+"/apply", map[string]any{"planHash": plan.PlanHash, "decisions": decisions}, nil)
	if err != nil {
		return err
	}
	if r.Status == http.StatusConflict && r.code() == "plan_stale" {
		return &ExitError{Code: exitPlanStale, Msg: fmt.Sprintf("the organisation changed since this plan was made; nothing was applied. Run `%s` again", brand.Cmd("pack plan "+plan.Pack.Slug))}
	}
	if !r.ok() {
		return r.failure()
	}
	var res struct {
		InstallID string `json:"installId"`
		JobID     string `json:"jobId"`
	}
	if err := json.Unmarshal(r.Body, &res); err != nil || res.JobID == "" {
		return errors.New("the gateway did not say which job applies the plan")
	}
	if !f.jsonOut {
		fmt.Fprintf(out, "Applying (install %s)...\n", textsafe.Clean(res.InstallID))
	}
	jobOut := io.Writer(io.Discard)
	if f.jsonOut {
		jobOut = out
	}
	if err := waitForJob(cmd.Context(), c, res.JobID, packApplyWait, jobOut, f.jsonOut); err != nil {
		return fmt.Errorf("the pack was not fully applied; see `%s` and undo it with `%s`: %w", brand.Cmd("pack status "+plan.Pack.Slug), brand.Cmd("pack uninstall "+plan.Pack.Slug), err)
	}
	if f.jsonOut {
		return nil
	}
	fmt.Fprintln(out, "Done.")
	return walkConnections(cmd, plan.CredentialPrompts, f)
}

// walkConnections lists the connections the organisation still needs and, at a
// terminal, offers to make each one.
func walkConnections(cmd *cobra.Command, prompts []packs.CredentialPrompt, f applyFlags) error {
	out := cmd.OutOrStdout()
	var missing []packs.CredentialPrompt
	for _, p := range prompts {
		if p.Status != "connected" {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	fmt.Fprintln(out, "\nStill to connect:")
	for _, p := range missing {
		who := "your organisation"
		if p.Scope == "personal" {
			who = "each member"
		}
		fmt.Fprintf(out, "  %s (%s): `%s`\n", textsafe.Clean(p.Slug), who, textsafe.Clean(p.CLICommand))
	}
	if f.noConnect || !canPrompt(cmd.InOrStdin()) {
		return nil
	}
	for _, p := range missing {
		if p.Scope != "org" || !askDefaultNo(cmd.InOrStdin(), out, fmt.Sprintf("Connect %s now? [y/N] ", textsafe.Clean(p.Slug))) {
			continue
		}
		if err := connectConnector(cmd, p.Slug, f.gatewayURL); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not connect %s: %v\n", textsafe.Clean(p.Slug), err)
		}
	}
	return nil
}

func packInstallCmd() *cobra.Command {
	var f applyFlags
	cmd := &cobra.Command{
		Use:   "install <pack>[@version]",
		Short: "Install a starter pack after showing what it changes",
		Example: `  zuano pack install solo-developer
  zuano pack install finance-invoicing --param currency=EUR --param approval_threshold=1000 --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentPack(brand.Cmd("pack install")); err != nil {
				return err
			}
			ref, err := parsePackRef(args[0])
			if err != nil {
				return err
			}
			params, err := parseParams(f.params, f.paramsFile)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			plan, raw, err := fetchPackPlan(cmd.Context(), c, packPath(ref.slug, "plans"), map[string]any{"version": ref.version, "params": params, "mode": "install"})
			if err != nil {
				return err
			}
			return applyPackPlan(cmd, c, plan, raw, f)
		},
	}
	f.bind(cmd, true)
	return cmd
}

func packUpdateCmd() *cobra.Command {
	var f applyFlags
	var toVersion string
	cmd := &cobra.Command{
		Use:   "update <pack>",
		Short: "Update an installed pack, keeping the changes you made since",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentPack(brand.Cmd("pack update")); err != nil {
				return err
			}
			ref, err := parsePackRef(args[0])
			if err != nil {
				return err
			}
			params, err := parseParams(f.params, f.paramsFile)
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			plan, raw, err := fetchPackPlan(cmd.Context(), c, packPath(ref.slug, "plans"), map[string]any{"version": toVersion, "params": params, "mode": "update"})
			if err != nil {
				return err
			}
			return applyPackPlan(cmd, c, plan, raw, f)
		},
	}
	f.bind(cmd, true)
	cmd.Flags().StringVar(&toVersion, "to-version", "", "Version to update to (default: the latest on the install's channel)")
	return cmd
}

func packUninstallCmd() *cobra.Command {
	var f applyFlags
	var disableConnectors bool
	cmd := &cobra.Command{
		Use:   "uninstall <pack>",
		Short: "Remove what a pack set up; credentials are never deleted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentPack(brand.Cmd("pack uninstall")); err != nil {
				return err
			}
			ref, err := parsePackRef(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			in, err := installOf(cmd.Context(), c, ref.slug)
			if err != nil {
				return err
			}
			plan, raw, err := fetchPackPlan(cmd.Context(), c, installsPath(in.ID, "plans"), map[string]any{"mode": "uninstall", "disableConnectors": disableConnectors})
			if err != nil {
				return err
			}
			return applyPackPlan(cmd, c, plan, raw, f)
		},
	}
	f.bind(cmd, false)
	cmd.Flags().BoolVar(&disableConnectors, "disable-connectors", false, "Also disable the connectors the pack enabled")
	return cmd
}

func packRollbackCmd() *cobra.Command {
	var f applyFlags
	var to string
	cmd := &cobra.Command{
		Use:   "rollback <pack> --to <version>",
		Short: "Go back to an earlier version of an installed pack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentPack(brand.Cmd("pack rollback")); err != nil {
				return err
			}
			if to == "" {
				return errors.New("--to <version> is required")
			}
			ref, err := parsePackRef(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			in, err := installOf(cmd.Context(), c, ref.slug)
			if err != nil {
				return err
			}
			plan, raw, err := fetchPackPlan(cmd.Context(), c, installsPath(in.ID, "plans"), map[string]any{"mode": "rollback", "toVersion": to})
			if err != nil {
				return err
			}
			return applyPackPlan(cmd, c, plan, raw, f)
		},
	}
	f.bind(cmd, false)
	cmd.Flags().StringVar(&to, "to", "", "Version to go back to")
	return cmd
}

func packStatusCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "status [<pack>]",
		Short: "Show installed packs, or one install with the outcome of each change",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				installs, raw, err := listPackInstalls(cmd.Context(), c)
				if err != nil {
					return err
				}
				if f.jsonOut {
					printRaw(out, raw)
					return nil
				}
				if len(installs) == 0 {
					fmt.Fprintln(out, "No packs installed.")
					return nil
				}
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "PACK\tVERSION\tSTATUS\tCHANNEL\tUPDATE")
				for _, in := range installs {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(in.Pack.Slug), textsafe.Clean(in.Pack.Version), textsafe.Clean(in.Status), textsafe.Clean(in.Channel), orDash(textsafe.Clean(in.UpdateAvailable)))
				}
				return tw.Flush()
			}
			ref, err := parsePackRef(args[0])
			if err != nil {
				return err
			}
			in, err := installOf(cmd.Context(), c, ref.slug)
			if err != nil {
				return err
			}
			var doc struct {
				Install packInstall `json:"install"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, installsPath(in.ID), nil, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			d := doc.Install
			fmt.Fprintf(out, "%s %s: %s\n", textsafe.Clean(d.Pack.Slug), textsafe.Clean(d.Pack.Version), textsafe.Clean(d.Status))
			for _, op := range d.Ops {
				fmt.Fprintf(out, "  %-14s %s %s", textsafe.Clean(op.Status), textsafe.Clean(op.Kind), packs.TargetText(op.Target))
				if op.Reason != "" {
					fmt.Fprintf(out, "  (%s)", textsafe.Clean(op.Reason))
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}
