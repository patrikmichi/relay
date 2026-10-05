package cli

import (
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

const mcpCatalogPath = "/api/catalog?type=mcp_server"

// mcpListTimeout bounds a single catalog-list request end to end, mirroring
// skill_search.go's searchTimeout — a hanging gateway must not hang `relay
// mcp list` forever.
var mcpListTimeout = 30 * time.Second

// mcpListMaxResponseBytes bounds the response body read, mirroring
// skill_search.go's maxSearchResponseBytes posture: never trust an
// unbounded io.ReadAll on a network response.
const mcpListMaxResponseBytes = 4 << 20 // 4 MiB

// mcpListDoer is the minimal HTTP interface McpListCmd needs — satisfied by
// *client.Client (the same GetContext shape internal/catalog.Doer uses).
type mcpListDoer interface {
	GetContext(ctx context.Context, path string) (*http.Response, error)
}

// mcpCatalogResource is the subset of a GET /api/catalog entry's
// `resource` object this command prints; everything else is ignored by
// json.Unmarshal.
type mcpCatalogResource struct {
	ID    string  `json:"id"`
	Slug  string  `json:"slug"`
	Name  string  `json:"name"`
	Type  string  `json:"type"`
	OrgID *string `json:"orgId"`
}

// mcpCatalogVersion is the subset of an entry's `currentVersion` object
// this command prints: the semver, and manifestJson.source (an
// mcp_server's registration source — builtin, url, or repo).
type mcpCatalogVersion struct {
	Semver       string `json:"semver"`
	ManifestJSON struct {
		Source string `json:"source"`
	} `json:"manifestJson"`
}

// mcpCatalogEntry is one element of GET /api/catalog's `resources` array.
type mcpCatalogEntry struct {
	Resource       mcpCatalogResource `json:"resource"`
	CurrentVersion *mcpCatalogVersion `json:"currentVersion"`
}

// mcpCatalogListResponse mirrors GET /api/catalog's top-level JSON body.
type mcpCatalogListResponse struct {
	Resources []mcpCatalogEntry `json:"resources"`
}

func McpListCmd() *cobra.Command {
	var (
		gatewayURL string
		jsonOut    bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List MCP servers available on the gateway catalog",
		Long: `List catalog resources of type mcp_server: id, slug, name, registration
source (builtin/url/repo), and current published version.

Read-only — this command never writes any provider config. 'relay mcp
install'/'relay mcp remove' are deliberately NOT implemented (see README):
writing MCP-server registrations into per-provider client config
(~/.claude.json, ~/.codex/config.toml, .cursor/mcp.json, ...) is a separate,
larger design.

Requires a reachable, authenticated gateway; fails closed offline.

Examples:
  relay mcp list
  relay mcp list --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMcpList(cmd, gatewayURL, jsonOut)
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, then the config file)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print raw JSON results")

	return cmd
}

func runMcpList(cmd *cobra.Command, gatewayURL string, jsonOut bool) error {
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

	entries, err := listMcpServers(cmd.Context(), c)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if jsonOut {
		raw, marshalErr := json.MarshalIndent(entries, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("marshal results: %w", marshalErr)
		}
		fmt.Fprintln(out, string(raw))
		return nil
	}

	printMcpListResults(out, entries)
	return nil
}

// listMcpServers calls GET /api/catalog?type=mcp_server and decodes the
// response into the resource entries this command prints.
func listMcpServers(parent context.Context, doer mcpListDoer) ([]mcpCatalogEntry, error) {
	ctx, cancel := context.WithTimeout(parent, mcpListTimeout)
	defer cancel()

	resp, err := doer.GetContext(ctx, mcpCatalogPath)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", mcpCatalogPath, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, mcpListMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > mcpListMaxResponseBytes {
		return nil, fmt.Errorf("catalog response exceeds size cap (%d bytes)", mcpListMaxResponseBytes)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s", offlineGuidance)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp list failed (%d)", resp.StatusCode)
	}

	var parsed mcpCatalogListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode catalog response: %w", err)
	}
	if parsed.Resources == nil {
		return nil, fmt.Errorf("invalid catalog response: resources array missing")
	}
	for _, entry := range parsed.Resources {
		if entry.Resource.Type != "mcp_server" {
			return nil, fmt.Errorf("invalid catalog response: unexpected resource type")
		}
	}
	return parsed.Resources, nil
}

// printMcpListResults prints a tab-aligned table of results, or a "no
// results" message when empty.
func printMcpListResults(out io.Writer, entries []mcpCatalogEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(out, "no MCP servers found in the catalog")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSLUG\tNAME\tSOURCE\tVERSION")
	for _, e := range entries {
		source := "-"
		version := "-"
		if e.CurrentVersion != nil {
			if e.CurrentVersion.ManifestJSON.Source != "" {
				source = e.CurrentVersion.ManifestJSON.Source
			}
			if e.CurrentVersion.Semver != "" {
				version = e.CurrentVersion.Semver
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Resource.ID, e.Resource.Slug, e.Resource.Name, source, version)
	}
	_ = tw.Flush()
}
