package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type optimizeCandidate struct {
	Index          int      `json:"index"`
	DraftID        string   `json:"draftId"`
	Train          float64  `json:"train"`
	Validation     *float64 `json:"validation"`
	TrainGain      float64  `json:"trainGain"`
	ValidationGain *float64 `json:"validationGain"`
	Overfit        bool     `json:"overfit"`
}

type optimizeResult struct {
	Baseline struct {
		Train      float64 `json:"train"`
		Validation float64 `json:"validation"`
	} `json:"baseline"`
	Candidates []optimizeCandidate `json:"candidates"`
	BestIndex  *int                `json:"bestIndex"`
	Overfit    bool                `json:"overfit"`
	Reasons    []string            `json:"reasons"`
}

type optimizeJob struct {
	ID           string          `json:"id"`
	SetID        string          `json:"setId"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"`
	Error        *string         `json:"error"`
	InputTokens  int             `json:"inputTokens"`
	OutputTokens int             `json:"outputTokens"`
	CostUSD      float64         `json:"costUsd"`
	Result       *optimizeResult `json:"result"`
}

type evalDraft struct {
	ID                string         `json:"id"`
	Kind              string         `json:"kind"`
	Label             string         `json:"label"`
	Status            string         `json:"status"`
	BaseVersionID     string         `json:"baseVersionId"`
	PromotedVersionID string         `json:"promotedVersionId"`
	Description       string         `json:"description"`
	Content           string         `json:"content"`
	Notes             map[string]any `json:"notes"`
	CreatedAt         string         `json:"createdAt"`
}

type optimizeDoc struct {
	Job    optimizeJob `json:"job"`
	Drafts []evalDraft `json:"drafts"`
}

// SkillOptimizeCmd returns `relay skill optimize`.
func SkillOptimizeCmd() *cobra.Command {
	var f evalFlags
	var set, kind, version, model, judge, jobID string
	var candidates, repeats, maxTokens int
	var overfit float64
	var wait, cancel bool
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "optimize <skill> --kind description|instructions",
		Short: "Rewrite a skill's description or instructions into a scored draft",
		Long: fmt.Sprintf(`Start an optimization job on an eval set. The gateway scores the version, shows
the model only the failing train cases, asks for rewrites, scores each on
train, scores the best one once on validation and keeps it as a draft when it
beat the version on train. A draft is never published by itself: look at it
with 'relay skill drafts show' and publish it with 'relay skill drafts promote'.

--kind description rewrites the description (trigger cases); --kind
instructions rewrites the SKILL.md (task cases). Without --wait the job starts
and its id is printed; --wait polls and prints the result. Exit status %d when
the job failed or was cancelled, 4 when --wait-timeout passed.

Example:
  relay skill optimize weekly-timesheet --kind description --set smoke --wait`, exitEvalFailed),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			if jobID != "" && cancel {
				raw, err := a.do(ctx, http.MethodDelete, "/optimize/"+url.PathEscape(jobID), nil)
				if err != nil {
					return err
				}
				if f.jsonOut {
					return printIndented(out, raw)
				}
				fmt.Fprintf(out, "Cancelled job %s\n", jobID)
				return nil
			}
			if jobID == "" {
				if kind != "description" && kind != "instructions" {
					return fmt.Errorf("--kind must be description or instructions")
				}
				s, err := a.resolveSet(ctx, set)
				if err != nil {
					return err
				}
				body := map[string]any{"setId": s.ID, "kind": kind}
				if version != "" {
					v, err := a.resolveVersion(ctx, version)
					if err != nil {
						return err
					}
					body["versionId"] = v.ID
				}
				for k, v := range map[string]int{"candidates": candidates, "repeats": repeats, "maxTokens": maxTokens} {
					if v != 0 {
						body[k] = v
					}
				}
				if model != "" {
					body["model"] = model
				}
				if judge != "" {
					body["judgeModel"] = judge
				}
				if cmd.Flags().Changed("overfit-threshold") {
					body["overfitThreshold"] = overfit
				}
				var started optimizeDoc
				raw, err := a.sendJSON(ctx, http.MethodPost, "/optimize", body, &started)
				if err != nil {
					return err
				}
				jobID = started.Job.ID
				if !wait {
					if f.jsonOut {
						return printIndented(out, raw)
					}
					fmt.Fprintf(out, "Started %s optimization job %s on set %s.\nFollow it with: relay skill optimize %s --job %s --wait\n", kind, jobID, s.Name, a.label(), jobID)
					return nil
				}
			}

			var doc optimizeDoc
			var raw []byte
			fetch := func() error {
				var err error
				raw, err = a.getJSON(ctx, "/optimize/"+url.PathEscape(jobID), &doc)
				return err
			}
			if wait {
				resume := fmt.Sprintf("relay skill optimize %s --job %s --wait", a.label(), jobID)
				err = waitFor(ctx, waitTimeout, cmd.ErrOrStderr(), f.jsonOut, resume, func() (bool, string, error) {
					if err := fetch(); err != nil {
						return false, "", err
					}
					return terminalStatus(doc.Job.Status), fmt.Sprintf("job %s: %s", jobID, doc.Job.Status), nil
				})
				if err != nil {
					return err
				}
			} else if err := fetch(); err != nil {
				return err
			}

			if f.jsonOut {
				err = printIndented(out, raw)
			} else {
				renderOptimize(out, a.label(), doc)
			}
			if err != nil {
				return err
			}
			if s := doc.Job.Status; s == "failed" || s == "cancelled" {
				msg := fmt.Sprintf("optimization job %s %s", jobID, s)
				if doc.Job.Error != nil && *doc.Job.Error != "" {
					msg += ": " + *doc.Job.Error
				}
				return &ExitError{Code: exitEvalFailed, Msg: msg}
			}
			return nil
		},
	}
	f.add(cmd)
	fl := cmd.Flags()
	fl.StringVar(&kind, "kind", "", "description or instructions")
	fl.StringVar(&set, "set", "", "Eval set id or name (default: the skill's only set)")
	fl.StringVar(&version, "version", "", "Version to improve: semver or rev_ id (default: the current published version)")
	fl.IntVar(&candidates, "candidates", 0, "Rewrites to try, 1 to 6")
	fl.IntVar(&repeats, "repeats", 0, "Repeats per case, 1 to 5")
	fl.StringVar(&model, "model", "", "Model to run")
	fl.StringVar(&judge, "judge-model", "", "Model that judges rubric cases")
	fl.IntVar(&maxTokens, "max-tokens", 0, "Lower the token budget of the job")
	fl.Float64Var(&overfit, "overfit-threshold", 0, "Largest train gain over validation gain that still passes, 0 to 1")
	fl.BoolVar(&wait, "wait", false, "Wait for the job to finish and print the result")
	fl.DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "Longest time to wait with --wait")
	fl.StringVar(&jobID, "job", "", "Show (and with --wait, wait for) an existing job instead of starting one")
	fl.BoolVar(&cancel, "cancel", false, "With --job: cancel the job")
	return cmd
}

func gainText(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%+.2f", *v)
}

func scoreText(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *v)
}

func renderOptimize(out io.Writer, skill string, d optimizeDoc) {
	j := d.Job
	fmt.Fprintf(out, "Job %s  %s  %s  set %s\n", j.ID, j.Status, j.Kind, j.SetID)
	fmt.Fprintf(out, "%d tokens in / %d out, $%.4f\n", j.InputTokens, j.OutputTokens, j.CostUSD)
	if j.Error != nil && *j.Error != "" {
		fmt.Fprintf(out, "Error: %s\n", *j.Error)
	}
	if r := j.Result; r != nil {
		fmt.Fprintf(out, "Baseline: train %.2f, validation %.2f\n\n", r.Baseline.Train, r.Baseline.Validation)
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tTRAIN\tTRAIN GAIN\tVALIDATION\tVALIDATION GAIN\tOVERFIT\tDRAFT")
		for _, c := range r.Candidates {
			best := ""
			if r.BestIndex != nil && *r.BestIndex == c.Index {
				best = " (best)"
			}
			fmt.Fprintf(tw, "%d%s\t%.2f\t%+.2f\t%s\t%s\t%v\t%s\n", c.Index, best, c.Train, c.TrainGain, scoreText(c.Validation), gainText(c.ValidationGain), c.Overfit, dash(c.DraftID))
		}
		_ = tw.Flush()
		for _, reason := range r.Reasons {
			fmt.Fprintf(out, "- %s\n", reason)
		}
	}
	for _, dr := range d.Drafts {
		fmt.Fprintf(out, "\nDraft %s (%s, %s) is waiting: relay skill drafts show %s %s\n", dr.ID, dr.Kind, dr.Status, skill, dr.ID)
	}
}

// SkillDraftsCmd returns `relay skill drafts`.
func SkillDraftsCmd() *cobra.Command {
	drafts := &cobra.Command{
		Use:   "drafts",
		Short: "List, show, promote and discard optimization drafts",
		Long: `An optimization job leaves its best rewrite as a draft. A draft never
publishes by itself; 'promote' sends it through the normal publish pipeline,
so every gate and review applies.`,
	}
	drafts.AddCommand(draftsListCmd(), draftsShowCmd(), draftsPromoteCmd(), draftsDiscardCmd())
	return drafts
}

func draftsListCmd() *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "list <skill>",
		Short: "List a skill's drafts, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			var doc struct {
				Drafts []evalDraft `json:"drafts"`
			}
			raw, err := a.getJSON(cmd.Context(), "/drafts", &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return printIndented(out, raw)
			}
			if len(doc.Drafts) == 0 {
				fmt.Fprintf(out, "%s has no drafts\n", a.label())
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tSTATUS\tLABEL\tCREATED")
			for _, d := range doc.Drafts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.ID, d.Kind, d.Status, d.Label, d.CreatedAt)
			}
			return tw.Flush()
		},
	}
	f.add(cmd)
	return cmd
}

func draftsShowCmd() *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "show <skill> <draft-id>",
		Short: "Show a draft with its scores and text",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			var doc struct {
				Draft evalDraft `json:"draft"`
			}
			raw, err := a.getJSON(cmd.Context(), "/drafts/"+url.PathEscape(args[1]), &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return printIndented(out, raw)
			}
			d := doc.Draft
			fmt.Fprintf(out, "Draft:   %s (%s)\nKind:    %s\nStatus:  %s\nBase:    %s\n", d.ID, d.Label, d.Kind, d.Status, dash(d.BaseVersionID))
			if d.PromotedVersionID != "" {
				fmt.Fprintf(out, "Promoted as: %s\n", d.PromotedVersionID)
			}
			for _, k := range []string{"trainGain", "validationGain", "overfit"} {
				if v, ok := d.Notes[k]; ok {
					fmt.Fprintf(out, "%-7s  %v\n", k+":", v)
				}
			}
			switch {
			case d.Description != "":
				fmt.Fprintf(out, "\nDescription:\n%s\n", d.Description)
			case d.Content != "":
				fmt.Fprintf(out, "\nSKILL.md:\n%s\n", strings.TrimRight(d.Content, "\n"))
			}
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func draftsPromoteCmd() *cobra.Command {
	var f evalFlags
	var version string
	cmd := &cobra.Command{
		Use:   "promote <skill> <draft-id>",
		Short: "Publish a draft as a new version through the publish pipeline",
		Long:  "Publish a draft as a new version. The gateway runs every publish gate and review on it, so the new version may land in a state that needs approval. --version sets the semver (default: the next patch).",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			body := map[string]any{}
			if version != "" {
				body["version"] = version
			}
			var doc struct {
				Published struct {
					ResourceID string `json:"resourceId"`
					VersionID  string `json:"versionId"`
					Semver     string `json:"semver"`
					State      string `json:"state"`
				} `json:"published"`
			}
			raw, err := a.sendJSON(cmd.Context(), http.MethodPost, "/drafts/"+url.PathEscape(args[1])+"/promote", body, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			p := doc.Published
			fmt.Fprintf(cmd.OutOrStdout(), "Promoted %s as %s %s (%s), state %s\n", args[1], a.label(), p.Semver, p.VersionID, p.State)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&version, "version", "", "Semver for the new version, X.Y.Z (default: next patch)")
	return cmd
}

func draftsDiscardCmd() *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "discard <skill> <draft-id>",
		Short: "Discard a draft",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			raw, err := a.do(cmd.Context(), http.MethodDelete, "/drafts/"+url.PathEscape(args[1]), nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Discarded draft %s\n", args[1])
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

type qualityPoint struct {
	RunID      string   `json:"runId"`
	Kind       string   `json:"kind"`
	At         string   `json:"at"`
	VersionID  string   `json:"versionId"`
	Semver     *string  `json:"semver"`
	Train      *float64 `json:"train"`
	Validation *float64 `json:"validation"`
	Errors     int      `json:"errors"`
}

func (p qualityPoint) version() string {
	if p.Semver != nil {
		return *p.Semver
	}
	return p.VersionID
}

// SkillQualityCmd returns `relay skill quality`.
func SkillQualityCmd() *cobra.Command {
	var f evalFlags
	var all bool
	cmd := &cobra.Command{
		Use:   "quality [skill]",
		Short: "Show a skill's quality score and its history by version",
		Long: `Show the 0 to 100 quality score (the mean over kinds of the latest validation
score), the latest score per kind and the score of every evaluated version.
--all lists every skill of the organization, weakest first (admin or owner).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) == 1) {
				return fmt.Errorf("name a skill, or pass --all for the organization dashboard")
			}
			if all {
				return runQualityAll(cmd, f)
			}
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			var doc struct {
				Score   *float64                `json:"score"`
				Latest  map[string]qualityPoint `json:"latest"`
				History []qualityPoint          `json:"history"`
			}
			raw, err := a.getJSON(cmd.Context(), "/quality", &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return printIndented(out, raw)
			}
			if doc.Score == nil {
				fmt.Fprintf(out, "%s has not been evaluated yet; run `relay skill eval %s --wait`\n", a.label(), a.label())
				return nil
			}
			fmt.Fprintf(out, "%s quality: %.0f/100\n", a.label(), *doc.Score)
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "\nAT\tVERSION\tKIND\tTRAIN\tVALIDATION\tERRORS")
			for _, p := range doc.History {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", p.At, p.version(), p.Kind, scoreText(p.Train), scoreText(p.Validation), p.Errors)
			}
			return tw.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "List every skill of the organization, weakest first")
	return cmd
}

func runQualityAll(cmd *cobra.Command, f evalFlags) error {
	gURL, err := resolveGatewayURLOrFailClosed(f.gatewayURL)
	if err != nil {
		return err
	}
	c, err := resolveClient(gURL)
	if err != nil {
		return err
	}
	a := &evalAPI{c: c}
	var doc struct {
		Skills []struct {
			Slug        string   `json:"slug"`
			Name        string   `json:"name"`
			Score       *float64 `json:"score"`
			EvaluatedAt string   `json:"evaluatedAt"`
		} `json:"skills"`
	}
	raw, err := a.request(cmd.Context(), http.MethodGet, "/api/marketplace/skills/quality", nil)
	if err != nil {
		return err
	}
	if err := decodeEval(raw, &doc); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if f.jsonOut {
		return printIndented(out, raw)
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SKILL\tNAME\tSCORE\tEVALUATED")
	for _, s := range doc.Skills {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Slug, s.Name, scoreWhole(s.Score), dash(s.EvaluatedAt))
	}
	return tw.Flush()
}

func scoreWhole(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f", *v)
}
