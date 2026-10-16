package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/patrikmichi/zuano/internal/brand"
)

// MCPSessionHeader carries the session id a gateway assigns at initialize.
const MCPSessionHeader = "Mcp-Session-Id"

const mcpProtocolVersion = "2025-11-25"

// mcpInitializeBody is the initialize request. Declaring URL elicitation lets
// the gateway answer a call that needs approval with a link to open instead of
// a held-call result.
func mcpInitializeBody() []byte {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"elicitation": map[string]any{"url": map[string]any{}}},
			"clientInfo":      map[string]any{"name": brand.Name, "version": clientVersion},
		},
	})
	return body
}

var mcpInitializedBody = []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

// MCPPostContext posts one JSON-RPC request to an MCP endpoint. The first
// request to an endpoint is preceded by an initialize that declares URL
// elicitation; the Mcp-Session-Id the gateway returns is sent from then on. A
// gateway that does not issue sessions is used without one, and one that
// forgets a session (HTTP 404) gets a fresh initialize and one retry.
func (c *Client) MCPPostContext(ctx context.Context, path string, body []byte) (*http.Response, error) {
	sid := c.mcpSession(ctx, path)
	resp, err := c.mcpPost(ctx, path, sid, body)
	if err != nil || sid == "" || resp.StatusCode != http.StatusNotFound {
		return resp, err
	}
	_ = resp.Body.Close()
	c.mcpMu.Lock()
	delete(c.mcpSessions, path)
	c.mcpMu.Unlock()
	return c.mcpPost(ctx, path, c.mcpSession(ctx, path), body)
}

func (c *Client) mcpPost(ctx context.Context, path, sid string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.GatewayURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set(MCPSessionHeader, sid)
	}
	return c.Do(req)
}

// mcpSession returns the session id for path, initializing once per endpoint.
// An initialize that fails is not an error here: the call that follows
// reports whatever is wrong, such as a refusal, in its own terms.
func (c *Client) mcpSession(ctx context.Context, path string) string {
	c.mcpMu.Lock()
	defer c.mcpMu.Unlock()
	if sid, done := c.mcpSessions[path]; done {
		return sid
	}
	if c.mcpSessions == nil {
		c.mcpSessions = map[string]string{}
	}
	sid := ""
	resp, err := c.mcpPost(ctx, path, "", mcpInitializeBody())
	if err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxJSONBytes))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			sid = resp.Header.Get(MCPSessionHeader)
			if note, err := c.mcpPost(ctx, path, sid, mcpInitializedBody); err == nil {
				_ = note.Body.Close()
			}
		}
	}
	c.mcpSessions[path] = sid
	return sid
}
