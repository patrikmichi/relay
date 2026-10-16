package transcripts

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrikmichi/zuano/internal/brand"
)

// nameCall classifies a tool name for agents that join the MCP server and the
// tool with underscores.
func nameCall(name string) ToolCall {
	var c ToolCall
	switch {
	case strings.HasPrefix(name, "mcp__"):
		c = classify(name)
	case strings.HasPrefix(name, brand.Name+"__"):
		c = classify("mcp__" + name)
	case strings.HasPrefix(name, brand.Name+"_"):
		c = classify("mcp__" + brand.Name + "__" + strings.TrimPrefix(name, brand.Name+"_"))
	default:
		c = classify(name)
	}
	c.Name = name
	return c
}

func decode(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ReadCodex reads a Codex rollout: JSONL lines whose response_item payloads are
// messages, function calls and their outputs. Reasoning and events are dropped.
func ReadCodex(data []byte) (*Session, error) {
	b := newBuilder(SourceCodex)
	b.s.Agent = &AgentInfo{Name: "codex"}
	err := eachJSONLine(data, b, func(m obj) {
		p := asObj(m["payload"])
		if ts := str(m, "timestamp"); ts != "" {
			if b.s.StartedAt == "" {
				b.s.StartedAt = ts
			}
			b.s.EndedAt = ts
		}
		switch str(m, "type") {
		case "session_meta":
			if cwd := str(p, "cwd"); cwd != "" {
				b.s.CwdHint = hint(cwd)
			}
			if v := str(p, "cli_version"); v != "" {
				b.s.Agent.Version = v
			}
		case "turn_context":
			if model := str(p, "model"); model != "" {
				b.s.Agent.Model = model
			}
		case "response_item":
			switch str(p, "type") {
			case "message":
				role := str(p, "role")
				if role != "user" && role != "assistant" {
					b.drop()
					return
				}
				b.turn(role, b.textOf(p["content"]), false)
			case "function_call":
				c := nameCall(str(p, "name"))
				c.ID, c.ArgsPreview = str(p, "call_id"), b.args(p["arguments"])
				b.call(b.lastAssistant(), c)
			case "function_call_output":
				out := p["output"]
				if om := asObj(out); om != nil {
					out = firstNonNil(om["content"], om["output"], out)
				}
				b.result(str(p, "call_id"), b.textOf(out), false)
			case "reasoning":
				b.drop()
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return b.finish(), nil
}

func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// lastAssistant is the turn tool calls attach to: the last one when it is the
// assistant's, otherwise a new one.
func (b *builder) lastAssistant() int {
	if n := len(b.s.Turns); n > 0 && b.s.Turns[n-1].Role == "assistant" {
		return n - 1
	}
	return b.turn("assistant", "", false)
}

// ReadGemini reads a Gemini CLI chat: an array of Content, or an object that
// holds it as history or messages.
func ReadGemini(data []byte) (*Session, error) {
	doc, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("not a Gemini CLI chat: %w", err)
	}
	var items []any
	switch t := doc.(type) {
	case []any:
		items = t
	case obj:
		items = firstList(t, "history", "messages")
	}
	if items == nil {
		return nil, errors.New("not a Gemini CLI chat: no history or messages")
	}
	b := newBuilder(SourceGemini)
	b.s.Agent = &AgentInfo{Name: "gemini-cli"}
	pending := map[string][]string{}
	n := 0
	for _, it := range items {
		m := asObj(it)
		role := firstString(str(m, "role"), str(m, "type"))
		switch role {
		case "user":
			role = "user"
		case "model", "gemini", "assistant":
			role = "assistant"
		default:
			b.drop()
			continue
		}
		if s, ok := m["content"].(string); ok {
			idx := b.turn(role, s, false)
			for _, tc := range list(m, "toolCalls") {
				tm := asObj(tc)
				c := nameCall(str(tm, "name"))
				c.ID, c.ArgsPreview = str(tm, "id"), b.args(tm["args"])
				b.call(idx, c)
				b.result(c.ID, b.textOf(tm["result"]), str(tm, "status") == "error")
			}
			continue
		}
		var text []string
		var calls []obj
		for _, p := range list(m, "parts") {
			pm := asObj(p)
			switch {
			case pm["text"] != nil:
				text = append(text, str(pm, "text"))
			case pm["functionCall"] != nil:
				calls = append(calls, asObj(pm["functionCall"]))
			case pm["functionResponse"] != nil:
				fr := asObj(pm["functionResponse"])
				name := str(fr, "name")
				if ids := pending[name]; len(ids) > 0 {
					b.result(ids[0], b.args(fr["response"]), false)
					pending[name] = ids[1:]
				}
			default:
				text = append(text, b.attachment())
			}
		}
		idx := b.turn(role, strings.Join(text, "\n"), false)
		for _, fc := range calls {
			n++
			c := nameCall(str(fc, "name"))
			c.ID, c.ArgsPreview = "g"+strconv.Itoa(n), b.args(fc["args"])
			pending[c.Name] = append(pending[c.Name], c.ID)
			b.call(idx, c)
		}
	}
	return b.finish(), nil
}

func firstList(m obj, keys ...string) []any {
	for _, k := range keys {
		if l, ok := m[k].([]any); ok {
			return l
		}
	}
	return nil
}

func firstString(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// ReadOpenCode reads the output of `opencode export`: the session info and
// messages made of text and tool parts.
func ReadOpenCode(data []byte) (*Session, error) {
	doc, err := decode(data)
	root := asObj(doc)
	if err != nil || root == nil || root["messages"] == nil {
		return nil, errors.New("not an OpenCode export: expected an object with messages (the output of `opencode export <session>`)")
	}
	b := newBuilder(SourceOpenCode)
	b.s.Agent = &AgentInfo{Name: "opencode"}
	if d := str(asObj(root["info"]), "directory"); d != "" {
		b.s.CwdHint = hint(d)
	}
	for _, mv := range list(root, "messages") {
		m := asObj(mv)
		info := asObj(m["info"])
		role := str(info, "role")
		if role != "user" && role != "assistant" {
			b.drop()
			continue
		}
		if created, ok := asObj(info["time"])["created"].(float64); ok {
			ts := time.UnixMilli(int64(created)).UTC().Format(time.RFC3339)
			if b.s.StartedAt == "" {
				b.s.StartedAt = ts
			}
			b.s.EndedAt = ts
		}
		var text []string
		var tools []obj
		for _, pv := range list(m, "parts") {
			p := asObj(pv)
			switch str(p, "type") {
			case "text":
				text = append(text, str(p, "text"))
			case "tool":
				tools = append(tools, p)
			case "file":
				text = append(text, b.attachment())
			default:
				b.drop()
			}
		}
		idx := b.turn(role, strings.Join(text, "\n"), false)
		for _, p := range tools {
			st := asObj(p["state"])
			c := nameCall(str(p, "tool"))
			c.ID, c.ArgsPreview = str(p, "callID"), b.args(st["input"])
			b.call(idx, c)
			b.result(c.ID, b.textOf(st["output"]), str(st, "status") == "error")
		}
	}
	return b.finish(), nil
}

// ReadClaudeAI reads conversations.json from the claude.ai export. conv picks
// a conversation by uuid; an export with one conversation needs none.
func ReadClaudeAI(data []byte, conv string) (*Session, error) {
	doc, err := decode(data)
	convs, ok := doc.([]any)
	if err != nil || !ok {
		return nil, errors.New("not a claude.ai export: expected the conversations.json array")
	}
	pick, err := pickConversation(convs, conv, "uuid", "name")
	if err != nil {
		return nil, err
	}
	b := newBuilder(SourceClaudeAI)
	b.s.Title = str(pick, "name")
	for _, mv := range list(pick, "chat_messages") {
		m := asObj(mv)
		role := "assistant"
		if str(m, "sender") == "human" {
			role = "user"
		}
		if ts := str(m, "created_at"); ts != "" {
			if b.s.StartedAt == "" {
				b.s.StartedAt = ts
			}
			b.s.EndedAt = ts
		}
		if parts := list(m, "content"); len(parts) > 0 {
			b.anthropic(role, parts, false)
		} else {
			b.turn(role, str(m, "text"), false)
		}
	}
	s := b.finish()
	if t := str(pick, "name"); t != "" {
		s.Title = t
	}
	return s, nil
}

// Conversation is one entry of an export, for choosing among several.
type Conversation struct{ ID, Title string }

// Conversations lists the conversations of a claude.ai or ChatGPT export.
func Conversations(source string, data []byte) ([]Conversation, error) {
	doc, err := decode(data)
	convs, ok := doc.([]any)
	if err != nil || !ok {
		return nil, errors.New("expected the conversations.json array of an export")
	}
	var out []Conversation
	for _, c := range convs {
		m := asObj(c)
		if source == SourceChatGPT {
			out = append(out, Conversation{firstString(str(m, "conversation_id"), str(m, "id")), str(m, "title")})
		} else {
			out = append(out, Conversation{str(m, "uuid"), str(m, "name")})
		}
	}
	return out, nil
}

func pickConversation(convs []any, id, idKey, titleKey string) (obj, error) {
	if len(convs) == 0 {
		return nil, errors.New("the export holds no conversations")
	}
	if id == "" {
		if len(convs) > 1 {
			return nil, fmt.Errorf("the export holds %d conversations; choose one with --conversation <id>", len(convs))
		}
		return asObj(convs[0]), nil
	}
	for _, c := range convs {
		m := asObj(c)
		if str(m, idKey) == id || str(m, "conversation_id") == id {
			return m, nil
		}
	}
	return nil, fmt.Errorf("the export has no conversation %q", id)
}

// ReadChatGPT reads conversations.json from the ChatGPT export, walking the
// message tree from the current node to the root.
func ReadChatGPT(data []byte, conv string) (*Session, error) {
	doc, err := decode(data)
	convs, ok := doc.([]any)
	if err != nil || !ok {
		return nil, errors.New("not a ChatGPT export: expected the conversations.json array")
	}
	pick, err := pickConversation(convs, conv, "id", "title")
	if err != nil {
		return nil, err
	}
	mapping := asObj(pick["mapping"])
	var chain []obj
	seen := map[string]bool{}
	for id := str(pick, "current_node"); id != "" && !seen[id]; {
		seen[id] = true
		node := asObj(mapping[id])
		if node == nil {
			break
		}
		if msg := asObj(node["message"]); msg != nil {
			chain = append(chain, msg)
		}
		id = str(node, "parent")
	}
	b := newBuilder(SourceChatGPT)
	b.s.Title = str(pick, "title")
	slices.Reverse(chain)
	for _, msg := range chain {
		author := asObj(msg["author"])
		content := asObj(msg["content"])
		var parts []string
		for _, p := range list(content, "parts") {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			} else {
				parts = append(parts, b.attachment())
			}
		}
		text := strings.Join(parts, "\n")
		switch role := str(author, "role"); role {
		case "user":
			b.turn("user", text, false)
		case "assistant":
			b.turn("assistant", text, false)
		case "tool":
			name := firstString(str(author, "name"), "tool")
			c := ToolCall{Name: name, Kind: KindMCPOther, ArgsPreview: "{}", ResultPreview: b.clip(text, MaxResultChars), Outcome: "ok"}
			if name == brand.Name {
				c.Kind = KindGateway
			}
			b.call(b.lastAssistant(), c)
		default:
			b.drop()
		}
	}
	return b.finish(), nil
}

const roleNames = `(?:user|human|you|assistant|claude|chatgpt|ai)`

// A marker is a heading ("## User"), bold ("**User**") or "User:" at the
// start of a line, so a sentence that merely begins with "You" is not one.
var roleMarker = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*(` + roleNames + `)\s*:?\s*()|\*\*(` + roleNames + `):?\*\*:?\s*(.*)|(` + roleNames + `):\s*(.*))$`)

// markerOf returns the role name and the rest of the line after a marker.
func markerOf(line string) (name, rest string, ok bool) {
	m := roleMarker.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	switch {
	case m[1] != "":
		return m[1], "", true
	case m[3] != "":
		return m[3], m[4], true
	}
	return m[5], m[6], true
}

// ReadMarkdown reads a pasted or exported chat with role markers at the start
// of a line: User:, Human:, ## Assistant, **Claude** and so on. Fenced blocks
// stay in their turn. Text without markers is one user turn.
func ReadMarkdown(text string) *Session {
	b := newBuilder(SourceMarkdown)
	var role string
	var cur []string
	flush := func() {
		if role != "" {
			b.turn(role, strings.Join(cur, "\n"), false)
		}
		cur = nil
	}
	fenced, found := false, false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if name, rest, ok := markerOf(line); ok && !fenced {
			flush()
			found = true
			role = "assistant"
			switch strings.ToLower(name) {
			case "user", "human", "you":
				role = "user"
			}
			if rest = strings.TrimSpace(rest); rest != "" {
				cur = append(cur, rest)
			}
			continue
		}
		cur = append(cur, line)
	}
	if !found {
		return ReadPlainText(text)
	}
	flush()
	return b.finish()
}

// ReadPlainText reads text with no roles as one user turn and says that
// extraction will be worse for it.
func ReadPlainText(text string) *Session {
	b := newBuilder(SourcePlainText)
	b.turn("user", text, false)
	b.s.Warnings = append(b.s.Warnings, "no roles detected; extraction quality is lower")
	return b.finish()
}
