package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

// skillSearchPath is independent of the aggregate MCP feature switch.
const skillSearchPath = "/api/catalog/skills/search"

// maxSearchResponseBytes bounds the search response body read — a sane cap
// for a JSON search-results payload, mirroring catalog.readLimited's
// posture of never trusting an unbounded io.ReadAll on a network response
// (an unbounded read here would let a malicious/misbehaving gateway OOM the
// CLI process with an oversized body).
const maxSearchResponseBytes = 2 << 20 // 2 MiB

// readLimitedResponse reads at most limit+1 bytes from r, erroring if the
// stream exceeds limit — shared by every MCP/JSON-RPC response reader in
// this package (search, tool calls) so a malicious/misbehaving gateway can
// never hang or OOM the CLI via an unbounded body. Mirrors
// internal/client/refresh.go's readCapped for the CLI's own call paths.
func readLimitedResponse(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds size cap (%d bytes)", limit)
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
	GetContext(ctx context.Context, path string) (*http.Response, error)
}

// searchTimeout bounds a single search request end to end — shorter than
// catalog.downloadTimeout since a search payload is small and interactive
// (a hanging gateway must not hang `relay skill search` forever). A
// package-level var so tests can shrink it.
var searchTimeout = 30 * time.Second

// SkillSearchCmd searches the dedicated catalog route and prints skill metadata.
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

Requires an authenticated gateway supporting the dedicated catalog search API.
The aggregate MCP endpoint does not need to be enabled.

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

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
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

	results, err := searchSkills(cmd.Context(), c, query)
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

// searchSkills queries the dedicated catalog route with a bounded response.
func searchSkills(parent context.Context, doer skillSearchDoer, query string) ([]skillSearchEntry, error) {
	ctx, cancel := context.WithTimeout(parent, searchTimeout)
	defer cancel()
	resp, err := doer.GetContext(ctx, skillSearchPath+"?query="+url.QueryEscape(query))
	if err != nil {
		return nil, fmt.Errorf("catalog search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, fmt.Errorf("gateway does not support the dedicated skill search API; upgrade the gateway or use relay skill install <catalog-id>")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s", offlineGuidance)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search failed (%d); retry later or use relay skill install <catalog-id>", resp.StatusCode)
	}
	data, err := readLimitedResponse(resp.Body, maxSearchResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var result struct {
		OK   bool                `json:"ok"`
		Data *[]skillSearchEntry `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode search results: %w", err)
	}
	if !result.OK || result.Data == nil {
		return nil, fmt.Errorf("invalid catalog search response: expected ok:true and a data array")
	}
	return *result.Data, nil
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
