package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/client"
)

// artifactMaxBytes is the client-side size cap for a bundle (25 MiB).
// The server enforces its own cap; this is a client-side guard to fail fast.
// Declared as var (not const) so tests can temporarily lower it.
var artifactMaxBytes int64 = 25 << 20 // 25 MiB

// publishAPIPath is the endpoint on the gateway.
const publishAPIPath = "/api/marketplace/publish"

// ---- response types ----

// publishResponse mirrors the 201/200 body from POST /api/marketplace/publish.
type publishResponse struct {
	ResourceID string `json:"resourceId"`
	VersionID  string `json:"versionId"`
	Semver     string `json:"semver"`
	State      string `json:"state"`
	Channel    string `json:"channel,omitempty"`
}

// publishErrorDetail is one field-level error entry from a 422 response.
type publishErrorDetail struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// publishErrorBody is the top-level error envelope from the gateway.
type publishErrorBody struct {
	Error struct {
		Code    string               `json:"code"`
		Message string               `json:"message"`
		Details []publishErrorDetail `json:"details,omitempty"`
	} `json:"error"`
}

// ---- frontmatter parsing ----

// frontmatter holds the parsed YAML-like frontmatter from a markdown file.
// We parse a minimal subset: type, name, slug.
type frontmatter struct {
	Type string
	Name string
	Slug string
}

// parseFrontmatter reads the YAML frontmatter from a markdown file.
// It expects the file to start with "---\n" and end the block with "\n---".
// Only the fields we care about (type, name, slug) are extracted.
// This is a simple line-by-line scanner — it does not require a YAML library.
func parseFrontmatter(content []byte) frontmatter {
	text := string(content)
	if !strings.HasPrefix(text, "---") {
		return frontmatter{}
	}
	// Find the closing "---".
	rest := text[3:]
	// Skip optional trailing newline after opening "---".
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	}
	end := strings.Index(rest, "\n---")
	var block string
	if end >= 0 {
		block = rest[:end]
	} else {
		block = rest
	}

	var fm frontmatter
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if before, after, ok := strings.Cut(line, ":"); ok {
			key := strings.TrimSpace(before)
			val := strings.TrimSpace(after)
			// Strip inline comments and quotes.
			if idx := strings.Index(val, " #"); idx >= 0 {
				val = strings.TrimSpace(val[:idx])
			}
			val = strings.Trim(val, `"'`)
			switch strings.ToLower(key) {
			case "type":
				fm.Type = val
			case "name":
				fm.Name = val
			case "slug":
				fm.Slug = val
			}
		}
	}
	return fm
}

// ---- publishDoer interface — allows tests to swap in an httptest.Server ----

// publishDoer is the minimal HTTP interface PublishCmd uses.
type publishDoer interface {
	// Post sends a POST request to the given path with the given content-type and body.
	Post(path, contentType string, body io.Reader) (*http.Response, error)
}

// publishStatusDoer is the minimal HTTP interface used to poll publish status
// (GET /api/marketplace/publish/<versionId>/status). Kept as a SEPARATE
// interface from publishDoer (rather than adding Get to it) so existing
// publishDoer test fakes that only implement Post keep compiling unchanged.
type publishStatusDoer interface {
	Get(path string) (*http.Response, error)
}

// ---- PublishCmd + aliases ----

// PublishCmd returns the `relay publish <path>` cobra command.
func PublishCmd() *cobra.Command {
	return publishCmdWithDoer(nil)
}

// publishCmdWithDoer is the internal constructor; doer==nil means resolve at runtime.
func publishCmdWithDoer(doer publishDoer) *cobra.Command {
	var (
		flagType       string
		flagName       string
		flagSlug       string
		flagChannel    string
		flagDryRun     bool
		flagWatch      bool
		flagGatewayURL string
	)

	cmd := &cobra.Command{
		Use:   "publish <path>",
		Short: "Publish a skill, agent, or MCP server to the marketplace",
		Long: `Publish a resource to the relay marketplace.

<path> can be a single manifest file (SKILL.md / agent.md) or a directory.
The resource type is detected from the manifest frontmatter; use --type to override.

When publishing a directory, all files are bundled into a tar.gz.
The bundle is validated client-side before upload: symlinks that escape the root,
absolute paths, and ".." path components are refused.

Flags:
  --type <skill|agent|mcp_server|prompt|plugin>  Resource type (overrides frontmatter)
  --name <name>                                  Override the resource display name
  --slug <slug>                                  Override the resource slug
  --channel <stable|beta>                        Publish channel (default: stable)
  --dry-run                                      Validate + print what WOULD be sent; do not call the API
  --watch                                        After publishing, poll status until it reaches a
                                                  terminal state; exits non-zero on changes_requested`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mcpOpts := mcpPublishOptionsFromContext(cmd.Context())
			publishPath := ""
			if len(args) > 0 {
				publishPath = args[0]
			} else if mcpOpts.descriptorPath != "" {
				publishPath = mcpOpts.descriptorPath
			}
			if publishPath == "" {
				return fmt.Errorf("publish path is required; for MCP servers use <path> or --descriptor")
			}
			d := doer
			resolvedURL := flagGatewayURL
			if d == nil {
				var err error
				d, resolvedURL, err = resolvePublishDoerWithURL(flagGatewayURL)
				if err != nil {
					return err
				}
			}
			return runPublish(d, publishPath, publishOpts{
				resourceType:      flagType,
				name:              flagName,
				slug:              flagSlug,
				channel:           flagChannel,
				dryRun:            flagDryRun,
				watch:             flagWatch,
				gatewayURL:        resolvedURL,
				mcpDescriptorPath: mcpOpts.descriptorPath,
				mcpTransport:      mcpOpts.transport,
				mcpEndpoint:       mcpOpts.endpoint,
				mcpCommand:        mcpOpts.command,
				mcpAuthRef:        mcpOpts.authRef,
				mcpSource:         mcpOpts.source,
				mcpAuthType:       mcpOpts.authType,
				mcpAuthScope:      mcpOpts.authScope,
				mcpTools:          mcpOpts.tools,
			})
		},
	}

	cmd.Flags().StringVar(&flagType, "type", "", "Resource type: skill, agent, mcp_server, prompt, plugin")
	cmd.Flags().StringVar(&flagName, "name", "", "Override resource display name")
	cmd.Flags().StringVar(&flagSlug, "slug", "", "Override resource slug")
	cmd.Flags().StringVar(&flagChannel, "channel", "stable", "Publish channel: stable or beta")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Validate and print the payload without sending it")
	cmd.Flags().BoolVar(&flagWatch, "watch", false, "Poll publish status until terminal; exit non-zero on changes_requested")
	cmd.Flags().StringVar(&flagGatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")

	cmd.AddCommand(publishStatusCmd())

	return cmd
}

// skillPublishCmd returns `relay skill publish <path>` — thin alias.
func skillPublishCmd() *cobra.Command {
	cmd := publishCmdWithDoer(nil)
	cmd.Use = "publish <path>"
	cmd.Short = "Publish a skill to the marketplace"
	return cmd
}

// agentPublishCmd returns `relay agent publish <path>` — thin alias.
func agentPublishCmd() *cobra.Command {
	cmd := publishCmdWithDoer(nil)
	cmd.Use = "publish <path>"
	cmd.Short = "Publish an agent to the marketplace"
	return cmd
}

// mcpPublishCmd returns `relay mcp publish <path>` — thin alias.
func mcpPublishCmd() *cobra.Command {
	cmd := publishCmdWithDoer(nil)
	cmd.Use = "publish [path]"
	cmd.Short = "Publish an MCP server to the marketplace"
	cmd.Args = cobra.MaximumNArgs(1)
	cmd.Flags().String("descriptor", "", "Path to MCP server descriptor JSON")
	cmd.Flags().String("transport", "", "MCP transport: http, sse, or stdio-command")
	cmd.Flags().String("endpoint", "", "HTTP/SSE endpoint URL")
	cmd.Flags().String("command", "", "stdio command")
	cmd.Flags().String("auth-ref", "", "Credential reference name")
	cmd.Flags().String("source", "", "MCP source: url, repo, or builtin")
	cmd.Flags().String("auth-type", "", "Upstream auth: none, api_key, or oauth")
	cmd.Flags().String("auth-scope", "", "Credential scope: org or user")
	cmd.Flags().StringSlice("tool", nil, "Declared tool name (repeatable)")
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if err := cmd.Flags().Set("type", "mcp_server"); err != nil {
			return err
		}
		descriptor, _ := cmd.Flags().GetString("descriptor")
		transport, _ := cmd.Flags().GetString("transport")
		endpoint, _ := cmd.Flags().GetString("endpoint")
		command, _ := cmd.Flags().GetString("command")
		authRef, _ := cmd.Flags().GetString("auth-ref")
		source, _ := cmd.Flags().GetString("source")
		authType, _ := cmd.Flags().GetString("auth-type")
		authScope, _ := cmd.Flags().GetString("auth-scope")
		tools, _ := cmd.Flags().GetStringSlice("tool")
		cmd.SetContext(withMcpPublishOptions(cmd.Context(), publishMcpOptions{
			descriptorPath: descriptor,
			transport:      transport,
			endpoint:       endpoint,
			command:        command,
			authRef:        authRef,
			source:         source,
			authType:       authType,
			authScope:      authScope,
			tools:          tools,
		}))
		return nil
	}
	return cmd
}

// SkillCmd returns the `relay skill` parent command with `publish`,
// `migrate`, and the Wave 2 agentport manager sub-commands (install, list,
// diff, scan, score, uninstall, rollback), plus the Phase-2 gateway-catalog
// commands (search, and install's catalog-id source — see SkillInstallCmd).
func SkillCmd() *cobra.Command {
	skill := &cobra.Command{
		Use:   "skill",
		Short: "Skill commands",
	}
	skill.AddCommand(skillPublishCmd())
	skill.AddCommand(SkillMigrateCmd())
	skill.AddCommand(SkillInstallCmd())
	skill.AddCommand(SkillSearchCmd())
	skill.AddCommand(SkillListCmd())
	skill.AddCommand(SkillDiffCmd())
	skill.AddCommand(SkillScanCmd())
	skill.AddCommand(SkillScoreCmd())
	skill.AddCommand(SkillUninstallCmd())
	skill.AddCommand(SkillRollbackCmd())
	return skill
}

// AgentCmd returns the `relay agent` parent command with `publish`, and the
// offline agent-manager sub-commands (migrate) — mirroring SkillCmd's
// shape for the Agent IR. Supported agent providers: claude and opencode
// (both flat frontmatter+body markdown files). Codex has no single
// agent-file primitive (a TOML profile plus a decoupled prompt file is not
// equivalent to a markdown agent) and Cursor has no subagent primitive at
// all — neither is a supported agent migration target.
func AgentCmd() *cobra.Command {
	agent := &cobra.Command{
		Use:   "agent",
		Short: "Agent commands",
		Long: `Agent commands.

Supported agent providers: claude, opencode. Codex (TOML profile != md
agent) and Cursor (no subagent primitive) are not supported as agent
migration targets.`,
	}
	agent.AddCommand(agentPublishCmd())
	agent.AddCommand(AgentMigrateCmd())
	agent.AddCommand(AgentListCmd())
	agent.AddCommand(AgentDiffCmd())
	agent.AddCommand(AgentScanCmd())
	agent.AddCommand(AgentUninstallCmd())
	agent.AddCommand(AgentRollbackCmd())
	return agent
}

// MCPCmd returns the `relay mcp` parent command with a `publish` sub-command.
func MCPCmd() *cobra.Command {
	mcp := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server commands",
	}
	mcp.AddCommand(mcpPublishCmd())
	return mcp
}

// ---- auth resolution (mirrors sync.go / resolveSyncDoer) ----

// resolvePublishDoer resolves the gateway URL + OAuth token and returns a publishDoer.
// Used by tests that only need the doer.
func resolvePublishDoer(overrideURL string) (publishDoer, error) {
	d, _, err := resolvePublishDoerWithURL(overrideURL)
	return d, err
}

// resolvePublishDoerWithURL resolves an authenticated client and returns both
// the publishDoer and the resolved gateway URL (for catalog link
// generation). Auth resolution (GATEWAY_API_KEY / RELAY_EMAIL / persisted
// login email → keychain, with transparent OAuth refresh) is centralized in
// client.Resolve — *client.Client already satisfies both publishDoer and
// publishStatusDoer (matching Post/Get signatures), so no adapter type is
// needed here anymore.
func resolvePublishDoerWithURL(overrideURL string) (publishDoer, string, error) {
	gURL, err := resolveGatewayURLOrFailClosed(overrideURL)
	if err != nil {
		return nil, "", err
	}

	c, err := client.Resolve(gURL)
	if err != nil {
		return nil, "", err
	}
	return c, c.GatewayURL, nil
}

// resolvePublishStatusDoer resolves the gateway URL + OAuth token and returns
// a publishStatusDoer for `relay publish status`. Shares auth resolution with
// resolvePublishDoerWithURL — *client.Client satisfies both interfaces.
func resolvePublishStatusDoer(overrideURL string) (publishStatusDoer, error) {
	d, _, err := resolvePublishDoerWithURL(overrideURL)
	if err != nil {
		return nil, err
	}
	statusDoer, ok := d.(publishStatusDoer)
	if !ok {
		return nil, fmt.Errorf("internal error: resolved publish doer does not support status polling")
	}
	return statusDoer, nil
}

// ---- publish options ----

type publishOpts struct {
	resourceType string
	name         string
	slug         string
	channel      string
	dryRun       bool
	// watch: after a successful publish, poll status until terminal (see
	// watchPublishStatus). Exits non-zero on changes_requested.
	watch             bool
	mcpDescriptorPath string
	mcpTransport      string
	mcpEndpoint       string
	mcpCommand        string
	mcpAuthRef        string
	mcpSource         string
	mcpAuthType       string
	mcpAuthScope      string
	mcpTools          []string
	// gatewayURL is the resolved gateway base URL, used for catalog link generation.
	// Set by the command from the resolved doer; empty falls back to GATEWAY_URL env / default.
	gatewayURL string
}

type publishMcpOptions struct {
	descriptorPath string
	transport      string
	endpoint       string
	command        string
	authRef        string
	source         string
	authType       string
	authScope      string
	tools          []string
}

type mcpPublishContextKey struct{}

func withMcpPublishOptions(ctx context.Context, opts publishMcpOptions) context.Context {
	return context.WithValue(ctx, mcpPublishContextKey{}, opts)
}

func mcpPublishOptionsFromContext(ctx context.Context) publishMcpOptions {
	opts, _ := ctx.Value(mcpPublishContextKey{}).(publishMcpOptions)
	return opts
}

type mcpDescriptor struct {
	Name          string   `json:"name,omitempty"`
	Slug          string   `json:"slug,omitempty"`
	Version       string   `json:"version,omitempty"`
	Description   string   `json:"description,omitempty"`
	Transport     string   `json:"transport"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Command       string   `json:"command,omitempty"`
	AuthRef       string   `json:"authRef,omitempty"`
	Source        string   `json:"source,omitempty"`
	AuthType      string   `json:"authType,omitempty"`
	AuthScope     string   `json:"authScope,omitempty"`
	DeclaredTools []string `json:"declaredTools,omitempty"`
}

// ---- core publish logic ----

// runPublish implements the publish workflow.
func runPublish(d publishDoer, path string, opts publishOpts) error {
	// Validate channel.
	ch := opts.channel
	if ch == "" {
		ch = "stable"
	}
	if ch != "stable" && ch != "beta" {
		return fmt.Errorf("invalid --channel %q: must be stable or beta", ch)
	}

	// Resolve path.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", absPath, err)
	}

	var (
		manifestContent []byte
		bundleBytes     []byte
		bundleFiles     []string // for dry-run display
		isDir           bool
	)

	if fi.IsDir() {
		isDir = true
		// Collect + validate the directory bundle.
		manifestContent, bundleBytes, bundleFiles, err = buildBundle(absPath)
		if err != nil {
			return err
		}
	} else {
		// Single file — read as manifest.
		manifestContent, err = os.ReadFile(absPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", absPath, err)
		}
		bundleFiles = []string{filepath.Base(absPath)}
	}

	isMcpPublish := opts.mcpDescriptorPath != "" ||
		opts.mcpTransport != "" ||
		opts.mcpEndpoint != "" ||
		opts.mcpCommand != "" ||
		opts.mcpAuthRef != "" ||
		len(opts.mcpTools) > 0 ||
		opts.resourceType == "mcp_server"

	var descriptor *mcpDescriptor
	if isMcpPublish {
		resolved, err := resolveMcpDescriptor(opts)
		if err != nil {
			return err
		}
		descriptor = &resolved
		manifestContent, err = buildMcpManifestContent(resolved, opts.name, opts.slug)
		if err != nil {
			return err
		}
	}

	// Detect resource type from frontmatter, overridable by --type.
	fm := parseFrontmatter(manifestContent)
	resourceType := opts.resourceType
	if resourceType == "" && descriptor != nil {
		resourceType = "mcp_server"
	}
	if resourceType == "" {
		resourceType = fm.Type
	}
	if resourceType == "" {
		return fmt.Errorf("could not detect resource type from frontmatter — use --type to specify it")
	}

	// Apply flag overrides.
	name := opts.name
	if name == "" && descriptor != nil {
		name = descriptor.Name
	}
	if name == "" {
		name = fm.Name
	}
	slug := opts.slug
	if slug == "" && descriptor != nil {
		slug = descriptor.Slug
	}
	if slug == "" {
		slug = fm.Slug
	}

	// --dry-run: print what WOULD be sent, exit 0, no API call.
	if opts.dryRun {
		fmt.Println("[dry-run] would publish to:", publishAPIPath)
		fmt.Printf("[dry-run] type:    %s\n", resourceType)
		fmt.Printf("[dry-run] name:    %s\n", name)
		fmt.Printf("[dry-run] slug:    %s\n", slug)
		fmt.Printf("[dry-run] channel: %s\n", ch)
		if isDir {
			fmt.Printf("[dry-run] bundle files (%d):\n", len(bundleFiles))
			for _, f := range bundleFiles {
				fmt.Printf("[dry-run]   %s\n", f)
			}
			fmt.Printf("[dry-run] bundle size: %d bytes\n", len(bundleBytes))
		} else {
			fmt.Printf("[dry-run] manifest file: %s (%d bytes)\n", bundleFiles[0], len(manifestContent))
		}
		if resourceType == "mcp_server" {
			descriptorJSON, _ := json.MarshalIndent(descriptor, "", "  ")
			fmt.Printf("[dry-run] MCP descriptor:\n%s\n", descriptorJSON)
		}
		fmt.Println("[dry-run] no API call made")
		return nil
	}

	// Build multipart body.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// Write manifest part.
	mh := textproto.MIMEHeader{}
	mh.Set("Content-Disposition", `form-data; name="manifest"; filename="manifest.md"`)
	mh.Set("Content-Type", "text/markdown")
	manifestPart, err := mw.CreatePart(mh)
	if err != nil {
		return fmt.Errorf("create manifest part: %w", err)
	}
	if _, err := manifestPart.Write(manifestContent); err != nil {
		return fmt.Errorf("write manifest part: %w", err)
	}

	// Write channel field.
	if err := mw.WriteField("channel", ch); err != nil {
		return fmt.Errorf("write channel field: %w", err)
	}

	// Write type field (overrides frontmatter server-side if provided).
	if opts.resourceType != "" || resourceType == "mcp_server" {
		if err := mw.WriteField("type", resourceType); err != nil {
			return fmt.Errorf("write type field: %w", err)
		}
	}

	if resourceType == "mcp_server" {
		descriptorJSON, err := json.Marshal(descriptor)
		if err != nil {
			return fmt.Errorf("encode MCP descriptor: %w", err)
		}
		if err := mw.WriteField("mcpServerDescriptor", string(descriptorJSON)); err != nil {
			return fmt.Errorf("write MCP descriptor field: %w", err)
		}
	}

	// Write name override.
	if opts.name != "" {
		if err := mw.WriteField("name", opts.name); err != nil {
			return fmt.Errorf("write name field: %w", err)
		}
	}

	// Write slug override.
	if opts.slug != "" {
		if err := mw.WriteField("slug", opts.slug); err != nil {
			return fmt.Errorf("write slug field: %w", err)
		}
	}

	// Write bundle part (directory case).
	if isDir && len(bundleBytes) > 0 {
		bh := textproto.MIMEHeader{}
		bh.Set("Content-Disposition", `form-data; name="bundle"; filename="bundle.tar.gz"`)
		bh.Set("Content-Type", "application/gzip")
		bundlePart, err := mw.CreatePart(bh)
		if err != nil {
			return fmt.Errorf("create bundle part: %w", err)
		}
		if _, err := bundlePart.Write(bundleBytes); err != nil {
			return fmt.Errorf("write bundle part: %w", err)
		}
	}

	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	// POST to the gateway.
	resp, err := d.Post(publishAPIPath, mw.FormDataContentType(), &buf)
	if err != nil {
		return fmt.Errorf("POST %s: %w", publishAPIPath, err)
	}
	defer resp.Body.Close()

	// Read capped response body.
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if err := handlePublishResponse(resp.StatusCode, resp.Header, rawBody, opts.gatewayURL); err != nil {
		return err
	}

	// --watch: poll status until terminal. Only applies after a successful
	// publish (201 Created or 200 OK idempotent re-publish) — handlePublishResponse
	// above already returned an error for every other status code.
	if opts.watch && (resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK) {
		var pub publishResponse
		if jsonErr := json.Unmarshal(rawBody, &pub); jsonErr == nil && pub.VersionID != "" {
			statusDoer, ok := d.(publishStatusDoer)
			if !ok {
				return fmt.Errorf("--watch requires a client that supports status polling (internal error)")
			}
			fmt.Println()
			return watchPublishStatus(statusDoer, pub.VersionID)
		}
	}

	return nil
}

func resolveMcpDescriptor(opts publishOpts) (mcpDescriptor, error) {
	var descriptor mcpDescriptor
	if opts.mcpDescriptorPath != "" {
		raw, err := os.ReadFile(opts.mcpDescriptorPath)
		if err != nil {
			return descriptor, fmt.Errorf("read MCP descriptor %s: %w", opts.mcpDescriptorPath, err)
		}
		if err := json.Unmarshal(raw, &descriptor); err != nil {
			return descriptor, fmt.Errorf("parse MCP descriptor JSON: %w", err)
		}
	}

	if opts.mcpTransport != "" {
		descriptor.Transport = opts.mcpTransport
	}
	if opts.mcpEndpoint != "" {
		descriptor.Endpoint = opts.mcpEndpoint
	}
	if opts.mcpCommand != "" {
		descriptor.Command = opts.mcpCommand
	}
	if opts.mcpAuthRef != "" {
		descriptor.AuthRef = opts.mcpAuthRef
	}
	if opts.mcpSource != "" {
		descriptor.Source = opts.mcpSource
	}
	if opts.mcpAuthType != "" {
		descriptor.AuthType = opts.mcpAuthType
	}
	if opts.mcpAuthScope != "" {
		descriptor.AuthScope = opts.mcpAuthScope
	}
	if len(opts.mcpTools) > 0 {
		descriptor.DeclaredTools = opts.mcpTools
	}

	switch descriptor.Transport {
	case "http", "sse":
		if descriptor.Endpoint == "" {
			return descriptor, fmt.Errorf("MCP descriptor transport %q requires --endpoint or endpoint in descriptor JSON", descriptor.Transport)
		}
		if descriptor.Command != "" {
			return descriptor, fmt.Errorf("MCP descriptor transport %q must not include command", descriptor.Transport)
		}
	case "stdio-command":
		if descriptor.Command == "" {
			return descriptor, fmt.Errorf("MCP descriptor transport %q requires --command or command in descriptor JSON", descriptor.Transport)
		}
		if descriptor.Endpoint != "" {
			return descriptor, fmt.Errorf("MCP descriptor transport %q must not include endpoint", descriptor.Transport)
		}
	default:
		return descriptor, fmt.Errorf("MCP descriptor transport is required and must be http, sse, or stdio-command")
	}

	return descriptor, nil
}

func buildMcpManifestContent(descriptor mcpDescriptor, name, slug string) ([]byte, error) {
	if name == "" {
		name = descriptor.Name
	}
	if name == "" {
		return nil, fmt.Errorf("MCP server name is required; set --name or name in descriptor JSON")
	}
	if slug == "" {
		slug = descriptor.Slug
	}
	version := descriptor.Version
	if version == "" {
		version = "1.0.0"
	}
	description := descriptor.Description
	if description == "" {
		description = "Org-owned MCP server."
	}

	manifest := map[string]interface{}{
		"type":        "mcp_server",
		"name":        name,
		"version":     version,
		"description": description,
		"transport":   descriptor.Transport,
	}
	if slug != "" {
		manifest["slug"] = slug
	}
	if descriptor.Endpoint != "" {
		manifest["endpoint"] = descriptor.Endpoint
	}
	if descriptor.Command != "" {
		manifest["command"] = descriptor.Command
	}
	if descriptor.AuthRef != "" {
		manifest["authRef"] = descriptor.AuthRef
	}
	if descriptor.Source != "" {
		manifest["source"] = descriptor.Source
	}
	if descriptor.AuthType != "" {
		manifest["authType"] = descriptor.AuthType
	}
	if descriptor.AuthScope != "" {
		manifest["authScope"] = descriptor.AuthScope
	}
	if len(descriptor.DeclaredTools) > 0 {
		manifest["tools"] = descriptor.DeclaredTools
	}

	return json.Marshal(manifest)
}

// handlePublishResponse interprets the HTTP status + body and prints output or returns an error.
// hdr is the response header (used for Retry-After on 429).
// gatewayURL is the resolved gateway base URL for the catalog link; empty falls back to env/default.
func handlePublishResponse(statusCode int, hdr http.Header, rawBody []byte, gatewayURL string) error {
	switch statusCode {
	case http.StatusCreated, http.StatusOK:
		var pub publishResponse
		if err := json.Unmarshal(rawBody, &pub); err != nil {
			return fmt.Errorf("decode publish response: %w", err)
		}
		fmt.Printf("Published successfully\n")
		fmt.Printf("  Resource ID: %s\n", pub.ResourceID)
		fmt.Printf("  Version ID:  %s\n", pub.VersionID)
		fmt.Printf("  Semver:      %s\n", pub.Semver)
		ch := pub.Channel
		if ch == "" {
			ch = "stable"
		}
		fmt.Printf("  Channel:     %s\n", ch)
		fmt.Printf("  State:       %s\n", pub.State)
		fmt.Printf("  Catalog URL: %s\n", catalogURL(pub.ResourceID, gatewayURL))
		if pub.State == "changes_requested" {
			fmt.Println()
			fmt.Println("The version needs changes before it can be published.")
			fmt.Println("Review the gate report at the catalog URL above, address the issues,")
			fmt.Println("then re-run `relay publish <path>` with a bumped version number.")
		}
		return nil

	case http.StatusUnauthorized:
		return fmt.Errorf("not authenticated — run `relay login` first")

	case http.StatusForbidden:
		return fmt.Errorf("not authorized to publish — you must be an author or admin for this organization")

	case http.StatusConflict:
		// 409: same (org, slug, semver) with different content.
		msg := extractErrorMessage(rawBody)
		if msg == "" {
			msg = "a version with this slug and semver already exists with different content"
		}
		return fmt.Errorf("version already exists with different content — bump the version in your manifest and retry (%s)", msg)

	case http.StatusUnprocessableEntity:
		// 422: field-level validation errors.
		return formatValidationError(rawBody)

	case http.StatusTooManyRequests:
		// 429: rate limited. Surface the Retry-After header if present.
		retryAfter := hdr.Get("Retry-After")
		var detail string
		if retryAfter != "" {
			detail = fmt.Sprintf(" — retry after %s seconds", retryAfter)
		} else if msg := extractErrorMessage(rawBody); msg != "" {
			detail = " — " + msg
		}
		return fmt.Errorf("rate limited%s; wait before retrying", detail)

	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("bundle too large — reduce the size of your bundle and retry")

	default:
		body := strings.TrimSpace(string(rawBody))
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		return fmt.Errorf("publish failed (%d): %s", statusCode, body)
	}
}

// extractErrorMessage returns the error.message string from a JSON error body, or "".
func extractErrorMessage(rawBody []byte) string {
	var eb publishErrorBody
	if err := json.Unmarshal(rawBody, &eb); err != nil {
		return ""
	}
	return eb.Error.Message
}

// formatValidationError returns a human-readable error from a 422 body.
func formatValidationError(rawBody []byte) error {
	var eb publishErrorBody
	if err := json.Unmarshal(rawBody, &eb); err != nil {
		return fmt.Errorf("validation error (422): %s", strings.TrimSpace(string(rawBody)))
	}
	if len(eb.Error.Details) == 0 {
		return fmt.Errorf("validation error: %s", eb.Error.Message)
	}
	var sb strings.Builder
	sb.WriteString("validation error — fix the following fields and retry:\n")
	for _, d := range eb.Error.Details {
		sb.WriteString(fmt.Sprintf("  %s: %s\n", d.Field, d.Message))
	}
	return errors.New(strings.TrimRight(sb.String(), "\n"))
}

// catalogURL returns a URL to view the resource in the catalog. gatewayURL
// is the resolved gateway base URL from the successful publish call that
// preceded this; empty falls back through resolveGatewayURLOrFailClosed.
// That fallback path should be unreachable in practice (a publish can't
// have succeeded without a resolved gateway URL) but if it ever is empty —
// e.g. this helper called standalone — offlineGuidance is returned instead
// of a malformed "/catalog/<id>" URL.
func catalogURL(resourceID, gatewayURL string) string {
	gURL := gatewayURL
	if gURL == "" {
		var err error
		gURL, err = resolveGatewayURLOrFailClosed("")
		if err != nil {
			return offlineGuidance
		}
	}
	return strings.TrimRight(gURL, "/") + "/catalog/" + resourceID
}

// ---- publish status (relay publish status <versionId> / relay publish --watch) ----

// publishStatusPath is the REST polling endpoint on the gateway — the
// counterpart to the management.publish_status MCP tool (gateway
// lib/marketplace/publish-status.ts backs both; see that file's doc comment
// for why the CLI uses this REST path instead of the aggregate MCP endpoint).
func publishStatusPath(versionID string) string {
	return publishAPIPath + "/" + versionID + "/status"
}

// terminalPublishStates mirrors gateway lib/marketplace/publish-status.ts's
// TERMINAL_PUBLISH_STATES — states at which --watch / status --watch stop polling.
var terminalPublishStates = map[string]bool{
	"published":         true,
	"changes_requested": true,
	"suspended":         true,
	"archived":          true,
}

// publishStatusGate mirrors gateway PublishStatusGate.
type publishStatusGate struct {
	Gate                  string `json:"gate"`
	Status                string `json:"status"`
	Summary               string `json:"summary,omitempty"`
	RequiresAdminApproval bool   `json:"requiresAdminApproval"`
}

// publishStatusAiScan mirrors gateway PublishStatusAiScan.
type publishStatusAiScan struct {
	Status     string `json:"status"`
	TrustScore *int   `json:"trustScore"`
	Findings   int    `json:"findings"`
}

// publishStatusResult mirrors gateway PublishStatusResult (the shape
// returned by both management.publish_status and the REST polling route).
type publishStatusResult struct {
	VersionID             string              `json:"versionId"`
	State                 string              `json:"state"`
	Gates                 []publishStatusGate `json:"gates"`
	AiScan                publishStatusAiScan `json:"aiScan"`
	RequiresAdminApproval bool                `json:"requiresAdminApproval"`
	Approvable            bool                `json:"approvable"`
}

// publishStatusPollInterval is the delay between polls in watchPublishStatus.
// A package var (not a const) so tests can shrink it.
var publishStatusPollInterval = 3 * time.Second

// publishStatusMaxPolls caps the polling loop so a server that never reaches
// a terminal state (bug, or a version stuck mid-pipeline) cannot hang the CLI
// forever. At the default 3s interval this is a 30-minute ceiling.
var publishStatusMaxPolls = 600

// sleepFn is overridable by tests so watchPublishStatus doesn't actually sleep.
var sleepFn = time.Sleep

// fetchPublishStatus performs one GET against the status polling endpoint and
// decodes the response. Non-200 responses are turned into an error using the
// same error-body shape as publish (publishErrorBody).
func fetchPublishStatus(d publishStatusDoer, versionID string) (*publishStatusResult, error) {
	resp, err := d.Get(publishStatusPath(versionID))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", publishStatusPath(versionID), err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := extractErrorMessage(rawBody)
		if msg == "" {
			msg = strings.TrimSpace(string(rawBody))
		}
		return nil, fmt.Errorf("publish status failed (%d): %s", resp.StatusCode, msg)
	}

	var status publishStatusResult
	if err := json.Unmarshal(rawBody, &status); err != nil {
		return nil, fmt.Errorf("decode publish status response: %w", err)
	}
	return &status, nil
}

// printPublishStatus renders a publishStatusResult as human-readable text.
func printPublishStatus(status *publishStatusResult) {
	fmt.Printf("Version:  %s\n", status.VersionID)
	fmt.Printf("State:    %s\n", status.State)
	if len(status.Gates) > 0 {
		fmt.Println("Gates:")
		for _, g := range status.Gates {
			approval := ""
			if g.RequiresAdminApproval {
				approval = " (requires admin approval)"
			}
			fmt.Printf("  %-14s %s%s\n", g.Gate, g.Status, approval)
		}
	}
	trustScore := "n/a"
	if status.AiScan.TrustScore != nil {
		trustScore = fmt.Sprintf("%d/100", *status.AiScan.TrustScore)
	}
	fmt.Printf("AI scan:  %s (trust score: %s, findings: %d)\n", status.AiScan.Status, trustScore, status.AiScan.Findings)
	fmt.Printf("Approvable now: %t\n", status.Approvable)
}

// watchPublishStatus polls fetchPublishStatus until the version reaches a
// terminal state (see terminalPublishStates), printing status on each change.
// Returns a non-nil error when the final state is changes_requested — so
// `relay publish --watch` / `relay publish status --watch` exit non-zero and
// CI can gate on it.
func watchPublishStatus(d publishStatusDoer, versionID string) error {
	var last string
	for i := 0; i < publishStatusMaxPolls; i++ {
		status, err := fetchPublishStatus(d, versionID)
		if err != nil {
			return err
		}
		if status.State != last {
			printPublishStatus(status)
			fmt.Println()
			last = status.State
		}
		if terminalPublishStates[status.State] {
			if status.State == "changes_requested" {
				return fmt.Errorf("publish status: %s — the version needs changes before it can be published", status.State)
			}
			return nil
		}
		sleepFn(publishStatusPollInterval)
	}
	return fmt.Errorf("timed out waiting for version %s to reach a terminal state after %d polls", versionID, publishStatusMaxPolls)
}

// publishStatusCmd returns the `relay publish status <versionId>` cobra command.
func publishStatusCmd() *cobra.Command {
	var (
		flagWatch      bool
		flagGatewayURL string
	)

	cmd := &cobra.Command{
		Use:   "status <versionId>",
		Short: "Check the publish-pipeline status of a resource version",
		Long: `Read the publish-pipeline status for a resource version: state, gate results,
AI red-team scan status/trust score, and whether an admin approval is required
or currently possible.

With --watch, polls until the version reaches a terminal state (published,
changes_requested, suspended, archived) and exits non-zero if it lands on
changes_requested — useful for gating CI on a publish.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			versionID := args[0]
			d, err := resolvePublishStatusDoer(flagGatewayURL)
			if err != nil {
				return err
			}
			if flagWatch {
				return watchPublishStatus(d, versionID)
			}
			status, err := fetchPublishStatus(d, versionID)
			if err != nil {
				return err
			}
			printPublishStatus(status)
			if status.State == "changes_requested" {
				return fmt.Errorf("publish status: %s — the version needs changes before it can be published", status.State)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&flagWatch, "watch", false, "Poll status until terminal; exit non-zero on changes_requested")
	cmd.Flags().StringVar(&flagGatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")

	return cmd
}

// ---- bundle builder (path-safe tar.gz) ----

// buildBundle reads a directory, validates every file entry for path safety,
// enforces the artifactMaxBytes cap, and returns:
//   - the content of the manifest file (the .md file at the root, or the first .md found)
//   - the tar.gz bytes of the entire directory
//   - the list of relative paths included (for dry-run display)
func buildBundle(root string) (manifestContent []byte, bundleBytes []byte, files []string, err error) {
	// Collect files.
	var entries []bundleEntry
	totalSize := int64(0)

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		// H1: skip non-regular, non-symlink entries (FIFOs, devices, sockets) to prevent
		// os.ReadFile blocking forever on a FIFO or reading device garbage.
		// Symlinks are allowed to fall through so checkSymlink can validate them below.
		t := d.Type()
		if t&os.ModeSymlink == 0 && !t.IsRegular() {
			return nil // skip FIFO / device / socket / irregular
		}

		// Compute relative path.
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("rel path for %s: %w", path, relErr)
		}

		// Path safety checks.
		if err := checkBundlePath(rel); err != nil {
			return err
		}

		// Symlink check: reject symlinks that escape the bundle root.
		if err := checkSymlink(path, root); err != nil {
			return err
		}

		// Use os.Stat (follows symlinks) for an accurate size of the content we will read.
		info, statErr := os.Stat(path)
		if statErr != nil {
			return fmt.Errorf("stat %s: %w", path, statErr)
		}

		totalSize += info.Size()
		if totalSize > artifactMaxBytes {
			return fmt.Errorf("bundle exceeds size cap (%d bytes); reduce the directory size and retry", artifactMaxBytes)
		}

		entries = append(entries, bundleEntry{abs: path, rel: rel, size: info.Size()})
		return nil
	})
	if walkErr != nil {
		return nil, nil, nil, walkErr
	}

	// Build the tar.gz in memory.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for _, e := range entries {
		data, readErr := os.ReadFile(e.abs)
		if readErr != nil {
			return nil, nil, nil, fmt.Errorf("read %s: %w", e.abs, readErr)
		}

		hdr := &tar.Header{
			Name:    e.rel,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: time.Unix(0, 0), // deterministic
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, nil, nil, fmt.Errorf("tar header %s: %w", e.rel, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, nil, nil, fmt.Errorf("tar write %s: %w", e.rel, err)
		}

		files = append(files, e.rel)
	}

	if err := tw.Close(); err != nil {
		return nil, nil, nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, nil, nil, fmt.Errorf("close gzip: %w", err)
	}
	bundleBytes = buf.Bytes()

	// Find the manifest: prefer a .md file at the root level.
	manifestContent = findManifestInEntries(root, entries)
	if manifestContent == nil {
		return nil, nil, nil, fmt.Errorf("no .md manifest file found in %s", root)
	}

	return manifestContent, bundleBytes, files, nil
}

type bundleEntry struct {
	abs  string
	rel  string
	size int64
}

// checkBundlePath returns an error if the relative path is unsafe.
// Rejects: absolute paths, ".." components, or paths containing "..".
func checkBundlePath(rel string) error {
	if filepath.IsAbs(rel) {
		return fmt.Errorf("bundle path %q is absolute — refusing to package it", rel)
	}
	// Clean the path and check for ".." escape.
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("bundle path %q escapes the root — refusing to package it", rel)
	}
	// Check individual components.
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf("bundle path %q contains '..' — refusing to package it", rel)
		}
	}
	return nil
}

// checkSymlink checks that a file is not a symlink escaping the bundle root.
// If the file is a symlink, it resolves the target and verifies it stays inside root.
func checkSymlink(path, root string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return nil // not a symlink
	}
	target, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("readlink %s: %w", path, err)
	}
	// Resolve target relative to the symlink's directory.
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Can't resolve → refuse (broken or escaping symlink).
		return fmt.Errorf("symlink %s → %s cannot be resolved — refusing to package it", path, target)
	}
	// Resolve the root itself to handle OS-level symlinks (e.g. /var → /private/var on macOS).
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// Fall back to Abs if EvalSymlinks fails (root may not exist yet in edge cases).
		absRoot, err = filepath.Abs(root)
		if err != nil {
			return fmt.Errorf("abs root: %w", err)
		}
	}
	if !strings.HasPrefix(resolved+string(filepath.Separator), absRoot+string(filepath.Separator)) {
		return fmt.Errorf("symlink %s → %s escapes the bundle root — refusing to package it", path, resolved)
	}
	return nil
}

// findManifestInEntries returns the content of the first root-level .md file.
func findManifestInEntries(root string, entries []bundleEntry) []byte {
	// Prefer a root-level .md (no directory separator in rel).
	for _, e := range entries {
		if !strings.Contains(e.rel, string(filepath.Separator)) && strings.HasSuffix(strings.ToLower(e.rel), ".md") {
			data, err := os.ReadFile(e.abs)
			if err == nil {
				return data
			}
		}
	}
	// Fall back to any .md.
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e.rel), ".md") {
			data, err := os.ReadFile(e.abs)
			if err == nil {
				return data
			}
		}
	}
	return nil
}
