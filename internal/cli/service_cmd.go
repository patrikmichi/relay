// Package cli implements the cobra command definitions.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

// DiscoveryTool mirrors one entry of a service's `tools` array in the
// GET /api/integrations response. InputSchema is
// carried through for future use (e.g. flag validation) but not consumed yet.
type DiscoveryTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
}

// DiscoveryService mirrors one service entry returned by GET /api/integrations.
type DiscoveryService struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Accessible bool            `json:"accessible"`
	ToolCount  int             `json:"toolCount"`
	Tools      []DiscoveryTool `json:"tools"`
}

// IntegrationsResponse is the JSON shape returned by GET /api/integrations.
type IntegrationsResponse struct {
	OK           bool               `json:"ok"`
	Error        string             `json:"error,omitempty"`
	ServiceCount int                `json:"serviceCount"`
	ToolCount    int                `json:"toolCount"`
	Services     []DiscoveryService `json:"services"`
}

// fetchIntegrations calls the always-on GET /api/integrations discovery endpoint and returns the full service+tool catalog (with descriptions and input schemas) for the calling principal.
func fetchIntegrations(c *client.Client, parents ...context.Context) (_ *IntegrationsResponse, err error) {
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	ctx, cancel, err := requestContext(parent, controlRequest)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { err = timeoutCause(ctx, err) }()
	resp, err := c.GetContext(ctx, "/api/integrations")
	if err != nil {
		return nil, fmt.Errorf("GET /api/integrations: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Not logged in / token revoked — degrade gracefully (mirrors the
		// old fetchCatalog behavior for optional discovery call sites).
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("integrations request failed (%d)", resp.StatusCode)
	}

	var out IntegrationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode integrations response: %w", err)
	}
	if !out.OK {
		return nil, fmt.Errorf("integrations request failed: %s", out.Error)
	}
	return &out, nil
}

// maxToolCallResponseBytes bounds a single tools/call response body — same
// posture as readLimitedResponse's other caller (skill search): an
// unbounded read here would let a malicious/misbehaving gateway hang or OOM
// the CLI process with an oversized body.
const maxToolCallResponseBytes = 4 << 20 // 4 MiB

// mcpRPCError mirrors the JSON-RPC 2.0 top-level error object.
type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolResult struct {
	IsError bool              `json:"isError"`
	Content []json.RawMessage `json:"content"`
}

// mcpEnvelope mirrors the full JSON-RPC 2.0 response envelope for any MCP
// call this CLI makes (per-service tools/call, aggregate tools/call for
// catalog search, ...). Result/Error are mutually exclusive per the JSON-RPC
// spec; decodeMCPEnvelope enforces that instead of trusting the caller.
type mcpEnvelope struct {
	Result *mcpToolResult `json:"result"`
	Error  *mcpRPCError   `json:"error"`
}

func decodeMCPEnvelope(body []byte) (*mcpEnvelope, error) {
	var env mcpEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("response is not valid JSON-RPC: %w", err)
	}
	if env.Result == nil && env.Error == nil {
		return nil, fmt.Errorf("response has neither a JSON-RPC result nor error field: %s", truncateForError(body))
	}
	if env.Result != nil && env.Error != nil {
		return nil, fmt.Errorf("response carries both a result and an error, which JSON-RPC forbids: %s", truncateForError(body))
	}
	return &env, nil
}

// truncateForError bounds how much of a response body an error message
// embeds, so a huge or garbage body can't blow up a single log/error line.
func truncateForError(body []byte) string {
	const max = 200
	if len(body) > max {
		return string(body[:max]) + "…"
	}
	return string(body)
}

// contentBlockText returns a content block's "text" field, and whether the
// block's "type" is "text" — the shape every current caller in this package
// expects its own tool's single-block replies to be.
func contentBlockText(raw json.RawMessage) (string, bool) {
	var typed struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil || typed.Type != "text" {
		return "", false
	}
	return typed.Text, true
}

func renderContentBlock(raw json.RawMessage) string {
	if text, ok := contentBlockText(raw); ok {
		var parsed interface{}
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			return text
		}
		pretty, err := json.MarshalIndent(parsed, "", "  ")
		if err != nil {
			return text
		}
		return string(pretty)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return string(raw)
	}
	return pretty.String()
}

// renderContentBlocks renders every block in order, newline-joined.
func renderContentBlocks(blocks []json.RawMessage) string {
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = renderContentBlock(b)
	}
	return strings.Join(parts, "\n")
}

// callTool is the shared execution path for both `relay <service> <tool> [--arg key=value...]` (dynamic sub-commands, see buildToolCmd) and `relay call <service> <tool> [--arg key=value...]` (CallCmd, call.go).
func callTool(c *client.Client, service, tool string, rawArgs []string, jsonOut bool) error {
	return callToolWithContext(context.Background(), c, service, tool, rawArgs, jsonOut, os.Stdout)
}
func callToolWithContext(parent context.Context, c *client.Client, service, tool string, rawArgs []string, jsonOut bool, out io.Writer) (err error) {
	ctx, cancel, err := requestContext(parent, toolCallRequest)
	if err != nil {
		return err
	}
	defer cancel()
	defer func() { err = timeoutCause(ctx, err) }()
	arguments := make(map[string]interface{})
	for _, raw := range rawArgs {
		k, v, ok := strings.Cut(raw, "=")
		if !ok {
			return fmt.Errorf("invalid --arg format %q: expected key=value", raw)
		}
		// Try to parse the value as JSON (handles numbers, booleans, objects).
		var parsed interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			arguments[k] = parsed
		} else {
			arguments[k] = v
		}
	}

	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      tool,
			"arguments": arguments,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	path := "/api/" + service + "/mcp"
	resp, err := c.PostContext(ctx, path, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := readLimitedResponse(resp.Body, maxToolCallResponseBytes)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("token expired or revoked — run `relay login` to re-authenticate")
	}

	if jsonOut {
		fmt.Fprintln(out, string(respBody))
	}

	env, envErr := decodeMCPEnvelope(respBody)
	if envErr != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("call failed (%d): %w", resp.StatusCode, envErr)
		}
		return envErr
	}

	if env.Error != nil {
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			return fmt.Errorf("call failed (%d, code %d): %s — retry after %ss", resp.StatusCode, env.Error.Code, env.Error.Message, retryAfter)
		}
		return fmt.Errorf("call failed (%d, code %d): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("call failed (%d): %s", resp.StatusCode, string(respBody))
	}

	if env.Result.IsError {
		return fmt.Errorf("tool reported failure: %s", renderContentBlocks(env.Result.Content))
	}

	if jsonOut {
		return nil // raw envelope already printed above
	}

	if len(env.Result.Content) == 0 {
		fmt.Fprintln(out, "{}")
		return nil
	}

	for _, block := range env.Result.Content {
		fmt.Fprintln(out, renderContentBlock(block))
	}
	return nil
}

// buildToolCmd creates a leaf cobra command for a single tool.
func buildToolCmd(serviceName, toolName, toolDesc, gatewayURL string) *cobra.Command {
	var rawArgs []string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:                toolName,
		Short:              toolDesc,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, _args []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}
			c, err := resolveClient(gURL)
			if err != nil {
				return err
			}
			return callToolWithContext(cmd.Context(), c, serviceName, toolName, rawArgs, jsonOut, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringArrayVar(&rawArgs, "arg", nil, "Tool argument as key=value (repeatable, JSON values supported)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the full raw JSON-RPC response instead of human-rendered output")
	return cmd
}

// reservedCommandNames are command names Cobra registers lazily (only once
// Find()/Execute() runs) rather than up front via AddCommand — so they
// can't be caught by walking root.Commands() at discovery time the way an
// explicitly-registered static command can. A discovered service must never
// be allowed to claim one of these either.
var reservedCommandNames = map[string]bool{"help": true, "completion": true}

// commandNameCollides reports whether name already names (or aliases) a
// top-level command on root, or is one of Cobra's own reserved names. The
// live command tree is the single source of truth here — not a
// hand-maintained name list, which silently drifts (cmd/relay/main.go's
// staticCommandNames omitted "recover" and "history" for exactly this
// reason until this fix).
func commandNameCollides(root *cobra.Command, name string) bool {
	if reservedCommandNames[name] {
		return true
	}
	for _, existing := range root.Commands() {
		if existing.Name() == name {
			return true
		}
		for _, alias := range existing.Aliases {
			if alias == name {
				return true
			}
		}
	}
	return false
}

func BuildServiceCommands(root *cobra.Command, gatewayURL string) error {
	c, err := client.Resolve(gatewayURL)
	if err != nil {
		return nil // not logged in — skip dynamic registration
	}

	info, err := fetchIntegrations(c, root.Context())
	if err != nil || info == nil || len(info.Services) == 0 {
		return nil // unreachable/unauthenticated — degrade gracefully
	}

	for _, svc := range info.Services {
		if !svc.Accessible || len(svc.Tools) == 0 {
			continue
		}
		serviceID := svc.ID
		if commandNameCollides(root, serviceID) {
			return fmt.Errorf("discovered service %q collides with an existing relay command name — refusing to register it; rename the service or contact your gateway operator", serviceID)
		}

		svcCmd := &cobra.Command{
			Use:   serviceID,
			Short: fmt.Sprintf("Commands for the %s service", svc.Name),
			Long: fmt.Sprintf(
				"Auto-generated sub-commands for the %s gateway service.\n\nUse `relay %s <tool> --arg key=value` to call a tool.",
				svc.Name, serviceID,
			),
		}

		for _, tool := range svc.Tools {
			svcCmd.AddCommand(buildToolCmd(serviceID, tool.Name, tool.Description, gatewayURL))
		}
		root.AddCommand(svcCmd)
	}
	return nil
}
