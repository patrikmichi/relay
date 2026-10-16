package mcpwrap

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	snapshotTTL     = 60 * time.Second
	auditBatchSize  = 50
	auditBatchEvery = 10 * time.Second
	auditQueueLimit = 5000
	fastPrefix      = "fast-"
)

// policySnapshot is the gateway's signed statement of which tools of an
// upstream may be decided on this machine.
type policySnapshot struct {
	Upstream  string    `json:"upstream"`
	Digest    string    `json:"digest"`
	ExpiresAt time.Time `json:"expiresAt"`
	Tools     []struct {
		Name string `json:"name"`
		Hash string `json:"hash"`
		Risk string `json:"risk"`
	} `json:"tools"`
	Deny []string `json:"deny"`
}

func (g *OnlineGovernor) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *OnlineGovernor) isFast(d Decision) bool { return strings.HasPrefix(d.CallID, fastPrefix) }

// snapshotFor returns a fresh snapshot, fetching one when the cached copy is
// older than the time to live or past its own expiry.
func (g *OnlineGovernor) snapshotFor(ctx context.Context) *policySnapshot {
	g.fast.Lock()
	defer g.fast.Unlock()
	now := g.now()
	if g.snap != nil && now.Before(g.snapUntil) {
		return g.snap
	}
	g.snap = nil
	r, err := g.API.Exchange(ctx, http.MethodPost, "/api/fw/local/snapshot", map[string]any{"upstream": g.sess.Upstream.ID})
	if err != nil || r.Status < 200 || r.Status > 299 {
		return nil
	}
	if g.Verify != nil && g.Verify(r) != nil {
		return nil
	}
	var s policySnapshot
	if json.Unmarshal(r.Body, &s) != nil || s.Upstream != g.sess.Upstream.ID || !now.Before(s.ExpiresAt) {
		return nil
	}
	until := now.Add(snapshotTTL)
	if s.ExpiresAt.Before(until) {
		until = s.ExpiresAt
	}
	g.snap, g.snapUntil = &s, until
	return g.snap
}

// fastRead decides a call to a read tool without asking the gateway: the tool
// has to be pinned with the hash the signed snapshot lists, classified read and
// not denied. Anything else, and any doubt, goes to the gateway.
func (g *OnlineGovernor) fastRead(ctx context.Context, tool string, pin sessionPin, args json.RawMessage) (Decision, bool) {
	if !g.sess.Settings.FastReads || pin.Risk != "read" {
		return Decision{}, false
	}
	s := g.snapshotFor(ctx)
	if s == nil {
		return Decision{}, false
	}
	for _, glob := range s.Deny {
		if ok, _ := path.Match(glob, tool); ok {
			return Decision{}, false
		}
	}
	for _, t := range s.Tools {
		if t.Name == tool && t.Hash == pin.Hash && t.Risk == "read" {
			d := Decision{Kind: "allow", CallID: fastPrefix + randomID()}
			g.record(map[string]any{"event": "call", "callId": d.CallID, "tool": tool, "risk": "read", "decision": "allow", "argumentsDigest": digestOf(args), "snapshot": s.Digest})
			return d, true
		}
	}
	return Decision{}, false
}

// fastResult screens a result locally; the findings are audited with the call.
func (g *OnlineGovernor) fastResult(d Decision, tool string, result json.RawMessage, isErr bool) ResultDecision {
	var findings []Finding
	for _, t := range resultTexts(result) {
		findings = append(findings, ScreenText(t)...)
	}
	worst := MaxSeverity(findings)
	g.record(map[string]any{"event": "result", "callId": d.CallID, "tool": tool, "error": isErr, "findings": len(findings), "severity": worst, "digest": digestOf(result)})
	if worst == SevCritical {
		return ResultDecision{Kind: "withhold", Message: "it contains instructions aimed at the model (" + findings[0].Rule + ")"}
	}
	if cleaned := StripInvisible(string(result)); cleaned != string(result) && json.Valid([]byte(cleaned)) {
		return ResultDecision{Kind: "redact", Content: json.RawMessage(cleaned)}
	}
	return ResultDecision{Kind: "allow"}
}

func (g *OnlineGovernor) record(entry map[string]any) {
	entry["at"] = g.now().UTC().Format(time.RFC3339Nano)
	g.fast.Lock()
	g.queue = append(g.queue, entry)
	if over := len(g.queue) - auditQueueLimit; over > 0 {
		g.queue = g.queue[over:]
	}
	full := len(g.queue) >= auditBatchSize && !g.now().Before(g.retryAt)
	g.fast.Unlock()
	if full {
		g.flush(context.Background())
	}
}

// flush sends the queued audit entries in one request; a failed send keeps
// them for the next one.
func (g *OnlineGovernor) flush(ctx context.Context) {
	g.fast.Lock()
	batch := g.queue
	g.queue = nil
	g.fast.Unlock()
	if len(batch) == 0 {
		return
	}
	r, err := g.API.Exchange(ctx, http.MethodPost, "/api/fw/local/audit-batch", map[string]any{"upstream": g.sess.Upstream.ID, "sessionRef": g.sessionRef, "entries": batch})
	if err == nil && r.Status >= 200 && r.Status <= 299 {
		return
	}
	g.logf("the audit of %d local decisions could not be sent; it will be retried", len(batch))
	g.fast.Lock()
	g.retryAt = g.now().Add(auditBatchEvery)
	g.queue = append(batch, g.queue...)
	if over := len(g.queue) - auditQueueLimit; over > 0 {
		g.queue = g.queue[over:]
	}
	g.fast.Unlock()
}

// startFlusher sends the audit queue on a timer until Close.
func (g *OnlineGovernor) startFlusher() {
	every := g.FlushEvery
	if every <= 0 {
		every = auditBatchEvery
	}
	g.stop, g.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(g.stopped)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				g.flush(context.Background())
			case <-g.stop:
				return
			}
		}
	}()
}

// Close stops the timer and sends what is still queued.
func (g *OnlineGovernor) Close() {
	if g.stop == nil {
		return
	}
	close(g.stop)
	<-g.stopped
	g.stop = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.flush(ctx)
}
