package transcripts

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type obj = map[string]any

func str(m obj, key string) string {
	s, _ := m[key].(string)
	return s
}

func list(m obj, key string) []any {
	l, _ := m[key].([]any)
	return l
}

func asObj(v any) obj {
	m, _ := v.(obj)
	return m
}

// textOf flattens a tool result or message body: a string, or text parts.
func (b *builder) textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, p := range t {
			pm := asObj(p)
			switch str(pm, "type") {
			case "text", "input_text", "output_text", "":
				if s := str(pm, "text"); s != "" {
					parts = append(parts, s)
				}
			case "image":
				parts = append(parts, b.attachment())
			default:
				b.drop()
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// anthropic adds one message in the shape Claude Code and Cline both store: a
// role and content that is a string or typed parts.
func (b *builder) anthropic(role string, content any, sidechain bool) {
	parts, ok := content.([]any)
	if !ok {
		b.turn(role, b.textOf(content), sidechain)
		return
	}
	var text []string
	var uses []obj
	for _, p := range parts {
		pm := asObj(p)
		switch str(pm, "type") {
		case "text":
			text = append(text, str(pm, "text"))
		case "image", "document":
			text = append(text, b.attachment())
		case "tool_use":
			uses = append(uses, pm)
		case "tool_result":
			isErr, _ := pm["is_error"].(bool)
			b.result(str(pm, "tool_use_id"), b.textOf(pm["content"]), isErr)
		default:
			b.drop() // thinking and anything else the model or the agent added
		}
	}
	idx := b.turn(role, strings.Join(text, "\n"), sidechain)
	if role != "assistant" {
		return
	}
	for _, u := range uses {
		name, input := str(u, "name"), u["input"]
		var c ToolCall
		if im := asObj(input); name == "use_mcp_tool" && str(im, "server_name") != "" {
			c = classifyServerTool(str(im, "server_name"), str(im, "tool_name"))
			input = im["arguments"]
		} else {
			c = classify(name)
		}
		c.ID, c.ArgsPreview = str(u, "id"), b.args(input)
		b.call(idx, c)
	}
}

// eachJSONLine calls fn with every non-empty line of a JSONL stream decoded to
// an object. A line that is not JSON is skipped and counted as dropped.
func eachJSONLine(data []byte, b *builder, fn func(obj)) error {
	r := bufio.NewReaderSize(bytes.NewReader(data), 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var m obj
			if json.Unmarshal(t, &m) != nil {
				b.drop()
			} else {
				fn(m)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read transcript: %w", err)
		}
	}
}

// ReadClaudeCode reads a Claude Code session: JSONL lines of type user or
// assistant carrying an Anthropic message; summary lines are dropped.
func ReadClaudeCode(data []byte) (*Session, error) {
	b := newBuilder(SourceClaudeCode)
	err := eachJSONLine(data, b, func(m obj) {
		typ := str(m, "type")
		if typ != "user" && typ != "assistant" {
			if typ == "summary" {
				b.drop()
			}
			return
		}
		msg := asObj(m["message"])
		if msg == nil {
			return
		}
		if ts := str(m, "timestamp"); ts != "" {
			if b.s.StartedAt == "" {
				b.s.StartedAt = ts
			}
			b.s.EndedAt = ts
		}
		if cwd := str(m, "cwd"); cwd != "" && b.s.CwdHint == "" {
			b.s.CwdHint = hint(cwd)
		}
		if model := str(msg, "model"); model != "" {
			if b.s.Agent == nil {
				b.s.Agent = &AgentInfo{Name: "claude-code"}
			}
			b.s.Agent.Model = model
		}
		if v := str(m, "version"); v != "" {
			if b.s.Agent == nil {
				b.s.Agent = &AgentInfo{Name: "claude-code"}
			}
			b.s.Agent.Version = v
		}
		sidechain, _ := m["isSidechain"].(bool)
		b.anthropic(typ, msg["content"], sidechain)
	})
	if err != nil {
		return nil, err
	}
	return b.finish(), nil
}

// ReadCline reads a Cline task's api_conversation_history.json: an array of
// Anthropic messages.
func ReadCline(data []byte) (*Session, error) {
	var msgs []any
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("not a Cline conversation (expected a JSON array of messages): %w", err)
	}
	b := newBuilder(SourceCline)
	b.s.Agent = &AgentInfo{Name: "cline"}
	for _, m := range msgs {
		mm := asObj(m)
		role := str(mm, "role")
		if role != "user" && role != "assistant" {
			continue
		}
		b.anthropic(role, mm["content"], false)
	}
	return b.finish(), nil
}
