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

// mcpCatalogPath is the plain REST catalog-listing endpoint (gateway
// app/api/catalog/route.ts) — NOT the aggregate MCP endpoint: management.
// search_skills (skill_search.go) is hardcoded to type:'skill' and has no
// type filter, so relay-cli-completion plan D6 falls back to this route's
// documented ?type= query param instead (GET /api/catalog?search=&type=&tag=).
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

// mcpCatalogResource mirrors the fields of gateway/lib/db/schema/marketplace
// .ts's Resource this command prints — only the subset it needs, everything
// else is ignored by json.Unmarshal.
type mcpCatalogResource struct {
	ID    string  `json:"id"`
	Slug  string  `json:"slug"`
	Name  string  `json:"name"`
	Type  string  `json:"type"`
	OrgID *string `json:"orgId"`
}

// mcpCatalogVersion mirrors the subset of ResourceVersion this command
// prints: the semver, and manifestJson.source (mcp_server's registration
// source discriminator — builtin/url/repo; see gateway
// lib/db/schema/marketplace-resources.ts's ManifestJson doc comment).
type mcpCatalogVersion struct {
	Semver       string `json:"semver"`
	ManifestJSON struct {
		Source string `json:"source"`
	} `json:"manifestJson"`
}

// mcpCatalogEntry mirrors one element of gateway CatalogEntry
// (lib/marketplace/catalog.ts) as returned by GET /api/catalog.
type mcpCatalogEntry struct {
	Resource       mcpCatalogResource `json:"resource"`
	CurrentVersion *mcpCatalogVersion `json:"currentVersion"`
}

// mcpCatalogListResponse mirrors GET /api/catalog's top-level JSON body.
type mcpCatalogListResponse struct {
	Resources []mcpCatalogEntry `json:"resources"`
}

// McpListCmd returns the `relay mcp list` cobra command — a read-only
// listing of catalog MCP-server resources (id, slug, name, source,
// version). Per relay-cli-completion plan D6, `mcp install`/`mcp remove`
// are deliberately descoped (see README) — writing an MCP-server
// registration into a per-provider client config is a fourth artifact kind
// with its own IR and codecs, a separate design from this listing command.
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

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
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

	entries, err := listMcpServers(c)
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
func listMcpServers(doer mcpListDoer) ([]mcpCatalogEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpListTimeout)
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
		return nil, fmt.Errorf("mcp list failed (%d): %s", resp.StatusCode, truncateForError(body))
	}

	var parsed mcpCatalogListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode catalog response: %w", err)
	}
	return parsed.Resources, nil
}

// truncateForError trims a raw response body to a short diagnostic snippet.
func truncateForError(body []byte) string {
	s := string(body)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
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
