package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const maxEvalDocumentBytes = 8 << 20

// SkillEvalsCmd returns `relay skill evals`: manage the eval sets and cases a
// skill is scored against.
func SkillEvalsCmd() *cobra.Command {
	evals := &cobra.Command{
		Use:   "evals",
		Short: "Manage a skill's eval sets and cases",
		Long: `An eval set is a list of cases a skill is scored against: trigger cases ask
whether a prompt should make the model load the skill, task cases run the
skill and grade the answer. Cases are split into train and validation by a
stable hash. Run a set with 'relay skill eval'.`,
	}
	sets := &cobra.Command{Use: "sets", Short: "List, show, create, import, export and delete eval sets"}
	sets.AddCommand(evalSetsListCmd(), evalSetsShowCmd(), evalSetsCreateCmd(), evalSetsImportCmd(), evalSetsExportCmd(), evalSetsDeleteCmd())
	cases := &cobra.Command{Use: "case", Aliases: []string{"cases"}, Short: "Add and remove cases in an eval set"}
	cases.AddCommand(evalCaseAddCmd(), evalCaseRemoveCmd())
	evals.AddCommand(sets, cases)
	return evals
}

func evalSetsListCmd() *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "list <skill>",
		Short: "List a skill's eval sets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			var doc struct {
				Sets []evalSet `json:"sets"`
			}
			raw, err := a.getJSON(cmd.Context(), "/evals/sets", &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return printIndented(out, raw)
			}
			if len(doc.Sets) == 0 {
				fmt.Fprintf(out, "%s has no eval sets\n", a.label())
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tTRIGGER\tTASK\tTRAIN\tVALIDATION")
			for _, s := range doc.Sets {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\n", s.ID, s.Name, s.Counts.Trigger, s.Counts.Task, s.Counts.Train, s.Counts.Validation)
			}
			return tw.Flush()
		},
	}
	f.add(cmd)
	return cmd
}

func evalSetsShowCmd() *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "show <skill> <set>",
		Short: "Show an eval set and its cases",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			set, err := a.resolveSet(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			var doc struct {
				Set evalSet `json:"set"`
			}
			raw, err := a.getJSON(cmd.Context(), "/evals/sets/"+url.PathEscape(set.ID), &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				return printIndented(out, raw)
			}
			s := doc.Set
			fmt.Fprintf(out, "Set:         %s (%s)\n", s.Name, s.ID)
			if s.Description != "" {
				fmt.Fprintf(out, "Description: %s\n", s.Description)
			}
			fmt.Fprintf(out, "Split:       %d%% train; %d train, %d validation cases\n", s.TrainPercent, s.Counts.Train, s.Counts.Validation)
			fmt.Fprintf(out, "Cases:       %d trigger, %d task\n", s.Counts.Trigger, s.Counts.Task)
			if len(s.Cases) == 0 {
				return nil
			}
			fmt.Fprintln(out)
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tSPLIT\tNAME\tINPUT")
			for _, c := range s.Cases {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Kind, c.Split, c.Name, truncate(c.Input, 60))
			}
			return tw.Flush()
		},
	}
	f.add(cmd)
	return cmd
}

func truncate(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "..."
}

func evalSetsCreateCmd() *cobra.Command {
	var f evalFlags
	var name, description, version string
	var trainPercent int
	cmd := &cobra.Command{
		Use:   "create <skill> --name NAME",
		Short: "Create an empty eval set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			body := map[string]any{"name": name}
			if description != "" {
				body["description"] = description
			}
			if trainPercent != 0 {
				body["trainPercent"] = trainPercent
			}
			if version != "" {
				v, err := a.resolveVersion(cmd.Context(), version)
				if err != nil {
					return err
				}
				body["versionId"] = v.ID
			}
			var doc struct {
				Set evalSet `json:"set"`
			}
			raw, err := a.sendJSON(cmd.Context(), http.MethodPost, "/evals/sets", body, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created eval set %s (%s)\n", doc.Set.Name, doc.Set.ID)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&name, "name", "", "Set name (required)")
	cmd.Flags().StringVar(&description, "description", "", "What the set covers")
	cmd.Flags().StringVar(&version, "version", "", "Version the set was written against (semver or rev_ id)")
	cmd.Flags().IntVar(&trainPercent, "train-percent", 0, "Share of cases hashed into the train split, 10 to 90 (default 70)")
	return cmd
}

// readDocument reads a JSON document from a file or, for "-", the command's stdin.
func readDocument(cmd *cobra.Command, path string) (json.RawMessage, error) {
	var r io.Reader
	if path == "-" {
		r = cmd.InOrStdin()
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	data, err := readLimitedResponse(r, maxEvalDocumentBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s is not valid JSON", path)
	}
	return data, nil
}

func evalSetsImportCmd() *cobra.Command {
	var f evalFlags
	var version string
	cmd := &cobra.Command{
		Use:   "import <skill> <file|->",
		Short: "Create an eval set from an exported document",
		Long:  "Create a set and its cases from a relay.skill-evals/v1 document, as written by 'relay skill evals sets export'. Use - to read it from stdin.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := readDocument(cmd, args[1])
			if err != nil {
				return err
			}
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			suffix := "/evals/import"
			if version != "" {
				v, err := a.resolveVersion(cmd.Context(), version)
				if err != nil {
					return err
				}
				suffix += "?versionId=" + url.QueryEscape(v.ID)
			}
			var res struct {
				Set evalSet `json:"set"`
			}
			raw, err := a.sendJSON(cmd.Context(), http.MethodPost, suffix, doc, &res)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Imported eval set %s (%s) with %d cases\n", res.Set.Name, res.Set.ID, res.Set.Counts.Trigger+res.Set.Counts.Task)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&version, "version", "", "Version the set was written against (semver or rev_ id)")
	return cmd
}

func evalSetsExportCmd() *cobra.Command {
	var f evalFlags
	var outFile string
	cmd := &cobra.Command{
		Use:   "export <skill> <set>",
		Short: "Write an eval set as a portable JSON document",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			set, err := a.resolveSet(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			raw, err := a.do(cmd.Context(), http.MethodGet, "/evals/sets/"+url.PathEscape(set.ID)+"/export", nil)
			if err != nil {
				return err
			}
			if outFile == "" {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			var buf strings.Builder
			if err := printIndented(&buf, raw); err != nil {
				return err
			}
			if err := os.WriteFile(outFile, []byte(buf.String()), 0o600); err != nil {
				return err
			}
			if !f.jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", outFile)
			}
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVarP(&outFile, "out", "o", "", "Write the document to this file (default: stdout)")
	return cmd
}

func evalSetsDeleteCmd() *cobra.Command {
	var f evalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <skill> <set> --yes",
		Short: "Delete an eval set with its cases and runs",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("deleting a set also deletes its cases and runs; pass --yes to confirm")
			}
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			set, err := a.resolveSet(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			raw, err := a.do(cmd.Context(), http.MethodDelete, "/evals/sets/"+url.PathEscape(set.ID), nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted eval set %s (%s)\n", set.Name, set.ID)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm the deletion")
	return cmd
}

type caseFlags struct {
	file, kind, name, input, rubric, reference, split string
	shouldTrigger                                     bool
	contains, notContains, regex, connectors          []string
}

// build assembles the case body from flags, or reads it whole from --file.
func (cf caseFlags) build(cmd *cobra.Command) (any, error) {
	if cf.file != "" {
		return readDocument(cmd, cf.file)
	}
	if cf.kind != "trigger" && cf.kind != "task" {
		return nil, fmt.Errorf("--kind must be trigger or task")
	}
	if cf.name == "" || cf.input == "" {
		return nil, fmt.Errorf("--name and --input are required")
	}
	body := map[string]any{"kind": cf.kind, "name": cf.name, "input": cf.input}
	if len(cf.connectors) > 0 {
		body["requiredConnectors"] = cf.connectors
	}
	if cf.split != "" {
		body["split"] = cf.split
	}
	if cf.kind == "trigger" {
		if !cmd.Flags().Changed("should-trigger") {
			return nil, fmt.Errorf("a trigger case needs --should-trigger or --should-trigger=false")
		}
		body["shouldTrigger"] = cf.shouldTrigger
		return body, nil
	}
	var assertions []map[string]any
	for _, v := range cf.contains {
		assertions = append(assertions, map[string]any{"type": "contains", "value": v})
	}
	for _, v := range cf.notContains {
		assertions = append(assertions, map[string]any{"type": "not_contains", "value": v})
	}
	for _, v := range cf.regex {
		assertions = append(assertions, map[string]any{"type": "regex", "pattern": v})
	}
	if len(assertions) == 0 && cf.rubric == "" {
		return nil, fmt.Errorf("a task case needs at least one of --contains, --not-contains, --regex or --rubric")
	}
	expected := map[string]any{}
	if len(assertions) > 0 {
		expected["assertions"] = assertions
	}
	if cf.rubric != "" {
		expected["rubric"] = cf.rubric
	}
	if cf.reference != "" {
		expected["reference"] = cf.reference
	}
	body["expected"] = expected
	return body, nil
}

func evalCaseAddCmd() *cobra.Command {
	var f evalFlags
	var cf caseFlags
	cmd := &cobra.Command{
		Use:   "add <skill> <set>",
		Short: "Add a case to an eval set",
		Long: `Add a trigger or a task case.

A trigger case asks whether a prompt should make the model load the skill:
  relay skill evals case add my-skill smoke --kind trigger --name "asks for an invoice" \
    --input "Please invoice Acme for March" --should-trigger

A task case runs the skill and grades the answer with assertions and/or a rubric:
  relay skill evals case add my-skill smoke --kind task --name "invoice total" \
    --input "What is the total of invoice 42?" --contains "1 250" --rubric "States the currency"

--file reads the whole case as JSON (use it for json_path and structured
assertions); see the case shapes in the skill evals API reference.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := cf.build(cmd)
			if err != nil {
				return err
			}
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			set, err := a.resolveSet(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			var doc struct {
				Case evalCase `json:"case"`
			}
			raw, err := a.sendJSON(cmd.Context(), http.MethodPost, "/evals/sets/"+url.PathEscape(set.ID)+"/cases", body, &doc)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added %s case %s to %s (%s split)\n", doc.Case.Kind, doc.Case.ID, set.Name, doc.Case.Split)
			return nil
		},
	}
	f.add(cmd)
	fl := cmd.Flags()
	fl.StringVar(&cf.file, "file", "", "Read the case as JSON from this file (- for stdin)")
	fl.StringVar(&cf.kind, "kind", "", "trigger or task")
	fl.StringVar(&cf.name, "name", "", "Case name")
	fl.StringVar(&cf.input, "input", "", "The prompt to send")
	fl.BoolVar(&cf.shouldTrigger, "should-trigger", false, "Trigger case: the skill should be the one loaded (=false for a negative case)")
	fl.StringArrayVar(&cf.contains, "contains", nil, "Task case: the answer must contain this text (repeatable)")
	fl.StringArrayVar(&cf.notContains, "not-contains", nil, "Task case: the answer must not contain this text (repeatable)")
	fl.StringArrayVar(&cf.regex, "regex", nil, "Task case: the answer must match this pattern (repeatable)")
	fl.StringVar(&cf.rubric, "rubric", "", "Task case: what a good answer must do, scored by the judge")
	fl.StringVar(&cf.reference, "reference", "", "Task case: a model answer for the judge to compare with")
	fl.StringArrayVar(&cf.connectors, "connector", nil, "Connector the case needs; skipped when the run does not reach it (repeatable)")
	fl.StringVar(&cf.split, "split", "", "Pin the case to train or validation (default: by hash)")
	return cmd
}

func evalCaseRemoveCmd() *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "remove <skill> <set> <case-id>",
		Short: "Remove a case from an eval set",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.open(cmd, args[0])
			if err != nil {
				return err
			}
			set, err := a.resolveSet(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			raw, err := a.do(cmd.Context(), http.MethodDelete, "/evals/sets/"+url.PathEscape(set.ID)+"/cases/"+url.PathEscape(args[2]), nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				return printIndented(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed case %s from %s\n", args[2], set.Name)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}
