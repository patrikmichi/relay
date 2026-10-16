package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/policyfile"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// Exit statuses of the policy commands.
const (
	exitPolicyInvalid  = 40
	exitPolicyConflict = 41
	exitPolicyManaged  = 42
	exitPlanChanges    = 2
)

var policyPollDelay = 3 * time.Second

func refuseAgentPolicy(what string) error {
	if config.AgentMode() {
		return &ExitError{Code: exitDenied, Msg: fmt.Sprintf("%s changes the organisation's policy and is for a person or a CI job: it is refused while ZUANO_AGENT is set", what)}
	}
	return nil
}

type policyDirFlag struct{ dir string }

func (f *policyDirFlag) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.dir, "dir", brand.Name, "Directory of policy files")
}

// invalidPolicy reports problems and ends with status 40.
func invalidPolicy(w io.Writer, diags []policyfile.Diag) error {
	for _, d := range diags {
		fmt.Fprintf(w, "  %s\n", textsafe.Clean(d.String()))
	}
	return &ExitError{Code: exitPolicyInvalid, Msg: fmt.Sprintf("the policy is not valid (%d problem(s))", len(diags))}
}

// loadPolicy reads and checks the directory without a gateway.
func loadPolicy(cmd *cobra.Command, dir string) (*policyfile.Doc, error) {
	doc, err := policyfile.Load(dir)
	if err != nil {
		return nil, &ExitError{Code: exitPolicyInvalid, Msg: err.Error()}
	}
	diags, err := doc.Validate()
	if err != nil {
		return nil, err
	}
	if len(diags) > 0 {
		return nil, invalidPolicy(cmd.ErrOrStderr(), diags)
	}
	return doc, nil
}

// policyFailure maps a failed policy request to its exit status.
func policyFailure(w io.Writer, r gatewayReply) error {
	var body struct {
		Error  string            `json:"error"`
		Errors []policyfile.Diag `json:"errors"`
	}
	_ = json.Unmarshal(r.Body, &body)
	switch body.Error {
	case "policy_invalid":
		return invalidPolicy(w, body.Errors)
	case "policy_conflict":
		return &ExitError{Code: exitPolicyConflict, Msg: fmt.Sprintf("the live policy changed since the plan was made: run `%s` again", brand.Cmd("policy plan"))}
	case "policy_managed_in_git":
		return &ExitError{Code: exitPolicyManaged, Msg: "the policy is managed in Git: change it by pull request, or use break-glass in the dashboard"}
	}
	return r.failure()
}

// PolicyCmd returns `zuano policy`.
func PolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Manage the organisation's policy as files, and explain what it does with a call",
		Long: `The organisation's risk policy, grants, groups, approval settings, agent rules
and security settings can be kept as YAML files in a Git repository: reviewed
as a pull request, checked with 'plan' and applied with 'apply'. Every change,
from the dashboard or from a file, becomes a numbered release that can be
rolled back.

In a GitHub Actions or GitLab job the commands sign in with the job's OIDC
token, so no secret is stored; 'init --ci github' or '--ci gitlab' writes the
pipeline.

exit status
  0   done (plan: also when there are changes, unless --detailed-exitcode)
  2   plan --detailed-exitcode: there are changes
  10  refused for an agent session
  12  apply is waiting for an approval; resume with --resume
  14  a passkey confirmation is needed and nobody can give it
  40  the files are not a valid policy
  41  the live policy changed since the plan: plan again
  42  the policy is managed in Git; change it by pull request`,
	}
	cmd.AddCommand(policyExplainCmd(), policyInitCmd(), policyFmtCmd(), policyValidateCmd(), policyExportCmd(), policyPlanCmd(), policyApplyCmd(),
		policyStatusCmd(), policyDiffCmd(), policyReleasesCmd(), policyRollbackCmd(), policyFreezeCmd())
	return cmd
}

func policyInitCmd() *cobra.Command {
	var d policyDirFlag
	var f approvalFlags
	var ci string
	var fromLive bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a policy directory, optionally from the live policy, with a CI pipeline",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			path := filepath.Join(d.dir, "policy.yaml")
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists; nothing was written", textsafe.Clean(path))
			}
			header := ""
			if gw, err := config.GatewayURL(); err == nil && gw != "" && !Offline() {
				header = "# yaml-language-server: $schema=" + strings.TrimRight(gw, "/") + "/schemas/policy/v1.json\n"
			}
			content := header + "version: 1\norg: my-org   # your organisation's slug\n"
			if fromLive {
				c, err := policyClient(cmd.Context(), f.gatewayURL)
				if err != nil {
					return err
				}
				r, err := gatewayExchange(cmd.Context(), c, http.MethodGet, "/api/policy/document?format=yaml", nil, nil)
				if err != nil {
					return err
				}
				if !r.ok() {
					return r.failure()
				}
				content = header + string(r.Body)
			}
			if err := os.MkdirAll(d.dir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(out, "Wrote %s.\n", textsafe.Clean(path))
			if ci != "" {
				name, body, err := policyfile.CI(ci, d.dir)
				if err != nil {
					return err
				}
				if _, err := os.Stat(name); err == nil {
					return fmt.Errorf("%s already exists; left as it is", name)
				}
				if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(out, "Wrote %s. Set the variable %s_URL to the gateway's address.\n", name, envPrefix)
			}
			fmt.Fprintf(out, "Check it with `%s`.\n", brand.Cmd("policy validate"))
			return nil
		},
	}
	d.bind(cmd)
	f.bind(cmd, "Unused")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().StringVar(&ci, "ci", "", "Also write a pipeline that plans on review and applies on merge: github or gitlab")
	cmd.Flags().BoolVar(&fromLive, "from-live", false, "Start from the organisation's current policy")
	return cmd
}

func policyFmtCmd() *cobra.Command {
	var d policyDirFlag
	var check bool
	cmd := &cobra.Command{
		Use:   "fmt",
		Short: "Rewrite the policy files in the canonical layout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := policyfile.Load(d.dir)
			if err != nil {
				return &ExitError{Code: exitPolicyInvalid, Msg: err.Error()}
			}
			var changed []string
			for _, f := range doc.Files {
				formatted, err := policyfile.Format([]byte(f.Content))
				if err != nil {
					return &ExitError{Code: exitPolicyInvalid, Msg: fmt.Sprintf("%s: %v", f.Path, err)}
				}
				if string(formatted) == f.Content {
					continue
				}
				changed = append(changed, f.Path)
				if !check {
					if err := os.WriteFile(filepath.Join(d.dir, filepath.FromSlash(f.Path)), formatted, 0o644); err != nil {
						return err
					}
				}
			}
			out := cmd.OutOrStdout()
			for _, p := range changed {
				fmt.Fprintln(out, textsafe.Clean(p))
			}
			if check && len(changed) > 0 {
				return &ExitError{Code: 1, Msg: fmt.Sprintf("%d file(s) need formatting: run `%s`", len(changed), brand.Cmd("policy fmt"))}
			}
			return nil
		},
	}
	d.bind(cmd)
	cmd.Flags().BoolVar(&check, "check", false, "Only list files that need formatting; change nothing")
	return cmd
}

func policyValidateCmd() *cobra.Command {
	var d policyDirFlag
	var f approvalFlags
	var offline bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check the files against the schema, and with a gateway against the catalog",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := loadPolicy(cmd, d.dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if offline || Offline() {
				fmt.Fprintln(out, "The files match the schema. Run without --offline to check them against the gateway's catalog.")
				return nil
			}
			if gw, err := config.GatewayURL(); err != nil || gw == "" {
				fmt.Fprintln(out, "The files match the schema. No gateway is configured, so references were not checked.")
				return nil
			}
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			r, err := gatewayExchange(cmd.Context(), c, http.MethodPost, "/api/policy/validate", map[string]any{"files": doc.Files}, nil)
			if err != nil {
				return err
			}
			if !r.ok() {
				return policyFailure(cmd.ErrOrStderr(), r)
			}
			fmt.Fprintln(out, "The policy is valid.")
			return nil
		},
	}
	d.bind(cmd)
	f.bind(cmd, "Unused")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVar(&offline, "offline", false, "Check the schema only; never contact the gateway")
	return cmd
}

func policyExportCmd() *cobra.Command {
	var f approvalFlags
	var format string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Print the live policy as a document",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "yaml" && format != "json" {
				return fmt.Errorf("invalid --format %q: use yaml or json", textsafe.Clean(format))
			}
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			r, err := gatewayExchange(cmd.Context(), c, http.MethodGet, "/api/policy/document?format="+format, nil, nil)
			if err != nil {
				return err
			}
			if !r.ok() {
				return r.failure()
			}
			_, err = cmd.OutOrStdout().Write(r.Body)
			return err
		},
	}
	f.bind(cmd, "Unused")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().StringVar(&format, "format", "yaml", "yaml or json")
	return cmd
}

// requestPlan sends the files for a plan and decodes the answer.
func requestPlan(cmd *cobra.Command, c *client.Client, doc *policyfile.Doc) (policyfile.Plan, []byte, error) {
	r, err := gatewayExchange(cmd.Context(), c, http.MethodPost, "/api/policy/plan", map[string]any{"files": doc.Files, "base": "live"}, nil)
	if err != nil {
		return policyfile.Plan{}, nil, err
	}
	if !r.ok() {
		return policyfile.Plan{}, nil, policyFailure(cmd.ErrOrStderr(), r)
	}
	var res struct {
		Plan policyfile.Plan `json:"plan"`
	}
	if err := json.Unmarshal(r.Body, &res); err != nil || res.Plan.ID == "" {
		return policyfile.Plan{}, nil, errors.New("the gateway's plan could not be read")
	}
	return res.Plan, r.Body, nil
}

func policyPlanCmd() *cobra.Command {
	var d policyDirFlag
	var f approvalFlags
	var format, outFile string
	var failLoosening, detailed bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what applying the files would change and who it affects, changing nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "table" && format != "markdown" && format != "json" {
				return fmt.Errorf("invalid --format %q: use table, markdown or json", textsafe.Clean(format))
			}
			doc, err := loadPolicy(cmd, d.dir)
			if err != nil {
				return err
			}
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if errors.Is(err, errNoCICredentials) {
				fmt.Fprintln(cmd.OutOrStdout(), "Plan skipped: "+err.Error()+".")
				return nil
			}
			if err != nil {
				return err
			}
			plan, raw, err := requestPlan(cmd, c, doc)
			if err != nil {
				return err
			}
			var rendered string
			switch format {
			case "json":
				rendered = string(raw)
			case "markdown":
				rendered = plan.RenderMarkdown()
			default:
				var sb strings.Builder
				plan.RenderTable(&sb)
				rendered = sb.String()
			}
			if outFile != "" {
				if err := os.WriteFile(outFile, []byte(rendered), 0o644); err != nil {
					return err
				}
			} else {
				fmt.Fprint(cmd.OutOrStdout(), rendered)
				if !strings.HasSuffix(rendered, "\n") {
					fmt.Fprintln(cmd.OutOrStdout())
				}
			}
			if failLoosening && plan.IsLoosening() {
				return &ExitError{Code: 1, Msg: "the plan loosens the policy and --fail-on-loosening is set"}
			}
			if detailed && plan.HasChanges() {
				return &ExitError{Code: exitPlanChanges}
			}
			return nil
		},
	}
	d.bind(cmd)
	f.bind(cmd, "Unused: use --format json")
	_ = cmd.Flags().MarkHidden("json")
	fl := cmd.Flags()
	fl.StringVar(&format, "format", "table", "table, markdown or json")
	fl.StringVar(&outFile, "out", "", "Write the plan to this file instead of standard output")
	fl.BoolVar(&failLoosening, "fail-on-loosening", false, "Exit 1 when the plan loosens the policy")
	fl.BoolVar(&detailed, "detailed-exitcode", false, "Exit 2 when there are changes")
	return cmd
}

func policyApplyCmd() *cobra.Command {
	var d policyDirFlag
	var f approvalFlags
	var wait int
	var resume string
	var autoApprove bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply the files as a new policy release",
		Long: `Shows the plan, asks (unless --auto-approve, which only skips this local
question) and applies. When the organisation requires an approval for this
change, apply is held: an administrator reviews the impact and confirms with a
passkey. --wait follows it for that many seconds; otherwise, or when it runs
out, apply exits 12 and 'apply --resume <id>' completes it later.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentPolicy(brand.Cmd("policy apply")); err != nil {
				return err
			}
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if resume != "" {
				return waitPending(cmd, c, resume, time.Duration(wait)*time.Second)
			}
			doc, err := loadPolicy(cmd, d.dir)
			if err != nil {
				return err
			}
			plan, _, err := requestPlan(cmd, c, doc)
			if err != nil {
				return err
			}
			plan.RenderTable(out)
			if !plan.HasChanges() {
				return nil
			}
			if !autoApprove {
				if err := confirmOrYes(cmd, false, "\nApply this policy?"); err != nil {
					return err
				}
			}
			r, err := gatewayExchange(cmd.Context(), c, http.MethodPost, "/api/policy/apply", map[string]any{"files": doc.Files, "expectedParentId": plan.ParentID, "planId": plan.ID}, nil)
			if err != nil {
				return err
			}
			switch {
			case r.Status == http.StatusAccepted:
				var held struct {
					ID  string `json:"pending_apply_id"`
					URL string `json:"url"`
				}
				if err := json.Unmarshal(r.Body, &held); err != nil || held.ID == "" {
					return errors.New("the gateway held the apply but did not say which")
				}
				fmt.Fprintf(out, "Held for approval (%s).\n", textsafe.Clean(held.ID))
				if link, err := gatewayLink(c.GatewayURL, held.URL); err == nil {
					fmt.Fprintf(out, "An administrator decides it at %s\n", textsafe.Clean(link))
				}
				return waitPending(cmd, c, held.ID, time.Duration(wait)*time.Second)
			case !r.ok():
				return policyFailure(cmd.ErrOrStderr(), r)
			}
			var res struct {
				Release struct {
					ID string `json:"id"`
				} `json:"release"`
			}
			_ = json.Unmarshal(r.Body, &res)
			fmt.Fprintf(out, "Applied as release %s.\n", textsafe.Clean(res.Release.ID))
			return nil
		},
	}
	d.bind(cmd)
	f.bind(cmd, "Unused")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().IntVar(&wait, "wait", 0, "Seconds to wait for an approval")
	cmd.Flags().StringVar(&resume, "resume", "", "Wait for a held apply (pending_...) to be decided")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "Do not ask before applying (for CI)")
	return cmd
}

// waitPending follows a held apply until it is decided or the time ends.
func waitPending(cmd *cobra.Command, c *client.Client, id string, total time.Duration) error {
	out := cmd.OutOrStdout()
	deadline := time.Now().Add(total)
	for {
		var doc struct {
			Pending struct {
				Status    string `json:"status"`
				ReleaseID string `json:"releaseId"`
			} `json:"pending"`
		}
		if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/policy/pending/"+url.PathEscape(id), nil, &doc); err != nil {
			return err
		}
		switch doc.Pending.Status {
		case "approved", "applied":
			fmt.Fprintf(out, "Approved and applied as release %s.\n", textsafe.Clean(doc.Pending.ReleaseID))
			return nil
		case "denied":
			return &ExitError{Code: exitDenied, Msg: "an administrator denied this policy change"}
		case "expired":
			return &ExitError{Code: exitExpired, Msg: "the approval expired; apply again"}
		}
		if !time.Now().Before(deadline) {
			return &ExitError{Code: exitPending, Msg: fmt.Sprintf("still waiting for approval; complete it with `%s`", brand.Cmd("policy apply --resume "+id+" --wait 600"))}
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(policyPollDelay):
		}
	}
}

type policyStatus struct {
	Management    string `json:"management"`
	Repo          string `json:"repo"`
	Branch        string `json:"branch"`
	ActiveRelease struct {
		ID        string `json:"id"`
		Source    string `json:"source"`
		AppliedAt string `json:"appliedAt"`
		Commit    string `json:"commit"`
	} `json:"activeRelease"`
	Drift *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"drift"`
	BreakGlass *struct {
		Actor     string `json:"actor"`
		Reason    string `json:"reason"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"breakGlass"`
}

func fetchPolicyStatus(cmd *cobra.Command, c *client.Client) (policyStatus, []byte, error) {
	var doc struct {
		Status policyStatus `json:"status"`
	}
	raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/policy/status", nil, &doc)
	return doc.Status, raw, err
}

func policyStatusCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show how the policy is managed, the active release and any drift",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			s, raw, err := fetchPolicyStatus(cmd, c)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "Managed in: %s", textsafe.Clean(s.Management))
			if s.Repo != "" {
				fmt.Fprintf(out, " (%s@%s)", textsafe.Clean(s.Repo), textsafe.Clean(s.Branch))
			}
			fmt.Fprintf(out, "\nActive release: %s (%s, %s)\n", textsafe.Clean(s.ActiveRelease.ID), textsafe.Clean(s.ActiveRelease.Source), orDash(textsafe.Clean(s.ActiveRelease.AppliedAt)))
			if s.Drift != nil {
				fmt.Fprintf(out, "Drift: %s: %s\n", textsafe.Clean(s.Drift.Kind), textsafe.Clean(s.Drift.Message))
			}
			if s.BreakGlass != nil {
				fmt.Fprintf(out, "Break-glass: %s until %s: %s\n", textsafe.Clean(s.BreakGlass.Actor), textsafe.Clean(s.BreakGlass.ExpiresAt), textsafe.Clean(s.BreakGlass.Reason))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func printChanges(out io.Writer, changes []policyfile.Change) {
	if len(changes) == 0 {
		fmt.Fprintln(out, "No differences.")
		return
	}
	for _, c := range changes {
		loc := ""
		if c.File != "" {
			loc = fmt.Sprintf("  %s:%d", textsafe.Clean(c.File), c.Line)
		}
		mark := map[string]string{"added": "+", "removed": "-"}[c.Op]
		if mark == "" {
			mark = "~"
		}
		fmt.Fprintf(out, "  %s %s%s\n", mark, textsafe.Clean(c.Path), loc)
	}
}

func fetchReleaseDiff(cmd *cobra.Command, c *client.Client, id, against string) ([]policyfile.Change, []byte, error) {
	var doc struct {
		Diff struct {
			Changes []policyfile.Change `json:"changes"`
		} `json:"diff"`
	}
	raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/policy/releases/"+url.PathEscape(id)+"/diff?against="+url.QueryEscape(against), nil, &doc)
	return doc.Diff.Changes, raw, err
}

func policyDiffCmd() *cobra.Command {
	var f approvalFlags
	var live bool
	cmd := &cobra.Command{
		Use:   "diff --live",
		Short: "Show how the live policy differs from the active release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !live {
				return errors.New("--live is required; to compare two releases use `" + brand.Cmd("policy releases diff <a> <b>") + "`")
			}
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			s, _, err := fetchPolicyStatus(cmd, c)
			if err != nil {
				return err
			}
			changes, raw, err := fetchReleaseDiff(cmd, c, s.ActiveRelease.ID, "live")
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			printChanges(cmd.OutOrStdout(), changes)
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&live, "live", false, "Compare the live policy with the active release")
	return cmd
}

type releaseRow struct {
	ID             string `json:"id"`
	Source         string `json:"source"`
	AppliedAt      string `json:"appliedAt"`
	Actor          string `json:"actor"`
	Commit         string `json:"commit"`
	Classification string `json:"classification"`
	Provenance     struct {
		PR        string   `json:"pr"`
		Approvers []string `json:"approvers"`
	} `json:"provenance"`
	Signed bool `json:"signed"`
}

func policyReleasesCmd() *cobra.Command {
	var f approvalFlags
	list := func(cmd *cobra.Command, _ []string) error {
		c, err := policyClient(cmd.Context(), f.gatewayURL)
		if err != nil {
			return err
		}
		var doc struct {
			Releases []releaseRow `json:"releases"`
		}
		raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/policy/releases", nil, &doc)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if f.jsonOut {
			printRaw(out, raw)
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "RELEASE\tSOURCE\tAPPLIED\tBY\tCHANGE\tCOMMIT")
		for _, r := range doc.Releases {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(r.ID), textsafe.Clean(r.Source), textsafe.Clean(r.AppliedAt), orDash(textsafe.Clean(r.Actor)), orDash(textsafe.Clean(r.Classification)), orDash(textsafe.Clean(r.Commit)))
		}
		return tw.Flush()
	}
	cmd := &cobra.Command{Use: "releases", Short: "List and compare policy releases", Args: cobra.NoArgs, RunE: list}
	listCmd := &cobra.Command{Use: "list", Short: "List releases, newest first", Args: cobra.NoArgs, RunE: list}
	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one release with where it came from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Release releaseRow `json:"release"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/policy/releases/"+url.PathEscape(args[0]), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			r := doc.Release
			fmt.Fprintf(out, "Release %s: %s, applied %s by %s\n", textsafe.Clean(r.ID), textsafe.Clean(r.Source), textsafe.Clean(r.AppliedAt), orDash(textsafe.Clean(r.Actor)))
			if r.Commit != "" {
				fmt.Fprintf(out, "  commit %s, pull request %s, approved by %s\n", textsafe.Clean(r.Commit), orDash(textsafe.Clean(r.Provenance.PR)), orDash(textsafe.Clean(strings.Join(r.Provenance.Approvers, ", "))))
			}
			fmt.Fprintf(out, "  signed: %t\n", r.Signed)
			return nil
		},
	}
	diff := &cobra.Command{
		Use:   "diff <a> <b>",
		Short: "Show what changed from release b to release a",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			changes, raw, err := fetchReleaseDiff(cmd, c, args[0], args[1])
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			printChanges(cmd.OutOrStdout(), changes)
			return nil
		},
	}
	cmd.AddCommand(listCmd, show, diff)
	f.bind(cmd, "Print the response body unchanged")
	cmd.PersistentFlags().AddFlagSet(cmd.Flags())
	return cmd
}

func policyRollbackCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "rollback <release>",
		Short: "Apply an earlier release again as a new release (needs a passkey confirmation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseAgentPolicy(brand.Cmd("policy rollback")); err != nil {
				return err
			}
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Roll the policy back to release %s?", textsafe.Clean(args[0]))); err != nil {
				return err
			}
			r, err := gatewayExchange(cmd.Context(), c, http.MethodPost, "/api/policy/releases/"+url.PathEscape(args[0])+"/rollback", map[string]any{}, nil)
			if err != nil {
				return err
			}
			if !r.ok() {
				return policyFailure(cmd.ErrOrStderr(), r)
			}
			var res struct {
				Release struct {
					ID string `json:"id"`
				} `json:"release"`
				RevertPR string `json:"revertPr"`
				Hint     string `json:"hint"`
			}
			_ = json.Unmarshal(r.Body, &res)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Rolled back as release %s.\n", textsafe.Clean(res.Release.ID))
			if res.RevertPR != "" {
				fmt.Fprintf(out, "A pull request brings the repository in line: %s\n", textsafe.Clean(res.RevertPR))
			} else if res.Hint != "" {
				fmt.Fprintf(out, "To bring the repository in line: %s\n", textsafe.Clean(res.Hint))
			}
			return nil
		},
	}
	f.bind(cmd, "Unused")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Roll back without asking")
	return cmd
}

func policyFreezeCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "freeze",
		Short: "Set every write, destructive and outward action to off for agents (needs a passkey confirmation)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseAgentPolicy(brand.Cmd("policy freeze")); err != nil {
				return err
			}
			c, err := policyClient(cmd.Context(), f.gatewayURL)
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, "Freeze all agent writes? Nothing is undone automatically; a policy apply lifts it."); err != nil {
				return err
			}
			r, err := gatewayExchange(cmd.Context(), c, http.MethodPost, "/api/policy/freeze", map[string]any{}, nil)
			if err != nil {
				return err
			}
			if !r.ok() {
				return policyFailure(cmd.ErrOrStderr(), r)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Frozen. Agents can read; every write is off.")
			return nil
		},
	}
	f.bind(cmd, "Unused")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Freeze without asking")
	return cmd
}
