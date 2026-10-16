package cli

import (
	"net/http"
	"strings"
	"testing"
)

func TestMcpLink_PrintsTheClaudeLinkAndTheChatGPTSteps(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/org/current/connect-links": reply(200, contractFixture(t, "connect-links.json")),
	})
	out, err := runCommand(t, MCPCmd(), "link", "claude-ai")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"modal=add-custom-connector&connectorName=Zuano&connectorUrl=https%3A%2F%2Fgateway.example%2Fapi%2Fmcp\n", "Connector URL: https://gateway.example/api/mcp", "1. Open Organization settings"} {
		if !strings.Contains(out, want) {
			t.Errorf("claude-ai lacks %q:\n%s", want, out)
		}
	}
	out, err = runCommand(t, MCPCmd(), "link", "claude-ai", "--directory")
	if err != nil || !strings.Contains(out, "%2Fapi%2Fdirectory%2Fmcp") {
		t.Errorf("directory: %q, %v", out, err)
	}
	out, err = runCommand(t, MCPCmd(), "link", "chatgpt")
	if err != nil || !strings.Contains(out, "Connector URL: https://gateway.example/api/chatgpt/mcp") || !strings.Contains(out, "no client secret") || strings.Contains(out, "Admin console") {
		t.Errorf("chatgpt: %q, %v", out, err)
	}
}

func TestMcpLink_RejectsAnUnknownTargetAndNeedsAnAdmin(t *testing.T) {
	approvalsGateway(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/org/org_2/connect-links": reply(403, `{"error":"forbidden","message":"admins only"}`),
	})
	if _, err := runCommand(t, MCPCmd(), "link", "gemini"); err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("unknown target err = %v", err)
	}
	if _, err := runCommand(t, MCPCmd(), "link", "chatgpt", "--org", "org_2"); err == nil || !strings.Contains(err.Error(), "admins only (HTTP 403)") {
		t.Errorf("403 err = %v", err)
	}
}
