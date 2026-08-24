// Package cli implements the cobra command definitions.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

// DiscoveryTool mirrors one tool entry returned by GET /api/integrations
// (gateway app/api/integrations/route.ts's DiscoveryTool). InputSchema is
// carried through for future use (e.g. flag validation) but not consumed yet.
type DiscoveryTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
}

// DiscoveryService mirrors one service entry returned by GET /api/integrations
// (gateway app/api/integrations/route.ts's DiscoveryService).
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

// fetchIntegrations calls the always-on GET /api/integrations discovery
// endpoint and returns the full service+tool catalog (with descriptions and
// input schemas) for the calling principal.
//
// This REPLACES the previous discovery path, which called `gateway.catalog`
// via the aggregate `POST /api/mcp` JSON-RPC endpoint. That endpoint 503s
// whenever AGGREGATE_MCP_ENABLED != 'true' (default OFF — see gateway
// app/api/mcp/route.ts), which silently broke `relay services`,
// `relay help-tools`, and dynamic per-service sub-command registration on
// any gateway that hadn't explicitly opted into the (intentional) aggregate
// kill-switch. /api/integrations is a dedicated, dispatch-free, always-on
// discovery endpoint (auth: verifyMcpAuth, same as every MCP surface) that
// was added specifically to serve this need — see gateway
// app/api/integrations/route.ts's doc comment.
//
// Returns (nil, nil) — not an error — when the caller is unauthenticated
// (401, i.e. c.Do's transparent refresh-and-retry ALSO failed), matching the
// previous fetchCatalog behavior: callers that want a hard auth failure
// (e.g. `relay services`) perform their own request instead of going
// through this helper so they can print the re-authenticate prompt and
// exit(1).
func fetchIntegrations(c *client.Client) (*IntegrationsResponse, error) {
	resp, err := c.Get("/api/integrations")
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

// callTool is the shared execution path for both `relay <service> <tool>
// [--arg key=value...]` (dynamic sub-commands, see buildToolCmd) and
// `relay call <service> <tool> [--arg key=value...]` (CallCmd, call.go).
//
// EXECUTION REPOINT: previously POSTed to the REST `/api/<service>/call`
// endpoint with a flat `{tool, arguments}` body. Now POSTs to the canonical
// per-service MCP JSON-RPC endpoint (`/api/<service>/mcp`, method
// "tools/call") with `{name, arguments}` params, so CLI execution goes
// through the SAME transport as any other MCP client — matching
// `relay help-tools` / discovery, which already used the per-service MCP
// surface for tools/list. Governance (rate-limit, allow/block policy,
// prompt-injection, schema validation, egress/SSRF, audit, redaction —
// lib/dispatch/run-tool.ts) is byte-for-byte identical either way; only the
// transport envelope changes (JSON-RPC result/error vs. the REST
// {ok, result|error} body `/call` used). `/call` itself is unchanged and
// still works (kept for scripts/back-compat) — this only repoints the CLI.
func callTool(c *client.Client, service, tool string, rawArgs []string) error {
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
	resp, err := c.Post(path, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		fmt.Fprintln(os.Stderr, "Token expired or revoked — run `relay login` to re-authenticate.")
		os.Exit(1)
	}

	// JSON-RPC envelope: {jsonrpc, id, result: {content:[{type,text}]}} on
	// success, {jsonrpc, id, error: {code, message}} on failure. The gateway
	// mostly responds 200 even for tool-level errors (JSON-RPC convention),
	// but returns real HTTP status codes for rate-limit (429, with a
	// Retry-After header) and a few policy rejections (400/403) — see
	// gateway app/api/[service]/mcp/route.ts's jsonRpcError helper.
	var rpcResp struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if jsonErr := json.Unmarshal(respBody, &rpcResp); jsonErr != nil {
		// Not a JSON-RPC body at all (unexpected) — surface raw output,
		// still respecting the HTTP status for the error/success split.
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("call failed (%d): %s", resp.StatusCode, string(respBody))
		}
		fmt.Println(string(respBody))
		return nil
	}

	if rpcResp.Error != nil {
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			return fmt.Errorf("call failed (%d, code %d): %s — retry after %ss", resp.StatusCode, rpcResp.Error.Code, rpcResp.Error.Message, retryAfter)
		}
		return fmt.Errorf("call failed (%d, code %d): %s", resp.StatusCode, rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("call failed (%d): %s", resp.StatusCode, string(respBody))
	}

	if rpcResp.Result == nil || len(rpcResp.Result.Content) == 0 {
		fmt.Println("{}")
		return nil
	}

	// toMcpResult (gateway lib/mcp.ts) JSON-encodes non-string outputs and
	// passes plain strings through untouched — mirror that here.
	text := rpcResp.Result.Content[0].Text
	var result interface{}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		fmt.Println(text)
		return nil
	}
	pretty, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(pretty))
	return nil
}

// buildToolCmd creates a leaf cobra command for a single tool.
// The command delegates to callTool.
//
// gatewayURL here is already the resolved URL BuildServiceCommands used to
// discover this tool in the first place (it only runs when !cli.Offline(),
// see cmd/relay/main.go's PersistentPreRunE) — but RunE re-resolves through
// resolveGatewayURLOrFailClosed anyway rather than closing over gatewayURL
// directly, so a dynamic tool command built during an earlier (online)
// invocation can never dial the gateway if --offline is passed to a later
// invocation of the same already-registered command. Cobra commands aren't
// rebuilt per-invocation within a single process, but resolveClient
// otherwise has no fail-closed check of its own — every gateway-dialing
// RunE in this package must resolve through resolveGatewayURLOrFailClosed
// so --offline is enforced uniformly, not just at discovery time.
func buildToolCmd(serviceName, toolName, toolDesc, gatewayURL string) *cobra.Command {
	var rawArgs []string
	cmd := &cobra.Command{
		Use:                toolName,
		Short:              toolDesc,
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, _args []string) error {
			gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
			if err != nil {
				return err
			}
			c := resolveClient(gURL)
			return callTool(c, serviceName, toolName, rawArgs)
		},
	}
	cmd.Flags().StringArrayVar(&rawArgs, "arg", nil, "Tool argument as key=value (repeatable, JSON values supported)")
	return cmd
}

// BuildServiceCommands fetches the gateway's integrations catalog and
// registers one cobra command per accessible service, with one sub-command
// per tool (tool metadata — name + description — comes embedded in the same
// discovery response, so no additional per-service round trip is needed).
//
// Call this from PersistentPreRunE on the root command so it only runs when
// a service command is about to be invoked. If the user is not logged in or
// the gateway is unreachable, this is a no-op (static help still works).
func BuildServiceCommands(root *cobra.Command, gatewayURL string) {
	c, err := client.Resolve(gatewayURL)
	if err != nil {
		return // not logged in — skip dynamic registration
	}

	info, err := fetchIntegrations(c)
	if err != nil || info == nil || len(info.Services) == 0 {
		return // unreachable/unauthenticated — degrade gracefully
	}

	for _, svc := range info.Services {
		if !svc.Accessible || len(svc.Tools) == 0 {
			continue
		}
		serviceID := svc.ID

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
}
