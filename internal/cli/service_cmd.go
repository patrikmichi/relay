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
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
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
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	AuthType    string          `json:"authType,omitempty"`
	Description string          `json:"description,omitempty"`
	Category    string          `json:"category,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	DocsURL     string          `json:"docsUrl,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	Accessible  bool            `json:"accessible"`
	ToolCount   int             `json:"toolCount"`
	Tools       []DiscoveryTool `json:"tools"`
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
	if err := client.DecodeJSON(resp.Body, &out); err != nil {
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
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type mcpToolResult struct {
	IsError           bool              `json:"isError"`
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
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
	if link, ok := resourceLinkText(raw); ok {
		return link
	}
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

// callTool is the shared execution path for both `zuano <service> <tool> [--arg key=value...]` (dynamic sub-commands, see buildToolCmd) and `zuano call <service> <tool> [--arg key=value...]` (CallCmd, call.go).
func callTool(c *client.Client, service, tool string, rawArgs []string, jsonOut bool) error {
	return callToolWithContext(context.Background(), c, service, tool, rawArgs, jsonOut, os.Stdout)
}

// buildToolArguments turns --arg key=value into string arguments and
// --arg-json key=<json> into typed ones. A value is never guessed at: 007
// stays "007" unless the person says it is a number.
func buildToolArguments(strs, jsons []string) (map[string]interface{}, error) {
	arguments := make(map[string]interface{}, len(strs)+len(jsons))
	for _, raw := range strs {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --arg format %q: expected key=value", raw)
		}
		arguments[k] = v
	}
	for _, raw := range jsons {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --arg-json format %q: expected key=<json>", raw)
		}
		if _, dup := arguments[k]; dup {
			return nil, fmt.Errorf("argument %q is given twice", k)
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			return nil, fmt.Errorf("--arg-json %s: not valid JSON (%v); quote a string as '\"text\"' or use --arg", k, err)
		}
		arguments[k] = parsed
	}
	return arguments, nil
}

// callToolWithContext runs the call. A call the gateway holds for approval is
// reported and, with a wait option, collected once it is decided.
func callToolWithContext(parent context.Context, c *client.Client, service, tool string, rawArgs []string, jsonOut bool, out io.Writer, opts ...callOpts) error {
	var o callOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	if len(o.files) > 0 {
		refs, err := uploadCallFiles(parent, c, o.files, rawArgs, o.jsonArgs)
		if err != nil {
			return err
		}
		rawArgs = append(slices.Clone(rawArgs), refs...)
	}
	held, err := sendToolCall(parent, c, service, tool, rawArgs, o, jsonOut, o.wait, out)
	if err != nil || held == nil {
		return err
	}
	switch {
	case held.job != nil:
		return handleStartedJob(parent, c, held.job, jsonOut, o, out)
	case held.elicit != nil:
		retry := func() (*heldCall, error) {
			return sendToolCall(parent, c, service, tool, rawArgs, o, jsonOut, false, out)
		}
		return handleURLElicitation(parent, c, held.elicit, retry, jsonOut, o, out)
	}
	return handlePending(parent, c, held.approval, jsonOut, o, out)
}

// heldCall is a call the gateway did not finish: held for an approval, asking
// a person to visit an approval page first, or started as work that runs on
// after the answer.
type heldCall struct {
	approval *pendingApproval
	job      *startedJob
	elicit   *urlElicitation
}

// sendToolCall posts the tools/call request. It returns the pending approval
// or started job when the gateway held the call or started it as background
// work instead of running it to the end. With jsonOut it prints the raw
// JSON-RPC envelope, except for a call that waiting will replace with the
// final result.
func sendToolCall(parent context.Context, c *client.Client, service, tool string, rawArgs []string, o callOpts, jsonOut, waiting bool, out io.Writer) (unfinished *heldCall, err error) {
	ctx, cancel, err := requestContext(parent, toolCallRequest)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { err = timeoutCause(ctx, err) }()
	arguments, err := buildToolArguments(rawArgs, o.jsonArgs)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	path := "/api/" + service + "/mcp"
	resp, err := c.MCPPostContext(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := readLimitedResponse(resp.Body, maxToolCallResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("token expired or revoked — run `zuano login` to re-authenticate")
	}

	env, envErr := decodeMCPEnvelope(respBody)
	held := pendingFromREST(resp.StatusCode, respBody)
	if held == nil && envErr == nil && env.Result != nil {
		held = pendingFromResult(env.Result)
	}
	var started *startedJob
	if held == nil {
		started = jobFromREST(resp.StatusCode, respBody)
		if started == nil && envErr == nil && env.Result != nil {
			started = jobFromResult(env.Result)
		}
	}
	var elicit *urlElicitation
	if held == nil && started == nil && envErr == nil && env.Error != nil && env.Error.Code == codeURLElicitationRequired {
		elicit = elicitationFromError(env.Error)
	}
	if jsonOut && ((held == nil && started == nil && elicit == nil) || !waiting) {
		fmt.Fprintln(out, string(respBody))
	}
	if held != nil {
		return &heldCall{approval: held}, nil
	}
	if started != nil {
		return &heldCall{job: started}, nil
	}
	if elicit != nil {
		return &heldCall{elicit: elicit}, nil
	}
	if refusal := policyRefusal(respBody, env); refusal != nil {
		return nil, refusal
	}

	if envErr != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("call failed (%d): %w", resp.StatusCode, envErr)
		}
		return nil, envErr
	}

	if env.Error != nil {
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			return nil, fmt.Errorf("call failed (%d, code %d): %s — retry after %ss%s", resp.StatusCode, env.Error.Code, env.Error.Message, retryAfter, client.RequestIDNote())
		}
		return nil, fmt.Errorf("call failed (%d, code %d): %s%s", resp.StatusCode, env.Error.Code, env.Error.Message, client.RequestIDNote())
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("call failed (%d): %s%s", resp.StatusCode, string(respBody), client.RequestIDNote())
	}

	if env.Result.IsError {
		return nil, fmt.Errorf("tool reported failure: %s", renderContentBlocks(env.Result.Content))
	}

	if !jsonOut {
		if len(env.Result.Content) == 0 {
			fmt.Fprintln(out, "{}")
		}
		for _, block := range env.Result.Content {
			fmt.Fprintln(out, renderContentBlock(block))
		}
	}
	if o.saveFiles != "" {
		// The result is already printed; a file that cannot be saved is
		// reported without hiding it.
		return nil, saveResultFiles(parent, c, env.Result, o.saveFiles, out, jsonOut)
	}
	return nil, nil
}

// buildToolCmd creates a leaf cobra command for a single tool.
func buildToolCmd(serviceName, toolName, toolDesc, gatewayURL string) *cobra.Command {
	var rawArgs, jsonArgs []string
	var jsonOut, noBrowser bool
	var wait, saveFiles string
	var files []string
	cmd := &cobra.Command{
		Use:                toolName,
		Short:              toolDesc,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, _args []string) error {
			opts, err := callOptsFor(cmd, wait)
			if err != nil {
				return err
			}
			opts.jsonArgs = jsonArgs
			opts.bindPresence(cmd, noBrowser)
			opts.files, opts.saveFiles = files, saveFiles
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}
			c, err := resolveClient(gURL)
			if err != nil {
				return err
			}
			return callToolWithContext(cmd.Context(), c, serviceName, toolName, rawArgs, jsonOut, cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringArrayVar(&rawArgs, "arg", nil, "Tool argument as key=value; the value is always a string (repeatable)")
	cmd.Flags().StringArrayVar(&jsonArgs, "arg-json", nil, "Tool argument as key=<json>, for numbers, booleans, objects and arrays (repeatable)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the full raw JSON-RPC response instead of human-rendered output")
	addWaitFlag(cmd, &wait)
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print an approval link instead of opening it in a browser")
	addFileFlags(cmd, &files, &saveFiles)
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
// hand-maintained name list, which silently drifts (cmd/zuano/main.go's
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
			return fmt.Errorf("discovered service %q collides with an existing zuano command name — refusing to register it; rename the service or contact your gateway operator", serviceID)
		}

		svcCmd := &cobra.Command{
			Use:   serviceID,
			Short: fmt.Sprintf("Commands for the %s service", svc.Name),
			Long: fmt.Sprintf(
				"Auto-generated sub-commands for the %s gateway service.\n\nUse `zuano %s <tool> --arg key=value` to call a tool.",
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
