package catalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patrikmichi/relay/internal/agentport"
)

// resourceDownloadPathPrefix is the generalized (skill+agent) download
// endpoint's path prefix, distinct from
// downloadPathPrefix (the legacy skill-only route FetchSkill still targets).
const resourceDownloadPathPrefix = "/api/catalog/resources/"

// headerResourceContentSha256 is the type-agnostic content-hash response
// header the generalized resources download route sends ALONGSIDE the
// legacy headerContentSha256 (X-Skill-Content-Sha256) alias — see the
// gateway's RESOURCE_CONTENT_SHA256_HEADER (resource-download.ts). FetchAgent
// prefers this header and falls back to headerContentSha256 so it keeps
// working against a gateway build that (for whatever reason) only sends the
// legacy alias.
const headerResourceContentSha256 = "X-Resource-Content-Sha256"

// resourceDownloadPath builds the generalized download endpoint's request
// path for id, mirroring downloadPath's query-string handling exactly (see
// buildDownloadPath).
func resourceDownloadPath(id, version, channel string) string {
	return buildDownloadPath(resourceDownloadPathPrefix, id, version, channel)
}

// agentClaudeAdapterID is the fixed provider id FetchAgent loads every
// downloaded agent bundle through — see the package doc comment above for
// why this is deliberately NOT provider-neutral/generic.
const agentClaudeAdapterID = agentport.ProviderID("claude")

// FetchAgent downloads catalog id (a res_… resource id or a slug) at version
// (optional exact semver — "" resolves to the latest on channel) / channel
// (optional — "" is the gateway's default channel) from the gateway's
// governed generalized download endpoint, verifies the response end to end,
// path-safe extracts the canonical bundle into a temp dir, loads the single
// root-level `<name>.md` file it contains via the Claude agent adapter's
// Load, and stamps gateway provenance onto the result.
//
// Fail-closed, in this order — identical posture to FetchSkill:
//  1. HTTP status: 401/403/404/409/429 map to the sentinel errors FetchSkill
//     already exports; any other non-200 is a generic error.
//  2. sha256(body) must equal X-Resource-Content-Sha256 (preferred) or
//     X-Skill-Content-Sha256 (legacy alias).
//  3. X-Skill-Scan-Verdict must indicate every applicable gate passed (or
//     na/skip) — see verifyScanVerdict.
//  4. The response must declare resource type "agent" and its catalog
//     identity/version must match the request.
//  5. Tar entries must be path-safe (extractTarGz) and within
//     MaxBundleEntries/MaxBundleBytes.
//  6. The extracted bundle must contain EXACTLY one root-level `*.md` file
//     and nothing else — zero, several, or extra resources are refused.
//
// The returned Agent's Provenance is {SourceProvider: "claude" (the format
// the bundle was loaded as), CatalogID, Version: <resolved version>}.
func FetchAgent(doer Doer, id, version, channel string) (*agentport.Agent, error) {
	if doer == nil {
		return nil, fmt.Errorf("catalog: no gateway client configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("catalog: empty catalog id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()

	resp, err := doer.GetContext(ctx, resourceDownloadPath(id, version, channel))
	if err != nil {
		return nil, fmt.Errorf("catalog: download %s: %w", id, err)
	}
	defer resp.Body.Close()

	if err := statusToError(resp.StatusCode, resp.Body); err != nil {
		return nil, fmt.Errorf("catalog: %s: %w", id, err)
	}

	body, err := readLimited(resp.Body, MaxBundleBytes)
	if err != nil {
		return nil, fmt.Errorf("catalog: read bundle for %s: %w", id, err)
	}

	contentSha256 := resp.Header.Get(headerResourceContentSha256)
	if contentSha256 == "" {
		contentSha256 = resp.Header.Get(headerContentSha256)
	}
	if err := verifyChecksum(body, contentSha256); err != nil {
		return nil, fmt.Errorf("catalog: %s: %w", id, err)
	}
	if err := verifyScanVerdict(resp.Header.Get(headerScanVerdict)); err != nil {
		return nil, fmt.Errorf("catalog: %s: %w", id, err)
	}

	if resp.Header.Get("X-Resource-Type") != "agent" {
		return nil, fmt.Errorf("catalog: agent resource type missing or mismatched; upgrade the gateway")
	}
	if resp.Header.Get(headerCatalogID) == "" || (strings.HasPrefix(id, "res_") && resp.Header.Get(headerCatalogID) != id) {
		return nil, fmt.Errorf("catalog: agent identity missing or mismatched")
	}
	if resp.Header.Get(headerVersion) == "" || (version != "" && resp.Header.Get(headerVersion) != version) {
		return nil, fmt.Errorf("catalog: agent version missing or mismatched")
	}

	tmpDir, err := os.MkdirTemp("", "relay-catalog-agent-*")
	if err != nil {
		return nil, fmt.Errorf("catalog: create temp extraction dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGz(body, tmpDir); err != nil {
		return nil, fmt.Errorf("catalog: extract bundle for %s: %w", id, err)
	}

	agentMdPath, err := findAgentBundleFile(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("catalog: %s: %w", id, err)
	}

	claudeAdapter, ok := agentport.AgentAdapterByID(agentClaudeAdapterID)
	if !ok {
		// Packaging bug, not a runtime/user condition — the claude agent
		// provider config is always embedded (mirrors mustBuiltinAgentAdapter's
		// panic rationale, but this is a library call site so it returns an
		// error instead of panicking).
		return nil, fmt.Errorf("catalog: internal error: claude agent adapter not loaded")
	}

	agent, err := claudeAdapter.Load(agentMdPath)
	if err != nil {
		return nil, fmt.Errorf("catalog: load extracted agent bundle for %s: %w", id, err)
	}

	resolvedVersion := resp.Header.Get(headerVersion)
	if resolvedVersion == "" {
		resolvedVersion = version
	}
	catalogID := resp.Header.Get(headerCatalogID)
	if catalogID == "" {
		catalogID = id
	}
	agent.Provenance = agentport.Provenance{
		SourceProvider: agentClaudeAdapterID,
		CatalogID:      catalogID,
		Version:        resolvedVersion,
	}
	return agent, nil
}

// findAgentBundleFile returns the path to the single root-level `*.md` file
// directly inside dir — the canonical agent-bundle shape (see the package
// doc comment). Zero or more than one match is an error: this package never
// guesses which file is the agent definition.
func findAgentBundleFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read extracted bundle: %w", err)
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			return "", fmt.Errorf("agent bundle contains unsupported resource directory %q", e.Name())
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			candidates = append(candidates, e.Name())
		} else {
			return "", fmt.Errorf("agent bundle contains unsupported file %q", e.Name())
		}
	}
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("agent bundle contains no root-level .md file")
	case 1:
		return filepath.Join(dir, candidates[0]), nil
	default:
		return "", fmt.Errorf("agent bundle contains multiple root-level .md files (%s) — expected exactly one", strings.Join(candidates, ", "))
	}
}
