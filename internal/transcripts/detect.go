package transcripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Sources lists the formats a person can name, with the flag value for each.
var Sources = map[string]string{
	"claude":    SourceClaudeCode,
	"codex":     SourceCodex,
	"gemini":    SourceGemini,
	"opencode":  SourceOpenCode,
	"cline":     SourceCline,
	"claude-ai": SourceClaudeAI,
	"chatgpt":   SourceChatGPT,
	"markdown":  SourceMarkdown,
	"text":      SourcePlainText,
}

// Detect names the format of a file from its name and first bytes.
func Detect(name string, data []byte) (string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "", errors.New("the file is empty")
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".markdown":
		return SourceMarkdown, nil
	case ".txt":
		return SourcePlainText, nil
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		if roleMarkerPresent(string(trimmed)) {
			return SourceMarkdown, nil
		}
		return SourcePlainText, nil
	}
	first := trimmed
	if i := bytes.IndexByte(trimmed, '\n'); i >= 0 {
		first = bytes.TrimSpace(trimmed[:i])
	}
	var doc any
	whole := json.Unmarshal(trimmed, &doc) == nil
	if ext == ".jsonl" || !whole {
		var line obj
		if json.Unmarshal(first, &line) != nil {
			return "", errors.New("the first line is not JSON")
		}
		return jsonlSource(line), nil
	}
	if m, ok := doc.(obj); ok && m["type"] != nil && m["messages"] == nil && m["history"] == nil {
		return jsonlSource(m), nil
	}
	switch t := doc.(type) {
	case []any:
		if len(t) == 0 {
			return "", errors.New("the file holds an empty array")
		}
		m := asObj(t[0])
		switch {
		case m["chat_messages"] != nil:
			return SourceClaudeAI, nil
		case m["mapping"] != nil:
			return SourceChatGPT, nil
		case m["parts"] != nil:
			return SourceGemini, nil
		case m["role"] != nil && m["content"] != nil:
			return SourceCline, nil
		}
	case obj:
		if ms := list(t, "messages"); len(ms) > 0 && asObj(ms[0])["info"] != nil {
			return SourceOpenCode, nil
		}
		if t["history"] != nil || t["messages"] != nil {
			return SourceGemini, nil
		}
	}
	return "", errors.New("the format is not recognised; name it with --agent")
}

func jsonlSource(line obj) string {
	switch str(line, "type") {
	case "session_meta", "response_item", "turn_context", "event_msg":
		return SourceCodex
	}
	return SourceClaudeCode
}

func roleMarkerPresent(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if _, _, ok := markerOf(line); ok {
			return true
		}
	}
	return false
}

// Read parses data as source (a Sources value). conversation chooses one
// conversation of an export.
func Read(source string, data []byte, conversation string) (*Session, error) {
	switch source {
	case SourceClaudeCode:
		return ReadClaudeCode(data)
	case SourceCodex:
		return ReadCodex(data)
	case SourceGemini:
		return ReadGemini(data)
	case SourceOpenCode:
		return ReadOpenCode(data)
	case SourceCline:
		return ReadCline(data)
	case SourceClaudeAI:
		return ReadClaudeAI(data, conversation)
	case SourceChatGPT:
		return ReadChatGPT(data, conversation)
	case SourceMarkdown:
		return ReadMarkdown(string(data)), nil
	case SourcePlainText:
		return ReadPlainText(string(data)), nil
	}
	return nil, fmt.Errorf("unknown format %q", source)
}
