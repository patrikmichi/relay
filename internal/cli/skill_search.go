package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

// aggregateMcpPath is the aggregate MCP JSON-RPC endpoint. management.* tools
// (get_skill, search_skills, publish_status, ...) are ONLY registered here —
// unlike built-in services, there is no per-service /api/management/mcp
// route (design spec §5.1). This endpoint is gated server-side by the
// AGGREGATE_MCP_ENABLED kill-switch (default off outside preview/opt-in
// environments) — see searchSkillsDegradationHint for the guidance shown
// when that surfaces as a 503.
const aggregateMcpPath = "/api/mcp"

// searchSkillsToolName is the gateway's discovery wrapper over listCatalog
// filtered to type:'skill' (lib/marketplace/mcp-search-skills-tool.ts).
const searchSkillsToolName = "management.search_skills"

// maxSearchResponseBytes bounds the search response body read — a sane cap
// for a JSON search-results payload, mirroring catalog.readLimited's
// posture of never trusting an unbounded io.ReadAll on a network response
// (an unbounded read here would let a malicious/misbehaving gateway OOM the
// CLI process with an oversized body).
const maxSearchResponseBytes = 2 << 20 // 2 MiB

// readLimitedSearchResponse reads at most maxSearchResponseBytes+1 bytes
// from r, erroring if the stream exceeds the cap.
func readLimitedSearchResponse(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSearchResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSearchResponseBytes {
		return nil, fmt.Errorf("search response exceeds size cap (%d bytes)", maxSearchResponseBytes)
	}
	return data, nil
}

// skillSearchEntry mirrors one entry of the management.search_skills tool
// result (SearchSkillsEntry in lib/marketplace/mcp-search-skills-tool.ts).
type skillSearchEntry struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	Description *string `json:"description"`
	Version     *string `json:"version"`
	TrustScore  *int    `json:"trustScore"`
}

// skillSearchDoer is the minimal HTTP interface SkillSearchCmd needs —
// satisfied by *client.Client.
type skillSearchDoer interface {
	PostContext(ctx context.Context, path, contentType string, body io.Reader) (*http.Response, error)
}

// searchTimeout bounds a single search request end to end — shorter than
// catalog.downloadTimeout since a search payload is small and interactive
// (m3 in the Go review: a hanging gateway must not hang `relay skill
// search` forever). A package-level var so tests can shrink it.
var searchTimeout = 30 * time.Second

// SkillSearchCmd returns the `relay skill search <query>` cobra command —
// calls the gateway's management.search_skills MCP tool (aggregate
// /api/mcp endpoint) and prints matching catalog skills (id, slug,
// version, trust score, description).
func SkillSearchCmd() *cobra.Command {
	var (
		gatewayURL string
		jsonOut    bool
	)

	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search the gateway catalog for installable skills",
		Long: `Search the gateway catalog for skills by keyword (name/summary), printing
each match's catalog id, slug, latest version, and trust score — feed the
id/slug directly into 'relay skill install <catalog-id>'.

Requires a reachable, authenticated gateway with the aggregate MCP endpoint
enabled server-side (AGGREGATE_MCP_ENABLED) — degrades with guidance if
either is unavailable.

Examples:
  relay skill search
  relay skill search "pr triage"
  relay skill search --json triage`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) > 0 {
				query = args[0]
			}
			return runSkillSearch(cmd, query, gatewayURL, jsonOut)
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print raw JSON results")

	return cmd
}

func runSkillSearch(cmd *cobra.Command, query, gatewayURL string, jsonOut bool) error {
	gURL, err := resolveGatewayURLOrFailClosed(gatewayURL)
	if err != nil {
		return err
	}

	c, err := client.Resolve(gURL)
	if err != nil {
		if errors.Is(err, client.ErrNotLoggedIn) {
			return fmt.Errorf("%s", offlineGuidance)
		}
		return err
	}

	results, err := searchSkills(c, query)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if jsonOut {
		raw, marshalErr := json.MarshalIndent(results, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("marshal results: %w", marshalErr)
		}
		fmt.Fprintln(out, string(raw))
		return nil
	}

	printSkillSearchResults(out, results)
	return nil
}

// searchSkills calls management.search_skills on the aggregate MCP endpoint
// and decodes its JSON-RPC result into a slice of skillSearchEntry.
func searchSkills(doer skillSearchDoer, query string) ([]skillSearchEntry, error) {
	arguments := map[string]interface{}{}
	if query != "" {
		arguments["query"] = query
	}

	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      searchSkillsToolName,
			"arguments": arguments,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	resp, err := doer.PostContext(ctx, aggregateMcpPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", aggregateMcpPath, err)
	}
	defer resp.Body.Close()

	respBody, err := readLimitedSearchResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("skill search is unavailable on this gateway (aggregate MCP endpoint disabled) — try `relay skill install <catalog-id>` directly if you already know the id/slug")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s", offlineGuidance)
	}

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
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("search failed (%d): %s", resp.StatusCode, string(respBody))
		}
		return nil, fmt.Errorf("unexpected search response: %s", string(respBody))
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("search failed (code %d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search failed (%d): %s", resp.StatusCode, string(respBody))
	}
	if rpcResp.Result == nil || len(rpcResp.Result.Content) == 0 {
		return nil, nil
	}

	var results []skillSearchEntry
	if err := json.Unmarshal([]byte(rpcResp.Result.Content[0].Text), &results); err != nil {
		return nil, fmt.Errorf("decode search results: %w", err)
	}
	return results, nil
}

// printSkillSearchResults prints a tab-aligned table of results, or a
// "no results" message when empty.
func printSkillSearchResults(out io.Writer, results []skillSearchEntry) {
	if len(results) == 0 {
		fmt.Fprintln(out, "no matching skills found")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSLUG\tVERSION\tTRUST\tDESCRIPTION")
	for _, r := range results {
		version := "-"
		if r.Version != nil {
			version = *r.Version
		}
		trust := "-"
		if r.TrustScore != nil {
			trust = fmt.Sprintf("%d", *r.TrustScore)
		}
		desc := ""
		if r.Description != nil {
			desc = *r.Description
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Slug, version, trust, desc)
	}
	_ = tw.Flush()
}
