// Package catalog implements relay's Phase-2 "GatewaySource" — the
// gateway-backed counterpart to agentport.LoadGenericSkill's local-path
// source. It downloads a governed skill bundle from the gateway catalog's
// download endpoint, verifies it end to end (content hash + scan verdict),
// path-safe extracts it, and loads it through the SAME
// agentport.LoadGenericSkill code path a local `relay skill install <path>`
// uses — so everything downstream (Migrate/Write/manifest) is unmodified,
// shipped Phase-1 code.
//
// Deliberately NOT part of internal/agentport: that package is intentionally
// gateway-agnostic (stdlib + gopkg.in/yaml.v3 only, see its package doc) so
// it can later be extracted verbatim into a standalone OSS module. This
// package is the only place relay-proprietary gateway/auth code meets the
// skill-manager engine — see FetchSkill's Doer parameter.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/patrikmichi/relay/internal/agentport"
)

// Doer is the minimal HTTP interface FetchSkill needs to reach the gateway's
// governed download endpoint. *client.Client (relay's auto-refreshing
// OAuth/keychain-backed gateway client, or a GATEWAY_API_KEY bearer client)
// satisfies this directly — this package never imports internal/client, to
// keep the dependency direction one-way (cli -> catalog -> agentport;
// catalog -> client only via this narrow interface at the call site, not a
// package import), and so tests can substitute an httptest-backed fake.
type Doer interface {
	// GetContext sends an authenticated GET to path (relative to the gateway
	// base URL, e.g. "/api/catalog/skills/res_abc/download?version=1.4.2"),
	// bounded by ctx, and returns the raw HTTP response. FetchSkill supplies
	// a context.WithTimeout(downloadTimeout) — a hanging gateway aborts the
	// request instead of blocking forever, without imposing a client-wide
	// timeout that would also affect unrelated, legitimately long-running
	// gateway calls (m3 in the Go review).
	GetContext(ctx context.Context, path string) (*http.Response, error)
}

// downloadTimeout bounds a single catalog download request end to end
// (bundles are capped at MaxBundleBytes = 50 MiB, so this is generous but
// not unbounded). A package-level var (not a const) so tests can shrink it
// to exercise the bounded-timeout path without a slow test.
var downloadTimeout = 120 * time.Second

// Response header names the download endpoint is contracted to set. See
// vault/plans/claude-infra/relay-gateway-source-install-2026-07-05.md §4.6.
const (
	headerContentSha256 = "X-Skill-Content-Sha256"
	headerVersion       = "X-Skill-Version"
	headerCatalogID     = "X-Skill-Catalog-Id"
	headerScanVerdict   = "X-Skill-Scan-Verdict"
)

// downloadPathPrefix / downloadPathSuffix build the download endpoint path:
// GET /api/catalog/skills/<id>/download[?version=&channel=].
const (
	downloadPathPrefix = "/api/catalog/skills/"
	downloadPathSuffix = "/download"
)

// Size/entry caps mirrored from the gateway's own BUNDLE_ENTRY_LIMIT /
// BUNDLE_TOTAL_LIMIT (design spec §6.2 step 5) — enforced client-side too so
// a compromised, buggy, or MITM'd gateway response can't gzip/entry-bomb the
// local extraction. MaxBundleBytes bounds BOTH the on-wire (still-gzipped)
// response body and the total decompressed bytes written to disk.
const (
	MaxBundleEntries = 500
	MaxBundleBytes   = 50 << 20 // 50 MiB
)

// Sentinel errors — distinguished by the CLI for its exit messaging (design
// spec §6.6 degradation table). Wrapped with additional context via %w, so
// errors.Is still matches these after wrapping.
var (
	ErrUnauthorized      = errors.New("unauthorized (401)")
	ErrNoAccess          = errors.New("access denied (403)")
	ErrNotFound          = errors.New("catalog resource/version not found (404)")
	ErrNotDownloadable   = errors.New("version is not in a downloadable state (409)")
	ErrRateLimited       = errors.New("rate limited (429)")
	ErrChecksumMismatch  = errors.New("downloaded content sha256 does not match X-Skill-Content-Sha256 — aborting before extract")
	ErrScanVerdictFailed = errors.New("scan verdict is missing, unscanned, or failed — refusing to install")
)

// FetchSkill downloads catalog id (a res_… resource id or a slug) at version
// (optional exact semver — "" resolves to the latest on channel) / channel
// (optional — "" is the gateway's default channel, normally "stable") from
// the gateway's governed download endpoint, verifies the response end to
// end, path-safe extracts the canonical bundle into a temp dir, loads it via
// agentport.LoadGenericSkill (deleting the temp dir before returning — the
// Skill IR holds the resource bytes in memory, so the temp dir is scratch
// space only), and stamps gateway provenance onto the result.
//
// Fail-closed, in this order:
//  1. HTTP status: 401/403/404/409/429 map to the sentinel errors above;
//     any other non-200 is a generic error. No body is trusted until 200.
//  2. sha256(body) must equal the X-Skill-Content-Sha256 header. Mismatch
//     aborts before any extraction — nothing is written to the temp dir.
//  3. X-Skill-Scan-Verdict must indicate every applicable gate passed (or
//     na/skip) — see verifyScanVerdict for the two accepted header shapes.
//     Missing, unparseable, or any failed gate aborts before extraction.
//  4. Tar entries must be path-safe (no absolute paths, no ".." escapes, no
//     symlinks/hardlinks/device files) and within MaxBundleEntries /
//     MaxBundleBytes — see extractTarGz.
//
// The returned Skill's Provenance is
// {SourceProvider: "gateway", CatalogID: id, Version: <resolved version>}.
func FetchSkill(doer Doer, id, version, channel string) (*agentport.Skill, error) {
	if doer == nil {
		return nil, fmt.Errorf("catalog: no gateway client configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("catalog: empty catalog id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()

	resp, err := doer.GetContext(ctx, downloadPath(id, version, channel))
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

	if err := verifyChecksum(body, resp.Header.Get(headerContentSha256)); err != nil {
		return nil, fmt.Errorf("catalog: %s: %w", id, err)
	}
	if err := verifyScanVerdict(resp.Header.Get(headerScanVerdict)); err != nil {
		return nil, fmt.Errorf("catalog: %s: %w", id, err)
	}

	tmpDir, err := os.MkdirTemp("", "relay-catalog-skill-*")
	if err != nil {
		return nil, fmt.Errorf("catalog: create temp extraction dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGz(body, tmpDir); err != nil {
		return nil, fmt.Errorf("catalog: extract bundle for %s: %w", id, err)
	}

	skill, err := agentport.LoadGenericSkill(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("catalog: load extracted bundle for %s: %w", id, err)
	}

	resolvedVersion := resp.Header.Get(headerVersion)
	if resolvedVersion == "" {
		resolvedVersion = version
	}
	catalogID := resp.Header.Get(headerCatalogID)
	if catalogID == "" {
		catalogID = id
	}
	skill.Provenance = agentport.Provenance{
		SourceProvider: agentport.ProviderID("gateway"),
		CatalogID:      catalogID,
		Version:        resolvedVersion,
	}
	return skill, nil
}

// downloadPath builds the download endpoint request path, URL-escaping id
// and attaching ?version=/&channel= when set.
func downloadPath(id, version, channel string) string {
	p := downloadPathPrefix + url.PathEscape(id) + downloadPathSuffix
	q := url.Values{}
	if version != "" {
		q.Set("version", version)
	}
	if channel != "" {
		q.Set("channel", channel)
	}
	if enc := q.Encode(); enc != "" {
		p += "?" + enc
	}
	return p
}

// statusToError maps a download endpoint HTTP status to a sentinel error
// (or nil for 200). For an unrecognized non-200 status, a snippet of the
// response body is included for diagnostics.
func statusToError(status int, body io.Reader) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrNoAccess
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrNotDownloadable
	case http.StatusTooManyRequests:
		return ErrRateLimited
	default:
		snippet, _ := io.ReadAll(io.LimitReader(body, 2<<10))
		return fmt.Errorf("unexpected status %d: %s", status, strings.TrimSpace(string(snippet)))
	}
}

// readLimited reads at most limit+1 bytes from r, erroring if the stream
// exceeds limit — a client-side mirror of the gateway's own
// ARTIFACT_MAX_BYTES/BUNDLE_TOTAL_LIMIT cap, applied before any bytes are
// trusted or decompressed.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("bundle exceeds size cap (%d bytes)", limit)
	}
	return data, nil
}

// verifyChecksum compares sha256(body) against the hex-encoded want value
// (the X-Skill-Content-Sha256 header). Returns ErrChecksumMismatch on any
// mismatch, including a missing/empty header — fail-closed, never "assume
// OK" when the header is absent.
func verifyChecksum(body []byte, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return fmt.Errorf("%w: response missing %s header", ErrChecksumMismatch, headerContentSha256)
	}
	got := sha256Hex(body)
	if got != want {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, got, want)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
