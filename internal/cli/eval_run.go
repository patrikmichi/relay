package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/evals"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// evalPollInterval is how often a cloud run is asked for its state; tests shorten it.
var evalPollInterval = 2 * time.Second

func evalInitCmd() *cobra.Command {
	var target, dir string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create " + brand.EvalsConfigFile + " and a sample dataset",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			kinds := map[string]string{"skill": "skill", "prompt": "prompt", "agent": "agent_config", "mcp": "mcp_server", "app": "app"}
			kind, ok := kinds[target]
			if !ok {
				return fmt.Errorf("--target must be skill, prompt, agent, mcp or app")
			}
			files, err := evals.Scaffold(kind)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, f := range files {
				path := filepath.Join(dir, f.Path)
				if _, err := os.Stat(path); err == nil && !force && !f.Append {
					return fmt.Errorf("%s exists; pass --force to overwrite", path)
				}
			}
			for _, f := range files {
				path := filepath.Join(dir, f.Path)
				if f.Append {
					if err := appendMissingLines(path, f.Content); err != nil {
						return err
					}
				} else {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(path, []byte(f.Content), 0o644); err != nil {
						return err
					}
				}
				fmt.Fprintf(out, "wrote %s\n", path)
			}
			fmt.Fprintf(out, "Next: set a provider key and run `%s`\n", brand.Cmd("eval run --offline"))
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "skill", "What to evaluate: skill, prompt, agent, mcp or app")
	cmd.Flags().StringVar(&dir, "dir", ".", "Directory to write into")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite files that exist")
	return cmd
}

func appendMissingLines(path, lines string) error {
	existing, _ := os.ReadFile(path)
	have := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add strings.Builder
	for _, l := range strings.Split(strings.TrimRight(lines, "\n"), "\n") {
		if !have[strings.TrimSpace(l)] {
			add.WriteString(l + "\n")
		}
	}
	if add.Len() == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		_, _ = f.WriteString("\n")
	}
	_, err = f.WriteString(add.String())
	return err
}

type evalRunFlags struct {
	approvalFlags
	config, suite, target, out, models string
	repeats                            int
	offline, ci, noWait, noCache       bool
	timeout                            time.Duration
}

func evalRunCmd() *cobra.Command {
	var f evalRunFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a suite, here with --offline or on the gateway",
		Example: `  zuano eval run --offline --suite pr-triage-skill
  zuano eval run --suite pr-triage-skill --ci
  zuano eval run --target skill:pr-triage --models anthropic/claude-sonnet-5.5,openai/gpt-6-mini`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.offline {
				return evalRunLocal(cmd, f)
			}
			return evalRunCloud(cmd, f)
		},
	}
	f.bind(cmd, "Print the run as JSON")
	evalConfigFlag(cmd, &f.config)
	cmd.Flags().StringVar(&f.suite, "suite", "", "Suite id (default: the only suite in the config)")
	cmd.Flags().StringVar(&f.target, "target", "", "Find the suite on the gateway by target, such as skill:<slug>")
	cmd.Flags().StringVar(&f.models, "models", "", "Comma-separated provider/model list replacing the suite's models")
	cmd.Flags().IntVar(&f.repeats, "repeats", 0, "Repeats per case (default: the suite's)")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "Run on this machine with your own provider keys")
	cmd.Flags().BoolVar(&f.ci, "ci", false, "CI mode: record the commit, write a job summary and annotate failures")
	cmd.Flags().StringVar(&f.out, "out", evalResultsDir, "Directory for results.json, report.md and report.html")
	cmd.Flags().BoolVar(&f.noWait, "no-wait", false, "Start a gateway run and return its id without waiting")
	cmd.Flags().BoolVar(&f.noCache, "no-cache", false, "Do not reuse earlier outputs of identical cases on the gateway")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Minute, "How long to wait for a gateway run (exit 17 when it ends first)")
	return cmd
}

func commaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func evalRunLocal(cmd *cobra.Command, f evalRunFlags) error {
	cfg, err := loadEvalConfig(f.config)
	if err != nil {
		return err
	}
	suite, err := cfg.Suite(f.suite)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	progress := func(done, total int) {
		if !f.jsonOut && (done == total || done%25 == 0) {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %d of %d cases\n", done, total)
		}
	}
	run, err := evals.RunSuite(cmd.Context(), evals.RunSpec{Config: cfg, Suite: suite, Models: commaList(f.models), Repeats: f.repeats, Progress: progress})
	if err != nil {
		return err
	}
	seed := evals.DefaultSeed
	if suite.Seed != nil {
		seed = int(*suite.Seed)
	}
	var baseline *evals.Run
	var comparisons []evals.Comparison
	if ref := suite.Gates.NoRegressionVs; ref != "" {
		baseline, err = evals.LoadBaseline(cfg.Dir, ref)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "No regression check: %v\n", err)
		}
	}
	run.Gate = evals.Gate(run, suite.Gates, baseline, int64(seed))
	if baseline != nil {
		if pairs, perr := sameCells(baseline, run); perr == nil {
			comparisons = evals.CompareAll(baseline, run, pairs, int64(seed))
		}
	}
	dir := f.out
	if err := writeJSONFile(filepath.Join(dir, "results.json"), run); err != nil {
		return err
	}
	in := evals.ReportInput{Run: run, BaselineRef: suite.Gates.NoRegressionVs, Comparisons: comparisons}
	md := evals.Markdown(in)
	page, err := evals.HTML(in)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(md), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.html"), page, 0o644); err != nil {
		return err
	}
	if f.ci {
		writeJobSummary(md)
	}
	if f.jsonOut {
		raw, _ := json.MarshalIndent(run, "", "  ")
		fmt.Fprintln(out, string(raw))
	} else {
		fmt.Fprint(out, md)
		fmt.Fprintf(out, "\nResults in %s\n", dir)
	}
	return gateExit(cmd.ErrOrStderr(), run.Gate, f.ci)
}

func writeJobSummary(md string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	if fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
		_, _ = fh.WriteString(md + "\n")
		_ = fh.Close()
	}
}

// ciInfo describes the CI job a run belongs to.
func ciInfo(getenv func(string) string) map[string]string {
	switch {
	case getenv("GITHUB_ACTIONS") != "":
		info := map[string]string{"provider": "github", "repo": getenv("GITHUB_REPOSITORY"), "sha": getenv("GITHUB_SHA"), "ref": getenv("GITHUB_REF")}
		if parts := strings.Split(getenv("GITHUB_REF"), "/"); len(parts) >= 3 && parts[1] == "pull" {
			info["pr"] = parts[2]
		}
		return info
	case getenv("GITLAB_CI") != "":
		return map[string]string{"provider": "gitlab", "repo": getenv("CI_PROJECT_PATH"), "sha": getenv("CI_COMMIT_SHA"), "ref": getenv("CI_COMMIT_REF_NAME"), "pr": getenv("CI_MERGE_REQUEST_IID")}
	}
	return map[string]string{"provider": "other"}
}

// evalsAgentGate refuses a command that spends budget when it runs in an agent
// session, unless the organisation allows agents to run evaluations.
func evalsAgentGate(ctx context.Context, c *client.Client, what string) error {
	if !config.AgentMode() {
		return nil
	}
	var s struct {
		AgentsMayRun bool `json:"agents_may_run"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, "/api/evals/settings", nil, &s); err != nil || !s.AgentsMayRun {
		return &ExitError{Code: exitDenied, Msg: what + " spends the evaluation budget and is refused while ZUANO_AGENT is set; an administrator can allow it with evals.agents_may_run"}
	}
	return nil
}

type cloudRun struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Error   string `json:"error"`
	URL     string `json:"url"`
	Summary struct {
		Cells []evals.CellSummary `json:"cells"`
	} `json:"summary"`
	Gate   *evals.GateResult `json:"gate_result"`
	Totals struct {
		Units    int   `json:"units"`
		Cached   int   `json:"cached"`
		CostNano int64 `json:"cost_nano"`
	} `json:"totals"`
}

// resolveCloudSuite finds the suite to run: --suite, a suite found by --target,
// or the only suite in the config file.
func resolveCloudSuite(ctx context.Context, c *client.Client, f evalRunFlags) (string, error) {
	if f.suite != "" {
		return f.suite, nil
	}
	if f.target != "" {
		return suiteByTarget(ctx, c, f.target)
	}
	if cfg, err := loadEvalConfig(f.config); err == nil {
		s, err := cfg.Suite("")
		if err != nil {
			return "", err
		}
		return s.ID, nil
	}
	return "", errors.New("name the suite with --suite or --target skill:<slug>")
}

func suiteByTarget(ctx context.Context, c *client.Client, target string) (string, error) {
	kind, slug, ok := strings.Cut(target, ":")
	if !ok || kind == "" || slug == "" {
		return "", fmt.Errorf("--target %q must be kind:slug, such as skill:pr-triage", target)
	}
	var doc struct {
		Suites []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"suites"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, "/api/evals/suites?target="+url.QueryEscape(target), nil, &doc); err != nil {
		return "", err
	}
	switch len(doc.Suites) {
	case 0:
		return "", fmt.Errorf("no suite evaluates %s: create one in the evals app or with %s", target, brand.EvalsConfigFile)
	case 1:
		return doc.Suites[0].ID, nil
	}
	var slugs []string
	for _, s := range doc.Suites {
		slugs = append(slugs, s.Slug)
	}
	return "", fmt.Errorf("%d suites evaluate %s (%s); choose one with --suite", len(doc.Suites), target, strings.Join(slugs, ", "))
}

func evalRunCloud(cmd *cobra.Command, f evalRunFlags) error {
	c, err := approvalsClient(f.gatewayURL)
	if err != nil {
		return noKeysHint(err)
	}
	ctx := cmd.Context()
	if err := evalsAgentGate(ctx, c, brand.Cmd("eval run")); err != nil {
		return err
	}
	suite, err := resolveCloudSuite(ctx, c, f)
	if err != nil {
		return err
	}
	body := map[string]any{"suiteId": suite, "cache": !f.noCache, "trigger": "manual"}
	if f.repeats > 0 {
		body["repeats"] = f.repeats
	}
	if models := commaList(f.models); len(models) > 0 {
		var cells []map[string]string
		for _, m := range models {
			ref, err := evals.ParseModel(m)
			if err != nil {
				return err
			}
			cells = append(cells, map[string]string{"provider": ref.Provider, "model": ref.Model})
		}
		body["cells"] = cells
	}
	if f.ci {
		body["trigger"] = "ci"
		body["ci"] = ciInfo(os.Getenv)
	}
	var started struct {
		RunID    string `json:"runId"`
		Estimate struct {
			Units    int   `json:"units"`
			CostNano int64 `json:"cost_nano"`
		} `json:"estimate"`
	}
	if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/evals/runs", body, &started); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if !f.jsonOut {
		fmt.Fprintf(out, "Started run %s (%d case runs reserved)\n", started.RunID, started.Estimate.Units)
	}
	if f.noWait {
		if f.jsonOut {
			printRaw(out, jsonBytes(started))
		}
		return nil
	}
	run, raw, err := waitForEvalRun(ctx, c, started.RunID, f.timeout)
	if err != nil {
		return err
	}
	return finishCloudRun(cmd, run, raw, f.jsonOut, f.ci)
}

func noKeysHint(err error) error {
	return fmt.Errorf("%w; to run here with your own provider keys use `%s`", err, brand.Cmd("eval run --offline"))
}

func jsonBytes(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func waitForEvalRun(ctx context.Context, c *client.Client, id string, timeout time.Duration) (*cloudRun, []byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		var doc struct {
			Run cloudRun `json:"run"`
		}
		raw, err := gatewayJSON(ctx, c, http.MethodGet, "/api/evals/runs/"+url.PathEscape(id), nil, &doc)
		if err != nil {
			return nil, nil, err
		}
		if jobTerminal(doc.Run.Status) {
			return &doc.Run, raw, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, &ExitError{Code: exitWaitTimeout, Msg: fmt.Sprintf("run %s is still %s; look at it again with `%s`", id, doc.Run.Status, brand.Cmd("eval report "+id))}
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(evalPollInterval):
		}
	}
}

func finishCloudRun(cmd *cobra.Command, run *cloudRun, raw []byte, jsonOut, ci bool) error {
	out := cmd.OutOrStdout()
	normalizeGate(run.Gate)
	if jsonOut {
		printRaw(out, raw)
	} else {
		fmt.Fprintf(out, "Run %s %s\n", run.ID, run.Status)
		if len(run.Summary.Cells) > 0 {
			printCellTable(out, run.Summary.Cells)
		}
		if run.Totals.Units > 0 {
			fmt.Fprintf(out, "\n%d case runs (%d cached)\n", run.Totals.Units, run.Totals.Cached)
		}
		if run.URL != "" {
			fmt.Fprintf(out, "%s\n", textsafe.Strict(run.URL))
		}
	}
	switch run.Status {
	case "failed", "cancelled":
		msg := textsafe.Strict(run.Error)
		if msg == "" {
			msg = "the run " + run.Status
		}
		return &ExitError{Code: exitRunFailed, Msg: msg}
	}
	return gateExit(cmd.ErrOrStderr(), run.Gate, ci)
}

func parseRunID(s string) bool { return strings.HasPrefix(s, "evr_") }
