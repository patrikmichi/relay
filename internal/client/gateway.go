// Package client provides an HTTP client for making authenticated calls to
// the gateway, with automatic OAuth refresh-token rotation.
//
// A Client built via Resolve (the normal entry point for CLI commands)
// transparently:
//   - proactively refreshes the access token shortly before its recorded
//     expiry, and
//   - reactively refreshes-and-retries ONCE on a 401 response (covers a
//     token revoked/expired server-side ahead of the local clock, or a
//     Client whose expiry metadata is unknown/stale),
//
// persisting the rotated pair to the OS keychain before returning. A Client
// built via the bare New constructor (e.g. GATEWAY_API_KEY bearer auth) has
// no refresh capability and behaves exactly like a plain bearer-token HTTP
// client — see canRefreshLocked.
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// ErrNotLoggedIn is returned by Resolve when neither GATEWAY_API_KEY nor a
// usable CLI session (RELAY_EMAIL / persisted login email + a matching OS
// keychain entry) can be found.
var ErrNotLoggedIn = errors.New("not logged in — run `relay login` first, or set GATEWAY_API_KEY for non-interactive auth")

// refreshSkew is how far ahead of the recorded expiry Client proactively
// refreshes — avoids a request racing an access token that expires mid-flight.
const refreshSkew = 30 * time.Second

// Client is an authenticated HTTP client for gateway API calls.
type Client struct {
	GatewayURL  string
	AccessToken string
	httpClient  *http.Client

	// Refresh context — populated only by Resolve for a keychain-backed
	// OAuth session. Zero values (email == "") mean "no refresh capability"
	// — see canRefreshLocked. Never set for a GATEWAY_API_KEY bearer client.
	//
	// mu is the ONE synchronization policy for every mutable field below
	// (AccessToken, refreshToken, expiresAt): every read and every write
	// goes through it, with no unlocked fast path — see bearerSnapshot,
	// canRefreshLocked, and refreshLocked. email/gatewayOrigin are set once
	// at construction and never mutated afterward, so they're safe to read
	// without mu.
	mu            sync.Mutex
	email         string
	gatewayOrigin string
	refreshToken  string
	expiresAt     time.Time

	// store/locker coordinate refresh across independently invoked `relay`
	// processes sharing the same (gatewayOrigin, email) keychain entry —
	// see refreshLocked. Defaulted by New/Resolve; overridable via
	// WithSessionStore/WithSessionLocker for tests.
	store  SessionStore
	locker SessionLocker
}

// errRedirectRefused is returned via http.Client.CheckRedirect to refuse
// every HTTP redirect outright — a redirect could relocate a
// credential-bearing request (bearer header, or a secret in a token
// exchange/refresh body) to an origin the user never selected.
var errRedirectRefused = errors.New("refusing to follow HTTP redirect")

func refuseRedirects(_ *http.Request, _ []*http.Request) error {
	return errRedirectRefused
}

// NoRedirectHTTPClient returns a plain HTTP client that never follows
// redirects. Shared with internal/auth for token exchange/device-code/refresh
// calls.
func NoRedirectHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: refuseRedirects}
}

// New creates a gateway client with a fixed bearer token and NO refresh
// capability. Use Resolve for the auto-refreshing session client every CLI
// command should prefer.
func New(gatewayURL, accessToken string) *Client {
	return &Client{
		GatewayURL:  strings.TrimRight(gatewayURL, "/"),
		AccessToken: accessToken,
		httpClient:  NoRedirectHTTPClient(),
		store:       keychainSessionStore{},
		locker:      flockSessionLocker{},
	}
}

// Resolve builds an authenticated Client for gateway calls.
//
// gatewayURL is normalized and policy-checked (config.NormalizeGatewayURL)
// first, so an invalid or insecure-transport gateway value is rejected here
// even for a caller that skipped internal/cli/gateway.go's
// resolveGatewayURLOrFailClosed.
//
// Resolution order:
//  1. GATEWAY_API_KEY env var — non-interactive bearer auth (scripts, cron,
//     CI). Mirrors the gateway's own bearer-key fallback (lib/api-keys.ts /
//     verifyMcpAuth). No refresh capability. Scoped to exactly this resolved
//     gateway for this invocation only — never persisted, never carried to
//     a different origin.
//  2. RELAY_EMAIL env var, or the email persisted at the last successful
//     `relay login` (config.ResolveEmail) → the OS keychain OAuth pair bound
//     to BOTH that email AND this normalized gateway origin, wrapped in a
//     Client with automatic pre-expiry / on-401 refresh.
//
// Returns ErrNotLoggedIn when neither path resolves to usable credentials.
// If a pre-gateway-identity ("legacy") session exists for the resolved
// email but no session is bound to this gateway, the returned error
// explains that a fresh login is required — it never falls back to using
// the legacy, unscoped session automatically.
//
// opts is normally empty; it exists so tests can inject a fake
// SessionStore/SessionLocker (WithSessionStore/WithSessionLocker) for
// deterministic refresh-coordination coverage without touching the OS
// keychain.
func Resolve(gatewayURL string, opts ...ClientOption) (*Client, error) {
	origin, err := config.NormalizeGatewayURL(gatewayURL)
	if err != nil {
		return nil, err
	}

	if apiKey := os.Getenv("GATEWAY_API_KEY"); apiKey != "" {
		c := New(origin, apiKey)
		for _, opt := range opts {
			opt(c)
		}
		return c, nil
	}

	email, err := config.ResolveEmail()
	if err != nil {
		return nil, ErrNotLoggedIn
	}

	tokenData, err := keychain.ReadToken(origin, email)
	if err != nil {
		if keychain.HasLegacyToken(email) {
			return nil, fmt.Errorf(
				"%w — found a pre-existing session for %s with no gateway binding; "+
					"run `relay login` again to create a session scoped to %s, then remove the old one with `relay logout --legacy`",
				ErrNotLoggedIn, email, origin,
			)
		}
		return nil, ErrNotLoggedIn
	}

	c := New(origin, tokenData.AccessToken)
	c.email = email
	c.gatewayOrigin = origin
	c.refreshToken = tokenData.RefreshToken
	if tokenData.ExpiresAt > 0 {
		c.expiresAt = time.Unix(tokenData.ExpiresAt, 0)
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Email returns the identity this Client was resolved for, or "" for a
// bearer-only Client (New / GATEWAY_API_KEY) that has no CLI session.
func (c *Client) Email() string {
	return c.email
}

// GatewayOrigin returns the normalized gateway identity this Client's
// keychain-backed OAuth session (if any) is bound to, or "" for a
// bearer-only Client (New / GATEWAY_API_KEY) that has no CLI session.
func (c *Client) GatewayOrigin() string {
	return c.gatewayOrigin
}

// canRefreshLocked reports whether this Client holds enough context (email
// + gateway identity + refresh token) to attempt a rotation. Caller must
// hold c.mu — refreshToken is mutable state (see the Client doc comment).
func (c *Client) canRefreshLocked() bool {
	return c.email != "" && c.gatewayOrigin != "" && c.refreshToken != ""
}

// bearerSnapshot returns a point-in-time copy of the current access token
// under c.mu — never a reference into live Client state, so a request that
// captured its snapshot is unaffected by a refresh that rotates
// AccessToken concurrently on another goroutine sharing this Client (R25).
func (c *Client) bearerSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// refreshLocked performs a coordinated refresh + persistence. Caller must
// hold c.mu — that serializes every in-process caller sharing this Client
// (R25). Cross-process coordination is separate: refreshLocked additionally
// acquires an OS-backed lock scoped to (gatewayOrigin, email) so independent
// `relay` invocations racing to refresh the same session never both submit
// the same single-use refresh token (R24). ctx bounds both the lock wait
// and the HTTP call — a caller's deadline covers the whole refresh, not just
// the request that triggered it (R23).
func (c *Client) refreshLocked(ctx context.Context) error {
	unlock, err := c.locker.Lock(ctx, c.gatewayOrigin, c.email)
	if err != nil {
		return fmt.Errorf("acquire cross-process refresh lock: %w", err)
	}
	defer unlock.Unlock()

	// Another process may have rotated the token while we waited for the
	// lock. A single-use refresh token IS this session's generation marker:
	// if the stored value no longer matches the one we're holding, someone
	// else already spent it and rotated — adopt their result instead of
	// submitting our now-stale copy, which the gateway would reject as
	// reuse and revoke the whole token family for.
	if current, readErr := c.store.Read(c.gatewayOrigin, c.email); readErr == nil &&
		current.RefreshToken != "" && current.RefreshToken != c.refreshToken {
		c.AccessToken = current.AccessToken
		c.refreshToken = current.RefreshToken
		if current.ExpiresAt > 0 {
			c.expiresAt = time.Unix(current.ExpiresAt, 0)
		}
		return nil
	}

	result, err := refreshOAuthToken(ctx, c.GatewayURL, c.refreshToken)
	if err != nil {
		return err
	}

	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	// Persist the rotated pair BEFORE updating in-memory state — a refresh
	// token is single-use with reuse detection (gateway
	// lib/oauth/token-store.ts): if we update AccessToken/RefreshToken here
	// but the keychain write then fails, this process's next call would
	// still work (in-memory pair is valid), but every OTHER process reading
	// the keychain (a concurrently-running `relay` invocation, or the very
	// next command) would present the now-spent old refresh token and get
	// its whole token family revoked. A persistence failure here is
	// therefore returned as a visible, explicit error — never swallowed,
	// and NEVER retried with this now-spent refresh token within this same
	// invocation. Recovering may require running `relay login` again; this
	// never falls back to writing the token to disk unencrypted.
	if err := c.store.Write(c.gatewayOrigin, c.email, keychain.TokenData{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    expiresIn,
		ExpiresAt:    expiresAt.Unix(),
	}); err != nil {
		return fmt.Errorf("refresh succeeded but persisting the rotated token to the OS keychain failed (reauthentication via `relay login` may be required): %w", err)
	}

	c.AccessToken = result.AccessToken
	c.refreshToken = result.RefreshToken
	c.expiresAt = expiresAt
	return nil
}

// maybeProactiveRefresh refreshes ahead of a known expiry, bounded by ctx.
// Best-effort: a failure here is NOT fatal — the request proceeds with the
// current token, and the reactive on-401 path in Do is the backstop.
func (c *Client) maybeProactiveRefresh(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.canRefreshLocked() || c.expiresAt.IsZero() {
		return
	}
	if time.Now().Add(refreshSkew).Before(c.expiresAt) {
		return
	}
	_ = c.refreshLocked(ctx) // best-effort — Do's reactive 401 path is the backstop
}

// unauthorizedResponse builds a synthetic 401 response with an empty,
// already-closed-equivalent body — used when Do must report "still
// unauthorized" after an internal refresh attempt without a live
// *http.Response to hand back (the real one was already drained/closed).
func unauthorizedResponse(req *http.Request) *http.Response {
	return &http.Response{
		Status:     "401 Unauthorized",
		StatusCode: http.StatusUnauthorized,
		Proto:      "HTTP/1.1",
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Request:    req,
	}
}

// cloneRequest builds a fresh copy of req for a retry after the
// Authorization header changes. Returns an error when the original request
// had a body that cannot be safely re-read (GetBody unset) — http.NewRequest
// populates GetBody automatically for *bytes.Buffer / *bytes.Reader /
// *strings.Reader bodies, which covers every call site in this codebase
// (see Client.Post).
func cloneRequest(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, fmt.Errorf("request body cannot be safely retried (no GetBody)")
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("re-read request body: %w", err)
		}
		clone.Body = body
	}
	return clone, nil
}

// Do performs an authenticated HTTP request. Sets Authorization: Bearer
// <AccessToken> on every request.
//
// For a Client resolved via Resolve (OAuth, keychain-backed): proactively
// refreshes when the access token is at/near its recorded expiry, and — as
// a backstop — transparently refreshes and retries ONCE on a 401 response.
// The rotated pair is persisted to the OS keychain before the retry.
//
// If the refresh itself fails (e.g. the refresh token was also
// revoked/expired), or the original request's body cannot be safely
// re-sent, the response returned is a 401 — indistinguishable in status
// from "no refresh was attempted" — so every existing call site's
//
//	if resp.StatusCode == http.StatusUnauthorized { ...run `relay login`... }
//
// keeps working with zero changes; it now means "refresh was attempted and
// did not resolve the problem" rather than "no refresh capability existed".
//
// A Client with no refresh capability (GATEWAY_API_KEY, or built via the
// bare New constructor) skips all of the above — the request is sent
// exactly once, matching the pre-refresh-support behavior of this package.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	c.maybeProactiveRefresh(ctx)
	req.Header.Set("Authorization", "Bearer "+c.bearerSnapshot())

	resp, err := c.httpClient.Do(req)
	c.mu.Lock()
	refreshable := c.canRefreshLocked()
	c.mu.Unlock()
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !refreshable {
		return resp, err
	}

	// 401 — attempt exactly one refresh-and-retry.
	_ = resp.Body.Close()

	c.mu.Lock()
	refreshErr := c.refreshLocked(ctx)
	retryToken := c.AccessToken
	c.mu.Unlock()
	if refreshErr != nil {
		return unauthorizedResponse(req), nil
	}

	retryReq, cloneErr := cloneRequest(req)
	if cloneErr != nil {
		// Cannot safely retry (unreadable body) — the keychain now holds a
		// freshly-refreshed, VALID token for the next command even though
		// THIS call still reports 401.
		return unauthorizedResponse(req), nil
	}
	retryReq.Header.Set("Authorization", "Bearer "+retryToken)
	return c.httpClient.Do(retryReq)
}

// Post is a convenience method for POST requests with a JSON body. The
// request carries context.Background() — i.e. no deadline. Callers that
// need a bounded request (e.g. internal/catalog's search/download calls)
// should use PostContext/GetContext instead so a hanging gateway can't wedge
// the CLI forever; this method is left as-is for the many existing call
// sites (sync, publish, service_cmd, tokens, ...) that legitimately want no
// deadline here, so widening this signature doesn't ripple across them.
func (c *Client) Post(path, contentType string, body io.Reader) (*http.Response, error) {
	return c.PostContext(context.Background(), path, contentType, body)
}

// Get is a convenience method for GET requests — see Post's doc comment on
// why this has no deadline; use GetContext for a bounded request.
func (c *Client) Get(path string) (*http.Response, error) {
	return c.GetContext(context.Background(), path)
}

// PostContext is Post with an explicit, caller-supplied context — the
// request is aborted the moment ctx's deadline/cancellation fires, even if
// the gateway never responds. Use with context.WithTimeout for any call site
// that must be bounded (internal/catalog's download/search calls; see m3 in
// the Go review) — this deliberately does NOT change the shared http.Client
// itself (no Client-wide Timeout), so unrelated long-running relay dispatch
// calls through Post/Get are unaffected.
func (c *Client) PostContext(ctx context.Context, path, contentType string, body io.Reader) (*http.Response, error) {
	url := c.GatewayURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	return c.Do(req)
}

// GetContext is PostContext's GET counterpart — see PostContext's doc comment.
func (c *Client) GetContext(ctx context.Context, path string) (*http.Response, error) {
	url := c.GatewayURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	return c.Do(req)
}
