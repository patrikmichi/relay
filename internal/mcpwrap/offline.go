package mcpwrap

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// OfflineGovernor governs a server with no account: pins approved on the
// terminal, a local policy, local screening and a local hash-chained log. It
// has no approvals, since there is no second person to give one.
type OfflineGovernor struct {
	Name   string
	Pins   PinStore
	Policy UpstreamPolicy
	Audit  *AuditLog
	Logf   func(format string, args ...any)

	mu   sync.Mutex
	set  *PinSet
	seen map[string]bool
}

func (g *OfflineGovernor) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

func (g *OfflineGovernor) audit(event string, fields map[string]any) {
	if g.Audit == nil {
		return
	}
	if err := g.Audit.Append(event, fields); err != nil {
		g.logf("the local audit log could not be written: %v", err)
	}
}

// reload takes up approvals made in another terminal while the server runs.
func (g *OfflineGovernor) reload() error {
	set, err := g.Pins.Load(g.Name)
	if err != nil {
		return err
	}
	g.set = set
	return nil
}

// Start loads the pins.
func (g *OfflineGovernor) Start(context.Context) error {
	set, err := g.Pins.Load(g.Name)
	if err != nil {
		return err
	}
	g.set, g.seen = set, map[string]bool{}
	g.audit("start", map[string]any{"upstream": g.Name})
	return nil
}

// Close does nothing; the log is written as events happen.
func (g *OfflineGovernor) Close() {}

// Tools records what the server shows and serves only what is pinned.
func (g *OfflineGovernor) Tools(_ context.Context, live []Definition, final bool) ([]Definition, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.reload(); err != nil {
		return nil, err
	}
	changes := g.set.Observe(live, g.seen, final)
	if final {
		g.seen = map[string]bool{}
	}
	if len(changes) > 0 {
		if err := g.Pins.Save(g.set); err != nil {
			return nil, err
		}
	}
	for _, c := range changes {
		g.audit("definition_"+c.Kind, map[string]any{"tool": c.Tool, "oldHash": c.OldHash, "newHash": c.NewHash, "severity": c.Severity})
		g.logf("%s: tool %s %s (%s); review it with `zuano mcp changes`", g.Name, c.Tool, c.Kind, c.Severity)
	}
	var served []Definition
	for _, def := range live {
		pin := g.set.Tools[nameOf(def)]
		if pin == nil || g.Policy.Denied(pin.Name) {
			continue
		}
		if pin.State == StatePinned || (pin.State == StateQuarantined && g.Policy.OnDefinitionChange == "warn") {
			served = append(served, cleanDefinition(pin.Def).(map[string]any))
		}
	}
	return served, nil
}

// Call allows a pinned tool the policy does not deny.
func (g *OfflineGovernor) Call(_ context.Context, tool string, _ json.RawMessage) (Decision, error) {
	g.mu.Lock()
	err := g.reload()
	pin := g.set.Tools[tool]
	g.mu.Unlock()
	if err != nil {
		return Decision{Kind: "deny", Reason: "tool_not_pinned", Message: "the pins could not be read: " + err.Error()}, nil
	}
	d := Decision{Kind: "allow"}
	switch {
	case pin == nil || pin.State == StatePending || pin.State == StateRejected || pin.State == StateRemoved:
		d = Decision{Kind: "deny", Reason: "tool_not_pinned", Message: "approve it first with `zuano mcp pins approve " + g.Name + " " + tool + "`"}
	case pin.State == StateQuarantined && g.Policy.OnDefinitionChange != "warn":
		d = Decision{Kind: "deny", Reason: "tool_definition_changed", Message: "the server changed this tool; review it with `zuano mcp changes`"}
	case g.Policy.Denied(tool):
		d = Decision{Kind: "deny", Reason: "denied_by_policy", Message: "firewall.yaml denies this tool"}
	}
	risk := "write"
	if pin != nil {
		risk = g.Policy.Classify(pin.Def)
	}
	g.audit("call", map[string]any{"tool": tool, "risk": risk, "decision": d.Kind, "reason": d.Reason})
	return d, nil
}

// resultTexts collects the text blocks of a tool result.
func resultTexts(result json.RawMessage) []string {
	var body struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Message string `json:"message"`
	}
	if json.Unmarshal(result, &body) != nil {
		return []string{string(result)}
	}
	var out []string
	for _, c := range body.Content {
		if c.Type == "text" && c.Text != "" {
			out = append(out, c.Text)
		}
	}
	if body.Message != "" {
		out = append(out, body.Message)
	}
	return out
}

// Result withholds a result that tells the model what to do, and strips
// invisible characters from one that is let through.
func (g *OfflineGovernor) Result(_ context.Context, _ Decision, tool string, result json.RawMessage, isErr bool) (ResultDecision, error) {
	var findings []Finding
	for _, t := range resultTexts(result) {
		findings = append(findings, ScreenText(t)...)
	}
	worst := MaxSeverity(findings)
	g.audit("result", map[string]any{"tool": tool, "error": isErr, "findings": len(findings), "severity": worst})
	if worst == SevCritical && g.Policy.Inbound == "withhold" {
		return ResultDecision{Kind: "withhold", Message: "it contains instructions aimed at the model (" + findings[0].Rule + ")"}, nil
	}
	if cleaned := StripInvisible(string(result)); cleaned != string(result) && json.Valid([]byte(cleaned)) {
		return ResultDecision{Kind: "redact", Content: json.RawMessage(cleaned)}, nil
	}
	if worst != "" {
		g.logf("%s: the result of %s was flagged (%s) but let through", g.Name, tool, strings.ToLower(worst))
	}
	return ResultDecision{Kind: "allow"}, nil
}
