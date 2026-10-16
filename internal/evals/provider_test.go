package evals

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func newTestProvider(t *testing.T, id string, h http.HandlerFunc) *HTTPProvider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p, err := NewProvider(id, env(map[string]string{
		"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": srv.URL,
		"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": srv.URL,
		"GEMINI_API_KEY": "k", "GEMINI_BASE_URL": srv.URL,
		"OPENAI_COMPATIBLE_BASE_URL": srv.URL,
	}))
	if err != nil {
		t.Fatal(err)
	}
	p.SetSleep(func(time.Duration) {})
	return p
}

func TestAnthropicRoundTrip(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	p := newTestProvider(t, ProviderAnthropic, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hi "},{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}],"usage":{"input_tokens":11,"output_tokens":7}}`)
	})
	temp := 0.0
	resp, err := p.Complete(context.Background(), ChatRequest{
		Model: "m", System: "sys", Temperature: &temp,
		Turns: []Turn{{Role: "user", Text: "q"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "tu_0", Name: "lookup"}}}, {Role: "tool", Text: "r1", ToolCallID: "tu_0"}, {Role: "tool", Text: "r2", ToolCallID: "tu_9"}},
		Tools: []ToolSpec{{Name: "lookup", Description: "d"}},
	})
	if err != nil || resp.Text != "hi " || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Args["q"] != "x" || resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if hdr.Get("x-api-key") != "k" || hdr.Get("anthropic-version") == "" || got["system"] != "sys" || got["max_tokens"].(float64) != 2048 {
		t.Errorf("request = %v %v", hdr, got)
	}
	msgs := got["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if len(msgs) != 3 || len(last) != 2 {
		t.Errorf("tool results were not merged into one user turn: %v", msgs)
	}
}

func TestOpenAIAndCompatibleRoundTrip(t *testing.T) {
	for _, id := range []string{ProviderOpenAI, ProviderOpenAICompatible} {
		var auth string
		var body map[string]any
		p := newTestProvider(t, id, func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
		})
		resp, err := p.Complete(context.Background(), ChatRequest{Model: "m", System: "s", MaxTokens: 50,
			Turns: []Turn{{Role: "user", Text: "q"}, {Role: "assistant", Text: "a", ToolCalls: []ToolCall{{ID: "c0", Name: "f", Args: map[string]any{"x": 1}}}}, {Role: "tool", Text: "r", ToolCallID: "c0"}},
			Tools: []ToolSpec{{Name: "f"}}})
		if err != nil || resp.Text != "ok" || resp.ToolCalls[0].Args["a"] != 1.0 || resp.Usage.InputTokens != 3 {
			t.Fatalf("%s resp = %+v, %v", id, resp, err)
		}
		if (id == ProviderOpenAI) != (auth == "Bearer k") || body["max_tokens"].(float64) != 50 {
			t.Errorf("%s auth %q body %v", id, auth, body)
		}
	}
}

func TestGoogleRoundTrip(t *testing.T) {
	var key, path string
	var body map[string]any
	p := newTestProvider(t, ProviderGoogle, func(w http.ResponseWriter, r *http.Request) {
		key, path = r.Header.Get("x-goog-api-key"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"yo"},{"functionCall":{"name":"f","args":{"b":2}}}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1}}`)
	})
	temp := 0.2
	resp, err := p.Complete(context.Background(), ChatRequest{Model: "gem", System: "s", MaxTokens: 9, Temperature: &temp,
		Turns: []Turn{{Role: "user", Text: "q"}, {Role: "assistant", Text: "t", ToolCalls: []ToolCall{{Name: "f"}}}, {Role: "tool", Text: "r", ToolName: "f"}},
		Tools: []ToolSpec{{Name: "f", Schema: map[string]any{"type": "object"}}, {Name: "g"}}})
	if err != nil || resp.Text != "yo" || resp.ToolCalls[0].Args["b"] != 2.0 || resp.Usage.OutputTokens != 1 {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if key != "k" || path != "/v1beta/models/gem:generateContent" || body["systemInstruction"] == nil || body["generationConfig"] == nil {
		t.Errorf("request %q %q %v", key, path, body)
	}
}

func TestProviderRetriesAndClassifiesFailures(t *testing.T) {
	var n int32
	p := newTestProvider(t, ProviderOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"done"}}]}`)
	})
	resp, err := p.Complete(context.Background(), ChatRequest{Model: "m", Turns: []Turn{{Role: "user", Text: "q"}}})
	if err != nil || resp.Text != "done" || n != 3 {
		t.Fatalf("after retries: %+v %v (%d calls)", resp, err, n)
	}

	for status, kind := range map[int]string{429: ErrRateLimited, 529: ErrRateLimited, 500: ErrTarget, 400: ErrModel, 401: ErrSetup, 404: ErrSetup} {
		p := newTestProvider(t, ProviderOpenAI, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
		})
		_, err := p.Complete(context.Background(), ChatRequest{Model: "m", Turns: []Turn{{Role: "user", Text: "q"}}})
		var ce *CaseError
		if !errors.As(err, &ce) || ce.Kind != kind {
			t.Errorf("status %d: %v, want kind %s", status, err, kind)
		}
		if status == 400 && !strings.Contains(err.Error(), "nope") {
			t.Errorf("the provider's message is lost: %v", err)
		}
	}

	bad := newTestProvider(t, ProviderOpenAI, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "not json") })
	if _, err := bad.Complete(context.Background(), ChatRequest{Model: "m"}); err == nil {
		t.Error("a non-JSON answer succeeded")
	}
}

func TestProviderContextAndTransportErrors(t *testing.T) {
	p := newTestProvider(t, ProviderAnthropic, func(w http.ResponseWriter, r *http.Request) {
		// The server only notices a closed connection once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Complete(ctx, ChatRequest{Model: "m"})
	var ce *CaseError
	if !errors.As(err, &ce) || ce.Kind != ErrTimeout {
		t.Errorf("deadline: %v", err)
	}
	dead, _ := NewProvider(ProviderOpenAI, env(map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "http://127.0.0.1:1"}))
	dead.SetSleep(func(time.Duration) {})
	_, err = dead.Complete(context.Background(), ChatRequest{Model: "m"})
	if !errors.As(err, &ce) || ce.Kind != ErrTarget || strings.Contains(err.Error(), "/v1/chat") {
		t.Errorf("transport error: %v", err)
	}
}

func TestNewProviderNeedsCredentials(t *testing.T) {
	for id, e := range map[string]map[string]string{
		ProviderAnthropic: {}, ProviderOpenAI: {}, ProviderGoogle: {}, ProviderOpenAICompatible: {"OPENAI_COMPATIBLE_API_KEY": "k"},
	} {
		if _, err := NewProvider(id, env(e)); !errors.Is(err, ErrNoKey) {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := NewProvider("mystery", env(nil)); err == nil {
		t.Error("an unknown provider")
	}
	if _, err := NewProvider(ProviderOpenAI, env(map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "http://[::1"})); err == nil {
		t.Error("an invalid base URL")
	}
	if p, err := NewProvider(ProviderOpenAICompatible, env(map[string]string{"OPENAI_COMPATIBLE_BASE_URL": "http://localhost:11434/"})); err != nil || p.ID() != ProviderOpenAICompatible {
		t.Errorf("a local server needs no key: %v", err)
	}
	f := EnvProviders(env(map[string]string{"OPENAI_API_KEY": "k"}))
	a, _ := f(ProviderOpenAI)
	b, _ := f(ProviderOpenAI)
	if a != b {
		t.Error("providers are not cached")
	}
	if _, err := f(ProviderGoogle); err == nil {
		t.Error("a missing key")
	}
	if OSEnv("ZUANO_EVALS_TEST_UNSET_VARIABLE") != "" {
		t.Error("OSEnv")
	}
}
