package mcpwrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Decision is a governor's answer for a tool call.
type Decision struct {
	// Kind is allow, deny, pending or redact.
	Kind       string
	CallID     string
	Reason     string
	Message    string
	ApprovalID string
	URL        string
	// Arguments replace the call's when Kind is redact.
	Arguments json.RawMessage
}

// ResultDecision is a governor's answer for a tool result.
type ResultDecision struct {
	// Kind is allow, redact or withhold.
	Kind    string
	Content json.RawMessage
	Message string
}

// Governor decides what the shim serves and relays.
type Governor interface {
	// Start prepares the governor; an error ends the shim before it relays
	// anything.
	Start(ctx context.Context) error
	// Tools returns the definitions to serve for one page of the server's
	// list; final is set on the last page.
	Tools(ctx context.Context, live []Definition, final bool) ([]Definition, error)
	Call(ctx context.Context, tool string, args json.RawMessage) (Decision, error)
	Result(ctx context.Context, d Decision, tool string, result json.RawMessage, isErr bool) (ResultDecision, error)
	Close()
}

// Shim relays MCP between an agent and a server it governs.
type Shim struct {
	Gov    Governor
	Agent  io.Reader
	Out    io.Writer
	Child  io.Reader // the server's stdout
	ChildW io.Writer // the server's stdin
	Logf   func(format string, args ...any)

	toAgent, toChild *LineWriter
	mu               sync.Mutex
	pending          map[string]pending
	seen             map[string]bool
}

type pending struct {
	method string
	tool   string
	call   Decision
}

func (s *Shim) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Shim) take(id json.RawMessage) (pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[string(id)]
	delete(s.pending, string(id))
	return p, ok
}

func (s *Shim) put(id json.RawMessage, p pending) {
	s.mu.Lock()
	s.pending[string(id)] = p
	s.mu.Unlock()
}

// Run relays until the agent or the server closes its side.
func (s *Shim) Run(ctx context.Context) error {
	s.toAgent, s.toChild = NewLineWriter(s.Out), NewLineWriter(s.ChildW)
	s.pending, s.seen = map[string]pending{}, map[string]bool{}
	if err := s.Gov.Start(ctx); err != nil {
		return err
	}
	defer s.Gov.Close()
	errs := make(chan error, 2)
	go func() { errs <- s.fromAgent(ctx) }()
	go func() { errs <- s.fromChild(ctx) }()
	err := <-errs
	if closer, ok := s.ChildW.(io.Closer); ok {
		_ = closer.Close()
	}
	return err
}

func (s *Shim) fromAgent(ctx context.Context) error {
	r := NewLineReader(s.Agent)
	for {
		raw, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		m, ok := parse(raw)
		if !ok {
			continue
		}
		if !m.IsRequest() {
			if err := s.toChild.Raw(raw); err != nil {
				return err
			}
			continue
		}
		if err := s.agentRequest(ctx, m, raw); err != nil {
			return err
		}
	}
}

func (s *Shim) agentRequest(ctx context.Context, m Message, raw []byte) error {
	switch m.Method {
	case "initialize":
		s.put(m.ID, pending{method: m.Method})
		return s.toChild.Raw(dropClientCapabilities(m, raw))
	case "tools/list":
		s.put(m.ID, pending{method: m.Method})
		return s.toChild.Raw(raw)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.Name == "" {
			return s.toAgent.JSON(errorResponse(m.ID, -32602, "tools/call needs a tool name"))
		}
		d, err := s.Gov.Call(ctx, p.Name, p.Arguments)
		if err != nil {
			s.logf("the governor failed on %s: %v", p.Name, err)
			return s.toAgent.JSON(toolResult(m.ID, true, "The call was not made: the firewall could not decide ("+err.Error()+").", nil))
		}
		switch d.Kind {
		case "allow", "redact":
			s.put(m.ID, pending{method: m.Method, tool: p.Name, call: d})
			if d.Kind == "redact" && len(d.Arguments) > 0 {
				return s.toChild.JSON(Message{JSONRPC: "2.0", ID: m.ID, Method: m.Method, Params: mustJSON(map[string]any{"name": p.Name, "arguments": d.Arguments})})
			}
			return s.toChild.Raw(raw)
		case "pending":
			text := "This call needs approval. " + d.Message
			return s.toAgent.JSON(toolResult(m.ID, true, text, map[string]any{"status": "pending_approval", "approval_id": d.ApprovalID, "url": d.URL}))
		default:
			text := "The firewall refused this call"
			if d.Reason != "" {
				text += " (" + d.Reason + ")"
			}
			if d.Message != "" {
				text += ": " + d.Message
			}
			return s.toAgent.JSON(toolResult(m.ID, true, text, map[string]any{"reason": d.Reason}))
		}
	}
	return s.toChild.Raw(raw)
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// dropClientCapabilities removes what lets the server ask the agent for
// anything: sampling, elicitation and roots.
func dropClientCapabilities(m Message, raw []byte) []byte {
	var params map[string]json.RawMessage
	if json.Unmarshal(m.Params, &params) != nil {
		return raw
	}
	var caps map[string]json.RawMessage
	if json.Unmarshal(params["capabilities"], &caps) != nil {
		return raw
	}
	for _, k := range []string{"sampling", "elicitation", "roots"} {
		delete(caps, k)
	}
	params["capabilities"] = mustJSON(caps)
	m.Params = mustJSON(params)
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func (s *Shim) fromChild(ctx context.Context) error {
	r := NewLineReader(s.Child)
	for {
		raw, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		m, ok := parse(raw)
		switch {
		case !ok:
			continue
		case m.IsRequest():
			// The server asking the agent for something: refused.
			if err := s.toChild.JSON(errorResponse(m.ID, -32601, "not available through the firewall")); err != nil {
				return err
			}
		case m.IsResponse():
			if err := s.childResponse(ctx, m, raw); err != nil {
				return err
			}
		default:
			if err := s.toAgent.Raw(raw); err != nil {
				return err
			}
		}
	}
}

func (s *Shim) childResponse(ctx context.Context, m Message, raw []byte) error {
	p, ok := s.take(m.ID)
	if !ok {
		return s.toAgent.Raw(raw)
	}
	switch p.method {
	case "tools/list":
		return s.toolsListResponse(ctx, m, raw)
	case "tools/call":
		isErr := len(m.Error) > 0
		body := m.Result
		if isErr {
			body = m.Error
		}
		rd, err := s.Gov.Result(ctx, p.call, p.tool, body, isErr)
		if err != nil {
			s.logf("the governor failed on the result of %s: %v", p.tool, err)
			return s.toAgent.JSON(toolResult(m.ID, true, "The result was withheld: the firewall could not check it ("+err.Error()+").", nil))
		}
		switch rd.Kind {
		case "redact":
			m.Result = rd.Content
			return s.toAgent.JSON(m)
		case "withhold":
			text := "The result was withheld by the firewall"
			if rd.Message != "" {
				text += ": " + rd.Message
			}
			return s.toAgent.JSON(toolResult(m.ID, true, text, nil))
		}
	}
	return s.toAgent.Raw(raw)
}

func (s *Shim) toolsListResponse(ctx context.Context, m Message, raw []byte) error {
	if len(m.Error) > 0 {
		return s.toAgent.Raw(raw)
	}
	defs, rest, err := decodeDefinitions(m.Result)
	if err != nil {
		return s.toAgent.JSON(errorResponse(m.ID, -32603, fmt.Sprintf("the server's tool list could not be read: %v", err)))
	}
	final := len(rest["nextCursor"]) == 0 || string(rest["nextCursor"]) == "null"
	served, err := s.Gov.Tools(ctx, defs, final)
	if err != nil {
		return s.toAgent.JSON(errorResponse(m.ID, -32603, fmt.Sprintf("the firewall could not check the tool list: %v", err)))
	}
	if served == nil {
		served = []Definition{}
	}
	rest["tools"] = mustJSON(served)
	m.Result = mustJSON(rest)
	return s.toAgent.JSON(m)
}
