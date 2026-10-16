package mcpwrap

import "encoding/json"

// Message is one JSON-RPC message.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// IsRequest reports a message that expects an answer.
func (m Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsResponse reports an answer.
func (m Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

func parse(raw []byte) (Message, bool) {
	var m Message
	if json.Unmarshal(raw, &m) != nil {
		return Message{}, false
	}
	return m, true
}

// errorResponse answers a request with a JSON-RPC error.
func errorResponse(id json.RawMessage, code int, msg string) Message {
	e, _ := json.Marshal(map[string]any{"code": code, "message": msg})
	return Message{JSONRPC: "2.0", ID: id, Error: e}
}

// toolResult answers a tools/call with text the agent shows.
func toolResult(id json.RawMessage, isError bool, text string, structured any) Message {
	body := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
	if structured != nil {
		body["structuredContent"] = structured
	}
	raw, _ := json.Marshal(body)
	return Message{JSONRPC: "2.0", ID: id, Result: raw}
}
