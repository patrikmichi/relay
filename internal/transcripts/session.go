// Package transcripts reads the conversation files coding agents and chat apps
// write and turns each into one normalized session, on this machine. The raw
// file never leaves it: only the normalized, redacted session is uploaded.
package transcripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/signing"
)

// Format is the normalized session format.
const Format = "zuano.session/v1"

// Sources a reader produces.
const (
	SourceClaudeCode = "claude_code"
	SourceCodex      = "codex"
	SourceGemini     = "gemini_cli"
	SourceOpenCode   = "opencode"
	SourceCline      = "cline"
	SourceClaudeAI   = "claude_ai_export"
	SourceChatGPT    = "chatgpt_export"
	SourceMarkdown   = "markdown"
	SourcePlainText  = "plain_text"
)

// Limits of a normalized session.
const (
	MaxTurns       = 400
	MaxTurnChars   = 8000
	MaxArgsChars   = 1000
	MaxResultChars = 1500
	maxTitleChars  = 120
	truncMarker    = "…[truncated]"
	attachmentText = "[attachment omitted]"
)

// Tool call kinds.
const (
	KindGateway  = "gateway"
	KindMCPOther = "mcp_other"
	KindBuiltin  = "builtin"
)

// Session is a conversation reduced to what a skill can be drafted from.
type Session struct {
	Format    string     `json:"format"`
	Source    string     `json:"source"`
	Agent     *AgentInfo `json:"agent,omitempty"`
	Title     string     `json:"title,omitempty"`
	StartedAt string     `json:"startedAt,omitempty"`
	EndedAt   string     `json:"endedAt,omitempty"`
	CwdHint   string     `json:"cwdHint,omitempty"`
	Turns     []Turn     `json:"turns"`
	Stats     Stats      `json:"stats"`
	Redaction Redaction  `json:"redaction"`
	// Warnings are for the person running the command and are not uploaded.
	Warnings []string `json:"-"`
}

// AgentInfo names the program that wrote the conversation.
type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Turn is one message.
type Turn struct {
	I         int        `json:"i"`
	Role      string     `json:"role"`
	Text      string     `json:"text"`
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	Sidechain bool       `json:"sidechain,omitempty"`
}

// ToolCall is one use of a tool.
type ToolCall struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name"`
	Service       string `json:"service,omitempty"`
	Tool          string `json:"tool,omitempty"`
	Kind          string `json:"kind"`
	ArgsPreview   string `json:"argsPreview"`
	ResultPreview string `json:"resultPreview,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
}

// Stats counts what the session holds and what was left out.
type Stats struct {
	Turns          int `json:"turns"`
	ToolCalls      int `json:"toolCalls"`
	DroppedParts   int `json:"droppedParts"`
	TruncatedParts int `json:"truncatedParts"`
}

// Redaction says what was removed, so both ends can show it.
type Redaction struct {
	RulesVersion   string         `json:"rulesVersion"`
	ClientRedacted bool           `json:"clientRedacted"`
	Counts         map[string]int `json:"counts"`
	Placeholders   int            `json:"placeholders"`
}

// Canonical is the session as canonical JSON: keys sorted at every depth, no
// whitespace. The gateway's readers produce the same bytes for the same file.
func (s *Session) Canonical() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return signing.CanonicalJSON(doc)
}

// Trim keeps turns from..to (their original numbers, 1-based and inclusive; 0
// means the start or the end). Without a range it keeps the last MaxTurns.
func (s *Session) Trim(from, to int) error {
	if from < 0 || to < 0 || (to > 0 && from > to) {
		return fmt.Errorf("invalid turn range %d-%d", from, to)
	}
	var kept []Turn
	for _, t := range s.Turns {
		n := t.I + 1
		if (from == 0 || n >= from) && (to == 0 || n <= to) {
			kept = append(kept, t)
		}
	}
	if from == 0 && to == 0 && len(kept) > MaxTurns {
		kept = kept[len(kept)-MaxTurns:]
	}
	if len(kept) == 0 {
		return errors.New("the turn range selects no turns")
	}
	if len(kept) > MaxTurns {
		return fmt.Errorf("the range holds %d turns; the limit is %d", len(kept), MaxTurns)
	}
	s.Turns = kept
	s.refreshStats()
	return nil
}

func (s *Session) refreshStats() {
	s.Stats.Turns, s.Stats.ToolCalls = len(s.Turns), 0
	for _, t := range s.Turns {
		s.Stats.ToolCalls += len(t.ToolCalls)
	}
}

// builder accumulates turns while a reader walks its file.
type builder struct {
	s     *Session
	calls map[string][2]int
}

func newBuilder(source string) *builder {
	return &builder{s: &Session{Format: Format, Source: source, Turns: []Turn{}, Redaction: Redaction{Counts: map[string]int{}}}, calls: map[string][2]int{}}
}

func (b *builder) clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	b.s.Stats.TruncatedParts++
	return string([]rune(s)[:max]) + truncMarker
}

func (b *builder) drop() { b.s.Stats.DroppedParts++ }

func (b *builder) attachment() string { b.drop(); return attachmentText }

// turn adds a message. Consecutive assistant messages of the same kind are one
// turn, since agents log each content block of a reply on its own line. It
// returns the turn's index, or -1 when there was nothing to add.
func (b *builder) turn(role, text string, sidechain bool) int {
	text = strings.TrimSpace(text)
	if n := len(b.s.Turns); n > 0 && role == "assistant" {
		last := &b.s.Turns[n-1]
		if last.Role == "assistant" && last.Sidechain == sidechain {
			if text != "" {
				joined := strings.TrimSpace(last.Text + "\n" + text)
				last.Text = b.clip(joined, MaxTurnChars)
			}
			return n - 1
		}
	}
	if text == "" && role != "assistant" {
		return -1
	}
	b.s.Turns = append(b.s.Turns, Turn{I: len(b.s.Turns), Role: role, Text: b.clip(text, MaxTurnChars), Sidechain: sidechain})
	return len(b.s.Turns) - 1
}

// call adds a tool call to a turn and remembers its id so the result finds it.
func (b *builder) call(turn int, c ToolCall) {
	if turn < 0 {
		turn = b.turn("assistant", "", false)
	}
	t := &b.s.Turns[turn]
	if c.ArgsPreview == "" {
		c.ArgsPreview = "{}"
	}
	t.ToolCalls = append(t.ToolCalls, c)
	if c.ID != "" {
		b.calls[c.ID] = [2]int{turn, len(t.ToolCalls) - 1}
	}
}

// result attaches a tool's output to the call with that id.
func (b *builder) result(id, output string, isErr bool) {
	pos, ok := b.calls[id]
	if !ok {
		return
	}
	c := &b.s.Turns[pos[0]].ToolCalls[pos[1]]
	c.ResultPreview = b.clip(output, MaxResultChars)
	c.Outcome = "ok"
	if isErr {
		c.Outcome = "error"
	}
}

func (b *builder) args(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		if json.Valid([]byte(s)) {
			var any1 any
			_ = json.Unmarshal([]byte(s), &any1)
			v = any1
		} else {
			return b.clip(s, MaxArgsChars)
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return b.clip(string(raw), MaxArgsChars)
}

// finish sets the title, the counts and returns the session.
func (b *builder) finish() *Session {
	s := b.s
	for _, t := range s.Turns {
		if s.Title == "" && t.Role == "user" && t.Text != "" {
			title := t.Text
			if i := strings.IndexByte(title, '\n'); i >= 0 {
				title = title[:i]
			}
			if utf8.RuneCountInString(title) > maxTitleChars {
				title = string([]rune(title)[:maxTitleChars])
			}
			s.Title = title
			break
		}
	}
	s.refreshStats()
	return s
}

// classify resolves a tool name the agent saw. A tool of the gateway's MCP
// server (the server name zuano sync writes) is a gateway call whose tool name
// is "service.tool"; any other MCP server's is mcp_other.
func classify(name string) ToolCall {
	c := ToolCall{Name: name, Kind: KindBuiltin}
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return c
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok {
		c.Kind = KindMCPOther
		return c
	}
	if server != brand.Name {
		c.Kind = KindMCPOther
		return c
	}
	c.Kind = KindGateway
	if svc, t, ok := strings.Cut(tool, "."); ok {
		c.Service, c.Tool = svc, t
	} else {
		c.Tool = tool
	}
	return c
}

// classifyServerTool is classify for agents that give the server and the tool
// as separate fields.
func classifyServerTool(server, tool string) ToolCall {
	return classify("mcp__" + server + "__" + tool)
}

// hint reduces a working directory to its last element, never a full path.
func hint(cwd string) string {
	cwd = strings.TrimRight(strings.ReplaceAll(cwd, `\`, "/"), "/")
	if i := strings.LastIndexByte(cwd, '/'); i >= 0 {
		return cwd[i+1:]
	}
	return cwd
}
