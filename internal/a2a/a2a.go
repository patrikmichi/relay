// Package a2a holds the types and terminal rendering for agent-to-agent
// governance: the registry of callable agents, tasks sent through the gateway,
// call chains and the agent graph. Every string comes from the gateway or from
// an agent card, so everything printed is cleaned first.
package a2a

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

// Skill is one thing an agent can do, as its pinned card describes it.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Risk        string `json:"risk"`
	Source      string `json:"classification_source"`
}

// Agent is an entry of the registry.
type Agent struct {
	ID             string   `json:"id"`
	Slug           string   `json:"slug"`
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	Status         string   `json:"status"`
	Exposure       []string `json:"exposure"`
	Skills         []Skill  `json:"skills"`
	CardState      string   `json:"card_state"`
	Signature      string   `json:"signature"`
	PendingChanges int      `json:"pending_changes"`
	OnCardChange   string   `json:"on_card_change"`
	CardURL        string   `json:"card_url"`
	Endpoint       string   `json:"endpoint"`
	Residency      []string `json:"residency"`
	DLPCategories  []string `json:"dlp_categories"`
}

// External reports an agent outside the organisation.
func (a Agent) External() bool { return a.Kind == "external" }

// Part is one piece of a message or artifact.
type Part struct {
	Text   string `json:"text,omitempty"`
	FileID string `json:"file_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Data   any    `json:"data,omitempty"`
}

// Artifact is something a task produced.
type Artifact struct {
	Name  string `json:"name"`
	Parts []Part `json:"parts"`
}

// Approval is a held effect somewhere below a task.
type Approval struct {
	ID  string `json:"approval_id"`
	URL string `json:"approval_url"`
	// Effect names what is held.
	Effect struct {
		Service string `json:"service"`
		Tool    string `json:"tool"`
		Risk    string `json:"risk"`
	} `json:"effect"`
}

// Task is the state of one message sent to an agent.
type Task struct {
	ID         string     `json:"id"`
	ContextID  string     `json:"context_id"`
	State      string     `json:"state"`
	Reason     string     `json:"reason"`
	Message    string     `json:"message"`
	Artifacts  []Artifact `json:"artifacts"`
	Approval   *Approval  `json:"approval"`
	ConnectURL string     `json:"connect_url"`
}

// Terminal reports a state the task will not leave.
func (t Task) Terminal() bool {
	switch t.State {
	case "COMPLETED", "FAILED", "CANCELED", "REJECTED":
		return true
	}
	return false
}

// Texts lists the text parts of a task's message and artifacts.
func (t Task) Texts() []string {
	var out []string
	if t.Message != "" {
		out = append(out, t.Message)
	}
	for _, a := range t.Artifacts {
		for _, p := range a.Parts {
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
	}
	return out
}

// Effect is a tool call made inside a hop.
type Effect struct {
	Service  string `json:"service"`
	Tool     string `json:"tool"`
	Risk     string `json:"risk"`
	Approval string `json:"approval_id"`
	Outcome  string `json:"outcome"`
}

// Hop is one delegation inside a chain.
type Hop struct {
	ID             string   `json:"id"`
	Parent         string   `json:"parent"`
	Depth          int      `json:"depth"`
	Caller         string   `json:"caller"`
	Callee         string   `json:"callee"`
	Skill          string   `json:"skill"`
	External       bool     `json:"external"`
	State          string   `json:"state"`
	DurationMs     int64    `json:"duration_ms"`
	Tokens         int64    `json:"tokens"`
	CostEUR        float64  `json:"cost_eur"`
	DLPFindings    int      `json:"dlp_findings"`
	InjectionFlags int      `json:"injection_flags"`
	Effects        []Effect `json:"effects"`
}

// Budget is a chain's limits and what is used.
type Budget struct {
	Limits Counters `json:"limits"`
	Used   Counters `json:"used"`
}

// Counters are the measured quantities of a chain budget.
type Counters struct {
	Tokens    int64   `json:"tokens"`
	CostEUR   float64 `json:"cost_eur"`
	ToolCalls int64   `json:"tool_calls"`
}

// Chain is a delegation tree.
type Chain struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Origin    string `json:"origin"`
	Depth     int    `json:"depth"`
	StartedAt string `json:"started_at"`
	Hops      []Hop  `json:"hops"`
	Budget    Budget `json:"budget"`
	Audit     struct {
		Verified bool   `json:"verified"`
		Note     string `json:"note"`
	} `json:"audit"`
}

func clean(s string) string { return textsafe.Strict(s) }

func dur(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return (time.Duration(ms) * time.Millisecond).Round(10 * time.Millisecond).String()
}

// Render prints the chain as a tree under a summary line.
func (c Chain) Render(w io.Writer) {
	audit := "audit not verified"
	if c.Audit.Verified {
		audit = "audit verified"
	}
	fmt.Fprintf(w, "chain %s  %s  origin %s  depth %d  %s\n", clean(c.ID), clean(c.Status), clean(c.Origin), c.Depth, audit)
	if c.Budget.Limits.Tokens > 0 || c.Budget.Limits.CostEUR > 0 {
		fmt.Fprintf(w, "budget: tokens %d of %d, cost %.2f of %.2f EUR, tool calls %d of %d\n",
			c.Budget.Used.Tokens, c.Budget.Limits.Tokens, c.Budget.Used.CostEUR, c.Budget.Limits.CostEUR, c.Budget.Used.ToolCalls, c.Budget.Limits.ToolCalls)
	}
	children := map[string][]Hop{}
	known := map[string]bool{}
	for _, h := range c.Hops {
		known[h.ID] = true
	}
	for _, h := range c.Hops {
		parent := h.Parent
		if !known[parent] {
			parent = ""
		}
		children[parent] = append(children[parent], h)
	}
	for _, list := range children {
		sort.SliceStable(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	}
	var walk func(parent, prefix string, seen map[string]bool)
	walk = func(parent, prefix string, seen map[string]bool) {
		hops := children[parent]
		for i, h := range hops {
			if seen[h.ID] {
				continue
			}
			seen[h.ID] = true
			last := i == len(hops)-1
			branch, next := "├─ ", prefix+"│  "
			if last {
				branch, next = "└─ ", prefix+"   "
			}
			where := "internal"
			if h.External {
				where = "EXTERNAL"
			}
			fmt.Fprintf(w, "%s%s%s → %s.%s  %s  %s  %s  tokens %d  %.2f EUR", prefix, branch, clean(h.Caller), clean(h.Callee), clean(h.Skill), where, clean(h.State), dur(h.DurationMs), h.Tokens, h.CostEUR)
			if h.DLPFindings > 0 {
				fmt.Fprintf(w, "  dlp %d", h.DLPFindings)
			}
			if h.InjectionFlags > 0 {
				fmt.Fprintf(w, "  injection %d", h.InjectionFlags)
			}
			fmt.Fprintln(w)
			for _, e := range h.Effects {
				held := ""
				if e.Approval != "" {
					held = "  approval " + clean(e.Approval)
				}
				fmt.Fprintf(w, "%s   effect %s.%s  %s  %s%s\n", next, clean(e.Service), clean(e.Tool), clean(e.Risk), clean(e.Outcome), held)
			}
			walk(h.ID, next, seen)
		}
	}
	walk("", "", map[string]bool{})
}

// Node is a caller or a callable agent in the graph.
type Node struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	External bool   `json:"external"`
}

// Edge is a policy edge or observed traffic between two nodes.
type Edge struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Allowed   bool    `json:"allowed"`
	Rule      string  `json:"rule"`
	Calls     int     `json:"calls"`
	Refusals  int     `json:"refusals"`
	Approvals int     `json:"approvals"`
	SpendEUR  float64 `json:"spend_eur"`
}

// Graph is the agent graph.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(clean(s)) + `"`
}

// DOT renders the graph for Graphviz: external agents are boxes, allowed
// edges dashed, observed traffic solid, edges with refusals red.
func (g Graph) DOT() string {
	var b strings.Builder
	b.WriteString("digraph agents {\n  rankdir=LR;\n")
	nodes := append([]Node(nil), g.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, n := range nodes {
		shape := "ellipse"
		if n.External {
			shape = "box"
		}
		name := n.Name
		if name == "" {
			name = n.ID
		}
		fmt.Fprintf(&b, "  %s [label=%s shape=%s];\n", quote(n.ID), quote(name+" ("+n.Kind+")"), shape)
	}
	edges := append([]Edge(nil), g.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	for _, e := range edges {
		attrs := []string{}
		label := e.Rule
		if e.Calls > 0 {
			label = fmt.Sprintf("%s %d calls", e.Rule, e.Calls)
		}
		attrs = append(attrs, "label="+quote(strings.TrimSpace(label)))
		if e.Allowed && e.Calls == 0 {
			attrs = append(attrs, "style=dashed")
		}
		if e.Refusals > 0 {
			attrs = append(attrs, "color=red")
		}
		fmt.Fprintf(&b, "  %s -> %s [%s];\n", quote(e.From), quote(e.To), strings.Join(attrs, " "))
	}
	b.WriteString("}\n")
	return b.String()
}
