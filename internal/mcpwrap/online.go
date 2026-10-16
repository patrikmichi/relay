package mcpwrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sync"

	"github.com/patrikmichi/zuano/internal/signing"
)

// Reply is a gateway response.
type Reply struct {
	Status int
	Header http.Header
	Body   []byte
}

// API is how the online governor reaches the gateway.
type API interface {
	Exchange(ctx context.Context, method, path string, body any) (Reply, error)
}

type sessionPin struct {
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Hash       string     `json:"hash"`
	State      string     `json:"state"`
	Risk       string     `json:"risk"`
	Definition Definition `json:"definition"`
}

type session struct {
	Upstream struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"upstream"`
	Status   string `json:"status"`
	AdminURL string `json:"adminUrl"`
	Settings struct {
		OnDefinitionChange string   `json:"onDefinitionChange"`
		Offline            string   `json:"offline"`
		Unknown            string   `json:"unknown"`
		DenyTools          []string `json:"denyTools"`
	} `json:"settings"`
	Pins []sessionPin `json:"pins"`
}

// Refusal is the gateway's refusal to let a server run at all.
type Refusal struct{ Reason, Message string }

func (r *Refusal) Error() string { return r.Message }

// OnlineGovernor governs a server through the gateway: pins, policy,
// approvals, DLP and audit are the gateway's.
type OnlineGovernor struct {
	API    API
	Verify func(Reply) error
	// Upstream is the server's slug in the gateway, when the agent's config
	// names one.
	Upstream    string
	Fingerprint string
	PackageSpec string
	InstallID   string
	AgentID     string
	// Screen is gateway (arguments and results go to the gateway) or local
	// (only digests and local findings do).
	Screen string
	Logf   func(format string, args ...any)

	mu         sync.Mutex
	sess       session
	pins       map[string]sessionPin
	seen       map[string]bool
	sessionRef string
}

func (g *OnlineGovernor) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start opens the session and takes the signed pin set.
func (g *OnlineGovernor) Start(ctx context.Context) error {
	g.sessionRef, g.seen = randomID(), map[string]bool{}
	r, err := g.API.Exchange(ctx, http.MethodPost, "/api/fw/local/session", map[string]any{
		"slug": g.Upstream, "fingerprint": g.Fingerprint, "packageSpec": g.PackageSpec, "installId": g.InstallID, "agentId": g.AgentID,
	})
	if err != nil {
		return fmt.Errorf("open a firewall session: %w", err)
	}
	if r.Status < 200 || r.Status > 299 {
		return fmt.Errorf("the gateway refused the firewall session (HTTP %d)", r.Status)
	}
	if g.Verify != nil {
		if err := g.Verify(r); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(r.Body, &g.sess); err != nil {
		return fmt.Errorf("the firewall session could not be read: %w", err)
	}
	if g.sess.Status == "quarantined" {
		return &Refusal{Reason: "upstream_quarantined", Message: "this server is quarantined by your organisation" + suffixURL(g.sess.AdminURL)}
	}
	if g.sess.Status == "unknown" && g.sess.Settings.Unknown == "deny" {
		return fmt.Errorf("this server is not known to the firewall and your organisation does not allow unknown servers; ask an administrator%s", suffixURL(g.sess.AdminURL))
	}
	g.pins = map[string]sessionPin{}
	for _, p := range g.sess.Pins {
		if p.Kind == "tool" && p.State == "active" {
			g.pins[p.Name] = p
		}
	}
	return nil
}

func suffixURL(u string) string {
	if u == "" {
		return ""
	}
	return " (" + u + ")"
}

// Close does nothing.
func (g *OnlineGovernor) Close() {}

func (g *OnlineGovernor) denied(tool string) bool {
	for _, glob := range g.sess.Settings.DenyTools {
		if ok, _ := path.Match(glob, tool); ok {
			return true
		}
	}
	return false
}

// Tools serves the gateway's pinned definitions for the tools the server
// still lists unchanged, and reports the differences.
func (g *OnlineGovernor) Tools(ctx context.Context, live []Definition, final bool) ([]Definition, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var served []Definition
	var changes []map[string]any
	for _, def := range live {
		name := nameOf(def)
		hash, err := DefHash(def)
		if name == "" || err != nil {
			continue
		}
		g.seen[name] = true
		pin, ok := g.pins[name]
		switch {
		case !ok:
			changes = append(changes, map[string]any{"kind": "tool", "name": name, "change": "added", "newHash": hash, "definition": def})
		case pin.Hash != hash:
			changes = append(changes, map[string]any{"kind": "tool", "name": name, "change": "modified", "oldHash": pin.Hash, "newHash": hash, "definition": def})
			if g.sess.Settings.OnDefinitionChange == "warn" && !g.denied(name) {
				served = append(served, cleanDefinition(pin.Definition).(map[string]any))
			}
		case !g.denied(name):
			served = append(served, cleanDefinition(pin.Definition).(map[string]any))
		}
	}
	if final {
		for name, pin := range g.pins {
			if !g.seen[name] {
				changes = append(changes, map[string]any{"kind": "tool", "name": name, "change": "removed", "oldHash": pin.Hash})
			}
		}
		g.seen = map[string]bool{}
	}
	if len(changes) > 0 {
		if _, err := g.API.Exchange(ctx, http.MethodPost, "/api/fw/local/changes", map[string]any{"upstream": g.sess.Upstream.ID, "changes": changes}); err != nil {
			g.logf("the changes could not be reported to the gateway: %v", err)
		}
		for _, c := range changes {
			g.logf("tool %v %v: hidden until an administrator reviews it", c["name"], c["change"])
		}
	}
	return served, nil
}

func digestOf(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	canon, err := signing.CanonicalJSON(v)
	if err != nil {
		canon = raw
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// Call asks the gateway; when it cannot be reached, the setting decides.
func (g *OnlineGovernor) Call(ctx context.Context, tool string, args json.RawMessage) (Decision, error) {
	g.mu.Lock()
	pin, pinned := g.pins[tool]
	g.mu.Unlock()
	if g.sess.Status != "active" && g.sess.Status != "" {
		return Decision{Kind: "pending", Message: "The server is waiting for an administrator" + suffixURL(g.sess.AdminURL) + ".", URL: g.sess.AdminURL}, nil
	}
	if !pinned || g.denied(tool) {
		return Decision{Kind: "deny", Reason: "tool_not_pinned", Message: "this tool is not approved"}, nil
	}
	body := map[string]any{"upstream": g.sess.Upstream.ID, "tool": tool, "sessionRef": g.sessionRef}
	if g.Screen == "local" {
		body["argumentsDigest"] = digestOf(args)
		body["localFindings"] = []Finding{}
	} else {
		body["arguments"] = args
	}
	r, err := g.API.Exchange(ctx, http.MethodPost, "/api/fw/local/calls", body)
	if err != nil || r.Status < 200 || r.Status > 299 {
		if g.sess.Settings.Offline == "allow_read_pinned" && pin.Risk == "read" {
			return Decision{Kind: "allow", CallID: "local-" + randomID()}, nil
		}
		reason := "gateway_unreachable"
		if err == nil {
			err = fmt.Errorf("HTTP %d", r.Status)
		}
		return Decision{Kind: "deny", Reason: reason, Message: "the gateway could not be asked (" + err.Error() + ")"}, nil
	}
	var out struct {
		Decision   string          `json:"decision"`
		CallID     string          `json:"callId"`
		Reason     string          `json:"reason"`
		Message    string          `json:"message"`
		ApprovalID string          `json:"approval_id"`
		URL        string          `json:"url"`
		Arguments  json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		return Decision{}, errors.New("the gateway's decision could not be read")
	}
	switch out.Decision {
	case "allow", "deny", "pending", "redact":
		return Decision{Kind: out.Decision, CallID: out.CallID, Reason: out.Reason, Message: out.Message, ApprovalID: out.ApprovalID, URL: out.URL, Arguments: out.Arguments}, nil
	}
	return Decision{}, fmt.Errorf("the gateway answered %q", out.Decision)
}

// Result sends the result for screening; a result that cannot be checked is
// withheld.
func (g *OnlineGovernor) Result(ctx context.Context, d Decision, tool string, result json.RawMessage, isErr bool) (ResultDecision, error) {
	if len(d.CallID) > 6 && d.CallID[:6] == "local-" {
		return ResultDecision{Kind: "allow"}, nil
	}
	body := map[string]any{"error": isErr}
	if g.Screen == "local" {
		var findings []Finding
		for _, t := range resultTexts(result) {
			findings = append(findings, ScreenText(t)...)
		}
		body["digest"], body["localFindings"] = digestOf(result), findings
	} else {
		body["content"] = result
	}
	r, err := g.API.Exchange(ctx, http.MethodPost, "/api/fw/local/calls/"+d.CallID+"/result", body)
	if err != nil || r.Status < 200 || r.Status > 299 {
		return ResultDecision{Kind: "withhold", Message: "the gateway could not check it"}, nil
	}
	var out struct {
		Decision string          `json:"decision"`
		Content  json.RawMessage `json:"content"`
		Message  string          `json:"message"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		return ResultDecision{Kind: "withhold", Message: "the gateway's answer could not be read"}, nil
	}
	switch out.Decision {
	case "allow", "redact", "withhold":
		return ResultDecision{Kind: out.Decision, Content: out.Content, Message: out.Message}, nil
	}
	return ResultDecision{Kind: "withhold", Message: "the gateway's answer was not understood"}, nil
}
