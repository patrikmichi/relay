package packs

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// Operation statuses in a plan.
const (
	StatusApply           = "apply"
	StatusNoop            = "noop"
	StatusConflict        = "conflict"
	StatusSkippedPlan     = "skipped_plan"
	StatusSkippedPlatform = "skipped_platform"
	StatusSkippedMissing  = "skipped_missing"
)

// Op is one change a plan makes.
type Op struct {
	Seq      int               `json:"seq"`
	Kind     string            `json:"kind"`
	Target   map[string]string `json:"target"`
	Before   json.RawMessage   `json:"before"`
	After    json.RawMessage   `json:"after"`
	Status   string            `json:"status"`
	Loosens  bool              `json:"loosens"`
	Requires *struct {
		Feature      string `json:"feature"`
		Limit        string `json:"limit"`
		RequiredPlan string `json:"requiredPlan"`
	} `json:"requires"`
	Reason string `json:"reason"`
}

// CredentialPrompt is a connection someone still has to make.
type CredentialPrompt struct {
	Slug       string   `json:"slug"`
	Name       string   `json:"name"`
	Scope      string   `json:"scope"`
	Status     string   `json:"status"`
	NeededBy   []string `json:"neededBy"`
	CLICommand string   `json:"cliCommand"`
	ConnectURL string   `json:"connectUrl"`
}

// Plan is what installing, updating or removing a pack would do.
type Plan struct {
	PlanID   string `json:"planId"`
	PlanHash string `json:"planHash"`
	Mode     string `json:"mode"`
	Pack     struct {
		Slug    string `json:"slug"`
		Version string `json:"version"`
		Name    string `json:"name"`
	} `json:"pack"`
	Params            map[string]any     `json:"params"`
	Ops               []Op               `json:"ops"`
	CredentialPrompts []CredentialPrompt `json:"credentialPrompts"`
	Summary           struct {
		Apply     int `json:"apply"`
		Noop      int `json:"noop"`
		Conflict  int `json:"conflict"`
		Skipped   int `json:"skipped"`
		Loosening int `json:"loosening"`
	} `json:"summary"`
	ExpiresAt string `json:"expiresAt"`
}

// Conflicts lists the sequence numbers of operations that need a decision.
func (p *Plan) Conflicts() []int {
	var seqs []int
	for _, op := range p.Ops {
		if op.Status == StatusConflict {
			seqs = append(seqs, op.Seq)
		}
	}
	return seqs
}

// Loosens reports whether any operation that would be applied loosens a
// control.
func (p *Plan) Loosens() bool {
	for _, op := range p.Ops {
		if op.Loosens && op.Status != StatusNoop && !strings.HasPrefix(op.Status, "skipped") {
			return true
		}
	}
	return false
}

// areas groups operation kinds the way the dashboard does.
var areas = []struct {
	title string
	kinds []string
}{
	{"Connectors", []string{"connector.enable", "connector.prompt"}},
	{"Groups", []string{"group.create", "group.map"}},
	{"Skills and bundles", []string{"resource.enable", "resource.assign"}},
	{"Risk and approvals", []string{"risk.cell", "risk.override", "approvals.approver_group", "approvals.grant_max"}},
	{"Automations", []string{"automation.create"}},
	{"Member rules", []string{"sync_profile.upsert"}},
}

var marks = map[string]string{
	StatusApply: "+", StatusNoop: "=", StatusConflict: "!",
	StatusSkippedPlan: "-", StatusSkippedPlatform: "-", StatusSkippedMissing: "-",
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "(none)"
	}
	return textsafe.Clean(string(raw))
}

// TargetText writes an operation target as sorted key=value pairs.
func TargetText(t map[string]string) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + textsafe.Clean(t[k])
	}
	return strings.Join(parts, " ")
}

// Render writes the plan grouped by area: one line per operation with its
// change, and a note for conflicts and skipped operations.
func (p *Plan) Render(w io.Writer) {
	fmt.Fprintf(w, "%s %s %s\n", textsafe.Clean(p.Mode), textsafe.Clean(p.Pack.Slug), textsafe.Clean(p.Pack.Version))
	known := map[string]bool{}
	for _, a := range areas {
		var lines []Op
		for _, op := range p.Ops {
			for _, k := range a.kinds {
				if op.Kind == k {
					lines = append(lines, op)
					known[op.Kind] = true
				}
			}
		}
		renderArea(w, a.title, lines)
	}
	var rest []Op
	for _, op := range p.Ops {
		if !known[op.Kind] {
			rest = append(rest, op)
		}
	}
	renderArea(w, "Other", rest)
	s := p.Summary
	fmt.Fprintf(w, "\n%d to apply, %d unchanged, %d conflicts, %d skipped", s.Apply, s.Noop, s.Conflict, s.Skipped)
	if s.Loosening > 0 {
		fmt.Fprintf(w, ", %d loosen a control", s.Loosening)
	}
	fmt.Fprintln(w)
}

func renderArea(w io.Writer, title string, ops []Op) {
	if len(ops) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, op := range ops {
		loosen := ""
		if op.Loosens {
			loosen = "  [loosens]"
		}
		fmt.Fprintf(w, "  %s %-3d %s %s%s\n", marks[op.Status], op.Seq, textsafe.Clean(op.Kind), TargetText(op.Target), loosen)
		if op.Status == StatusApply || op.Status == StatusConflict {
			fmt.Fprintf(w, "        %s -> %s\n", compactJSON(op.Before), compactJSON(op.After))
		}
		switch {
		case op.Status == StatusConflict:
			fmt.Fprintf(w, "        changed since the pack was installed; keep your value or take the pack's (%s)\n", textsafe.Clean(op.Reason))
		case strings.HasPrefix(op.Status, "skipped"):
			why := textsafe.Clean(op.Reason)
			if op.Requires != nil && op.Requires.RequiredPlan != "" {
				why += " (needs the " + textsafe.Clean(op.Requires.RequiredPlan) + " plan)"
			}
			fmt.Fprintf(w, "        skipped: %s\n", strings.TrimSpace(why))
		}
	}
}
