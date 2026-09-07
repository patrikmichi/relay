// FetchAgent is the Agent-IR analogue of FetchSkill (gateway_source.go) —
// relay-cli-completion plan D4. It downloads a governed AGENT bundle from
// the gateway catalog's generalized download endpoint
// (GET /api/catalog/resources/<id>/download, added gateway-side by D1/D2),
// verifies it end to end (content hash + scan verdict), path-safe extracts
// it, and loads the single agent file it contains through the SAME
// provider-adapter Load code path `relay agent migrate`/`relay agent
// install` use — so everything downstream (MigrateAgent/WriteAgent/manifest)
// is unmodified, shipped Phase-C code.
//
// Bundle-shape assumption (see plan D4): the catalog stores every published
// agent resource as a single flat `<name>.md` frontmatter+body file at the
// tarball root — the SAME canonical shape `relay agent publish <path>`
// already uploads (publish.go's buildBundle picks the root-level .md as the
// manifest) and the shape Claude Code's own agent files use. This package
// deliberately loads that file through agentport's CLAUDE agent adapter
// (agentport.AgentAdapterByID("claude")), not a generic/provider-neutral
// parser: the canonical catalog bundle format for an agent IS Claude's
// frontmatter+body shape (name/description/model/tools/... keys — see
// agents/claude.yml), exactly as LoadGenericSkill treats the Agent-Skills
// SKILL.md shape as the canonical skill format regardless of which skill
// provider originally authored it. A subsequent MigrateAgent to any other
// target adapter then only drops what that target's format genuinely can't
// represent — the same fidelity-loss reporting every other agent
// migrate/install path already gives the user. A bundle containing zero or
// more than one root-level .md file is rejected outright (ambiguous — this
// package refuses to guess which file is the agent).
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
// endpoint's path prefix — the D2 gateway route, distinct from
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
// (optional — "" is the gateway's default channel, normally "stable") from
// the gateway's governed generalized download endpoint, verifies the
// response end to end, path-safe extracts the canonical bundle into a temp
// dir, loads the single root-level `<name>.md` file it contains via the
// Claude agent adapter's Load (deleting the temp dir before returning — the
// Agent IR holds its body in memory, so the temp dir is scratch space only),
// and stamps gateway provenance onto the result.
//
// Fail-closed, in this order — identical posture to FetchSkill:
//  1. HTTP status: 401/403/404/409/429 map to the sentinel errors FetchSkill
//     already exports; any other non-200 is a generic error. No body is
//     trusted until 200.
//  2. sha256(body) must equal X-Resource-Content-Sha256 (preferred) or
//     X-Skill-Content-Sha256 (legacy alias, same value). Mismatch aborts
//     before any extraction.
//  3. X-Skill-Scan-Verdict must indicate every applicable gate passed (or
//     na/skip) — see verifyScanVerdict. Missing, unparseable, or any failed
//     gate aborts before extraction.
//  4. Tar entries must be path-safe (extractTarGz) and within
//     MaxBundleEntries/MaxBundleBytes.
//  5. The extracted bundle must contain EXACTLY one root-level `*.md` file —
//     zero or more than one is refused rather than guessed at.
//
// The returned Agent's Provenance is
// {SourceProvider: "gateway", CatalogID: id, Version: <resolved version>}.
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
		SourceProvider: agentport.ProviderID("gateway"),
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
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			candidates = append(candidates, e.Name())
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
