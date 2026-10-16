package cli

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport"
)

// artifactReport is the one JSON document a skill or agent write prints with
// --json: what was planned, what each target loses, what was written and any
// warnings. Prompts, fidelity text and progress go to stderr instead, so
// stdout can be piped into a parser.
type artifactReport struct {
	Command  string        `json:"command"`
	DryRun   bool          `json:"dryRun"`
	Plans    []*planReport `json:"plans"`
	Warnings []string      `json:"warnings,omitempty"`
	Error    string        `json:"error,omitempty"`
}

type planReport struct {
	From    string       `json:"from"`
	To      string       `json:"to"`
	Scope   string       `json:"scope"`
	Target  string       `json:"target"`
	Loss    []lossReport `json:"fidelity"`
	Dropped bool         `json:"dropsFields"`
	// Status is planned, dry-run, skipped (declined at the prompt), refused,
	// or written.
	Status  string `json:"status"`
	Written string `json:"written,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type lossReport struct {
	Field    string `json:"field"`
	Kind     string `json:"kind"`
	Note     string `json:"note,omitempty"`
	Security bool   `json:"security,omitempty"`
}

// addPlan starts the entry for one source-to-target projection. It is safe on
// a nil report, which means --json was not asked for.
func (r *artifactReport) addPlan(from, to string, scope agentport.Scope, target string, loss []agentport.LossItem, dropped bool) *planReport {
	if r == nil {
		return &planReport{}
	}
	p := &planReport{From: from, To: to, Scope: string(scope), Target: target, Dropped: dropped, Status: "planned", Loss: lossReports(loss)}
	r.Plans = append(r.Plans, p)
	return p
}

func (r *artifactReport) warn(msg string) {
	if r != nil {
		r.Warnings = append(r.Warnings, msg)
	}
}

// actionDoc is the --json document of the commands that do one thing (list,
// diff, scan, uninstall, rollback): a command name, the data it produced and
// the error if it failed. A nil doc means --json was not asked for.
type actionDoc map[string]any

// put records a result; it is a no-op without --json.
func (d actionDoc) put(key string, v any) {
	if d != nil {
		d[key] = v
	}
}

// beginAction is beginJSON for actionDoc: human output moves to stderr and
// finish prints the document on stdout.
func beginAction(cmd *cobra.Command, enabled bool, command string) (actionDoc, func(error) error) {
	if !enabled {
		return nil, func(err error) error { return err }
	}
	doc := actionDoc{"command": command}
	var out io.Writer = cmd.OutOrStdout()
	cmd.SetOut(cmd.ErrOrStderr())
	return doc, func(err error) error {
		if err != nil {
			doc["error"] = err.Error()
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(doc)
		return err
	}
}

func lossReports(loss []agentport.LossItem) []lossReport {
	out := make([]lossReport, 0, len(loss))
	for _, l := range loss {
		out = append(out, lossReport{Field: l.Field, Kind: string(l.Kind), Note: l.Note, Security: l.Security})
	}
	return out
}

// beginJSON switches a command to --json output. Everything the command
// writes through cmd.OutOrStdout from now on goes to stderr; the returned
// function prints the final document to the original stdout, with the error
// if the command failed, and passes the error through.
func beginJSON(cmd *cobra.Command, enabled bool, rep *artifactReport) (finish func(error) error, report *artifactReport) {
	if !enabled {
		return func(err error) error { return err }, nil
	}
	var doc io.Writer = cmd.OutOrStdout()
	cmd.SetOut(cmd.ErrOrStderr())
	return func(err error) error {
		if err != nil {
			rep.Error = err.Error()
		}
		if rep.Plans == nil {
			rep.Plans = []*planReport{}
		}
		enc := json.NewEncoder(doc)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return err
	}, rep
}
