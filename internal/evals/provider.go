package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ToolSpec describes a tool offered to a model.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
}

// Turn is one entry of a model conversation, including the tool exchange.
type Turn struct {
	Role      string // user, assistant or tool
	Text      string
	ToolCalls []ToolCall
	// ToolCallID and ToolName identify the call a tool turn answers.
	ToolCallID string
	ToolName   string
}

// ChatRequest is one model call.
type ChatRequest struct {
	Model       string
	System      string
	Turns       []Turn
	Tools       []ToolSpec
	Temperature *float64
	MaxTokens   int
}

// ChatResponse is a model's answer.
type ChatResponse struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
}

// Provider is a model API.
type Provider interface {
	ID() string
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// Provider ids and the environment each reads.
const (
	ProviderAnthropic        = "anthropic"
	ProviderOpenAI           = "openai"
	ProviderGoogle           = "google"
	ProviderOpenAICompatible = "openai_compatible"
)

// ProviderEnv names the variables a provider's credentials come from.
var ProviderEnv = map[string]struct{ Key, BaseURL string }{
	ProviderAnthropic:        {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"},
	ProviderOpenAI:           {"OPENAI_API_KEY", "OPENAI_BASE_URL"},
	ProviderGoogle:           {"GEMINI_API_KEY", "GEMINI_BASE_URL"},
	ProviderOpenAICompatible: {"OPENAI_COMPATIBLE_API_KEY", "OPENAI_COMPATIBLE_BASE_URL"},
}

var defaultBase = map[string]string{
	ProviderAnthropic: "https://api.anthropic.com",
	ProviderOpenAI:    "https://api.openai.com",
	ProviderGoogle:    "https://generativelanguage.googleapis.com",
}

// ErrNoKey is returned when the environment lacks the provider's key.
var ErrNoKey = errors.New("no API key")

// HTTPProvider calls a provider's HTTP API directly.
type HTTPProvider struct {
	id      string
	base    string
	key     string
	client  *http.Client
	sleep   func(time.Duration)
	retries int
}

// NewProvider builds the provider with the given id from the environment.
func NewProvider(id string, getenv func(string) string) (*HTTPProvider, error) {
	env, ok := ProviderEnv[id]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (anthropic, openai, google, openai_compatible)", id)
	}
	key := getenv(env.Key)
	base := getenv(env.BaseURL)
	if base == "" {
		base = defaultBase[id]
	}
	if id == ProviderOpenAICompatible && base == "" {
		return nil, fmt.Errorf("%w: set %s to the server's base URL", ErrNoKey, env.BaseURL)
	}
	if key == "" && id != ProviderOpenAICompatible {
		return nil, fmt.Errorf("%w: set %s", ErrNoKey, env.Key)
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("%s: %w", env.BaseURL, err)
	}
	return &HTTPProvider{id: id, base: strings.TrimRight(base, "/"), key: key, client: &http.Client{Timeout: 120 * time.Second}, sleep: time.Sleep, retries: 3}, nil
}

// ID implements Provider.
func (p *HTTPProvider) ID() string { return p.id }

// SetSleep replaces the backoff wait, for tests.
func (p *HTTPProvider) SetSleep(f func(time.Duration)) { p.sleep = f }

// Complete implements Provider.
func (p *HTTPProvider) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	var path string
	var body any
	hdr := http.Header{"Content-Type": {"application/json"}}
	switch p.id {
	case ProviderAnthropic:
		path, body = "/v1/messages", anthropicBody(req)
		hdr.Set("x-api-key", p.key)
		hdr.Set("anthropic-version", "2023-06-01")
	case ProviderGoogle:
		path, body = "/v1beta/models/"+url.PathEscape(req.Model)+":generateContent", googleBody(req)
		hdr.Set("x-goog-api-key", p.key)
	default:
		path, body = "/v1/chat/completions", openAIBody(req)
		if p.key != "" {
			hdr.Set("Authorization", "Bearer "+p.key)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}
	resp, err := p.post(ctx, p.base+path, hdr, raw)
	if err != nil {
		return ChatResponse{}, err
	}
	switch p.id {
	case ProviderAnthropic:
		return parseAnthropic(resp)
	case ProviderGoogle:
		return parseGoogle(resp)
	}
	return parseOpenAI(resp)
}

func (p *HTTPProvider) post(ctx context.Context, target string, hdr http.Header, body []byte) ([]byte, error) {
	var last error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = hdr.Clone()
		resp, err := p.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, &CaseError{Kind: ErrTimeout, Message: ctx.Err().Error()}
			}
			last = &CaseError{Kind: ErrTarget, Message: "request failed: " + urlError(err)}
		} else {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			_ = resp.Body.Close()
			switch {
			case rerr != nil:
				last = &CaseError{Kind: ErrTarget, Message: "reading the response: " + rerr.Error()}
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return raw, nil
			case resp.StatusCode == 429 || resp.StatusCode == 529 || resp.StatusCode >= 500:
				last = &CaseError{Kind: ErrRateLimited, Message: fmt.Sprintf("%s answered %d", p.id, resp.StatusCode)}
				if resp.StatusCode >= 500 && resp.StatusCode != 529 {
					last = &CaseError{Kind: ErrTarget, Message: fmt.Sprintf("%s answered %d", p.id, resp.StatusCode)}
				}
				if after, _ := strconv.Atoi(resp.Header.Get("Retry-After")); after > 0 && after <= 30 && attempt < p.retries {
					p.sleep(time.Duration(after) * time.Second)
					continue
				}
			case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404:
				// A rejected key or an unknown model fails every case the same way and says nothing about the target.
				return nil, &CaseError{Kind: ErrSetup, Message: fmt.Sprintf("%s answered %d: %s", p.id, resp.StatusCode, apiMessage(raw))}
			default:
				return nil, &CaseError{Kind: ErrModel, Message: fmt.Sprintf("%s answered %d: %s", p.id, resp.StatusCode, apiMessage(raw))}
			}
		}
		if attempt >= p.retries {
			return nil, last
		}
		p.sleep(time.Duration(1<<attempt) * 500 * time.Millisecond)
	}
}

// urlError drops the request URL a transport error would otherwise repeat.
func urlError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func apiMessage(raw []byte) string {
	var doc struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &doc) == nil && len(doc.Error) > 0 {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(doc.Error, &inner) == nil && inner.Message != "" {
			return truncate(inner.Message, 300)
		}
		var s string
		if json.Unmarshal(doc.Error, &s) == nil {
			return truncate(s, 300)
		}
	}
	return truncate(strings.TrimSpace(string(raw)), 300)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func schemaOrEmpty(s map[string]any) map[string]any {
	if s == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return s
}

func anthropicBody(r ChatRequest) map[string]any {
	var msgs []map[string]any
	for _, t := range r.Turns {
		switch t.Role {
		case "assistant":
			var blocks []map[string]any
			if t.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t.Text})
			}
			for _, c := range t.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": nonNil(c.Args)})
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			block := map[string]any{"type": "tool_result", "tool_use_id": t.ToolCallID, "content": t.Text}
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "user" {
				if prev, ok := msgs[n-1]["content"].([]map[string]any); ok {
					msgs[n-1]["content"] = append(prev, block)
					continue
				}
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{block}})
		default:
			msgs = append(msgs, map[string]any{"role": "user", "content": t.Text})
		}
	}
	body := map[string]any{"model": r.Model, "max_tokens": maxTokens(r), "messages": msgs}
	if r.System != "" {
		body["system"] = r.System
	}
	if r.Temperature != nil {
		body["temperature"] = *r.Temperature
	}
	if len(r.Tools) > 0 {
		var tools []map[string]any
		for _, t := range r.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schemaOrEmpty(t.Schema)})
		}
		body["tools"] = tools
	}
	return body
}

func maxTokens(r ChatRequest) int {
	if r.MaxTokens > 0 {
		return r.MaxTokens
	}
	return 2048
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func parseAnthropic(raw []byte) (ChatResponse, error) {
	var doc struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ChatResponse{}, &CaseError{Kind: ErrTarget, Message: "the response is not JSON"}
	}
	out := ChatResponse{Usage: Usage{InputTokens: doc.Usage.In, OutputTokens: doc.Usage.Out}}
	for _, c := range doc.Content {
		switch c.Type {
		case "text":
			out.Text += c.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Args: c.Input})
		}
	}
	return out, nil
}

func openAIBody(r ChatRequest) map[string]any {
	var msgs []map[string]any
	if r.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": r.System})
	}
	for _, t := range r.Turns {
		switch t.Role {
		case "assistant":
			m := map[string]any{"role": "assistant", "content": t.Text}
			var calls []map[string]any
			for _, c := range t.ToolCalls {
				args, _ := json.Marshal(nonNil(c.Args))
				calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": string(args)}})
			}
			if len(calls) > 0 {
				m["tool_calls"] = calls
			}
			msgs = append(msgs, m)
		case "tool":
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": t.ToolCallID, "content": t.Text})
		default:
			msgs = append(msgs, map[string]any{"role": "user", "content": t.Text})
		}
	}
	body := map[string]any{"model": r.Model, "messages": msgs}
	if r.MaxTokens > 0 {
		body["max_tokens"] = r.MaxTokens
	}
	if r.Temperature != nil {
		body["temperature"] = *r.Temperature
	}
	if len(r.Tools) > 0 {
		var tools []map[string]any
		for _, t := range r.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": schemaOrEmpty(t.Schema)}})
		}
		body["tools"] = tools
	}
	return body
}

func parseOpenAI(raw []byte) (ChatResponse, error) {
	var doc struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Choices) == 0 {
		return ChatResponse{}, &CaseError{Kind: ErrTarget, Message: "the response holds no choice"}
	}
	m := doc.Choices[0].Message
	out := ChatResponse{Text: m.Content, Usage: Usage{InputTokens: doc.Usage.In, OutputTokens: doc.Usage.Out}}
	for _, c := range m.ToolCalls {
		call := ToolCall{ID: c.ID, Name: c.Function.Name}
		_ = json.Unmarshal([]byte(c.Function.Arguments), &call.Args)
		out.ToolCalls = append(out.ToolCalls, call)
	}
	return out, nil
}

func googleBody(r ChatRequest) map[string]any {
	var contents []map[string]any
	for _, t := range r.Turns {
		switch t.Role {
		case "assistant":
			var parts []map[string]any
			if t.Text != "" {
				parts = append(parts, map[string]any{"text": t.Text})
			}
			for _, c := range t.ToolCalls {
				parts = append(parts, map[string]any{"functionCall": map[string]any{"name": c.Name, "args": nonNil(c.Args)}})
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
		case "tool":
			contents = append(contents, map[string]any{"role": "user", "parts": []map[string]any{{"functionResponse": map[string]any{"name": t.ToolName, "response": map[string]any{"result": t.Text}}}}})
		default:
			contents = append(contents, map[string]any{"role": "user", "parts": []map[string]any{{"text": t.Text}}})
		}
	}
	body := map[string]any{"contents": contents}
	if r.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": r.System}}}
	}
	gen := map[string]any{}
	if r.Temperature != nil {
		gen["temperature"] = *r.Temperature
	}
	if r.MaxTokens > 0 {
		gen["maxOutputTokens"] = r.MaxTokens
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	if len(r.Tools) > 0 {
		var decls []map[string]any
		for _, t := range r.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Schema) > 0 {
				d["parameters"] = t.Schema
			}
			decls = append(decls, d)
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	return body
}

func parseGoogle(raw []byte) (ChatResponse, error) {
	var doc struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Usage struct {
			In  int `json:"promptTokenCount"`
			Out int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Candidates) == 0 {
		return ChatResponse{}, &CaseError{Kind: ErrTarget, Message: "the response holds no candidate"}
	}
	out := ChatResponse{Usage: Usage{InputTokens: doc.Usage.In, OutputTokens: doc.Usage.Out}}
	for i, p := range doc.Candidates[0].Content.Parts {
		out.Text += p.Text
		if p.FunctionCall != nil {
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: "call_" + strconv.Itoa(i), Name: p.FunctionCall.Name, Args: p.FunctionCall.Args})
		}
	}
	return out, nil
}

// OSEnv reads the process environment.
func OSEnv(k string) string { return os.Getenv(k) }
