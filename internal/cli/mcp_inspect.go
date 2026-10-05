package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/spf13/cobra"
)

// Only explicitly permitted descriptor metadata is rendered. Endpoints,
// commands, credentials, and arbitrary manifest fields never enter this type.
type mcpInspection struct {
	Resource          mcpCatalogResource `json:"resource"`
	Version           string             `json:"version"`
	Source            string             `json:"source"`
	Transport         string             `json:"transport"`
	AuthType          string             `json:"authType"`
	AuthScope         string             `json:"authScope"`
	HasAuthReference  bool               `json:"hasAuthReference"`
	DeclaredToolCount int                `json:"declaredToolCount"`
}

func McpInspectCmd() *cobra.Command {
	var gateway string
	var jsonOut bool
	cmd := &cobra.Command{Use: "inspect <catalog-id>", Short: "Inspect permitted MCP server metadata", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		g, err := resolveGatewayURLOrFailClosed(gateway)
		if err != nil {
			return err
		}
		c, err := client.Resolve(g)
		if err != nil {
			return err
		}
		result, err := inspectMcpServer(cmd.Context(), c, args[0])
		if err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s (%s)\nVersion: %s\nSource: %s\nTransport: %s\nAuthentication: %s (%s)\nCredential reference configured: %t\nDeclared tools: %d\n", result.Resource.Name, result.Resource.ID, result.Version, result.Source, result.Transport, result.AuthType, result.AuthScope, result.HasAuthReference, result.DeclaredToolCount)
		return err
	}}
	cmd.Flags().StringVar(&gateway, "gateway-url", "", "Gateway URL override")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print permitted metadata as JSON")
	return cmd
}

func inspectMcpServer(parent context.Context, doer mcpListDoer, id string) (mcpInspection, error) {
	var out mcpInspection
	if id == "" || strings.ContainsAny(id, "/\\?#") || id == "." || id == ".." {
		return out, fmt.Errorf("invalid catalog resource id")
	}
	ctx, cancel := context.WithTimeout(parent, mcpListTimeout)
	defer cancel()
	resp, err := doer.GetContext(ctx, "/api/catalog/"+url.PathEscape(id))
	if err != nil {
		return out, fmt.Errorf("MCP inspection request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return out, fmt.Errorf("MCP resource not found or access denied")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return out, fmt.Errorf("authentication required; run relay login")
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("MCP inspection failed (HTTP %d)", resp.StatusCode)
	}
	body, err := readLimitedResponse(resp.Body, mcpListMaxResponseBytes)
	if err != nil {
		return out, fmt.Errorf("MCP inspection response unreadable or too large")
	}
	var parsed struct {
		Resource       mcpCatalogResource `json:"resource"`
		CurrentVersion *struct {
			Semver   string `json:"semver"`
			Manifest struct {
				Content string `json:"content"`
			} `json:"manifestJson"`
		} `json:"currentVersion"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Resource.Type != "mcp_server" || parsed.Resource.ID != id || parsed.CurrentVersion == nil {
		return out, fmt.Errorf("invalid MCP resource response")
	}
	var descriptor mcpDescriptor
	if json.Unmarshal([]byte(parsed.CurrentVersion.Manifest.Content), &descriptor) != nil {
		return out, fmt.Errorf("MCP descriptor unavailable or invalid; gateway upgrade may be required")
	}
	if err := validateMcpDescriptor(descriptor); err != nil {
		return out, err
	}
	out = mcpInspection{Resource: parsed.Resource, Version: parsed.CurrentVersion.Semver, Source: descriptor.Source, Transport: descriptor.Transport, AuthType: descriptor.AuthType, AuthScope: descriptor.AuthScope, HasAuthReference: descriptor.AuthRef != "", DeclaredToolCount: len(descriptor.DeclaredTools)}
	if out.Source == "" {
		out.Source = "builtin"
	}
	if out.AuthScope == "" {
		out.AuthScope = "org"
	}
	if out.AuthType == "" {
		out.AuthType = "none"
		if out.HasAuthReference {
			out.AuthType = "api_key"
		}
	}
	return out, nil
}
