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
// client — see canRefresh.
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
	// — see canRefresh. Never set for a GATEWAY_API_KEY bearer client.
	mu           sync.Mutex
	email        string
	refreshToken string
	expiresAt    time.Time
}

// New creates a gateway client with a fixed bearer token and NO refresh
// capability. Use Resolve for the auto-refreshing session client every CLI
// command should prefer.
func New(gatewayURL, accessToken string) *Client {
	return &Client{
		GatewayURL:  strings.TrimRight(gatewayURL, "/"),
		AccessToken: accessToken,
		httpClient:  &http.Client{},
	}
}

// Resolve builds an authenticated Client for gateway calls.
//
// Resolution order:
//  1. GATEWAY_API_KEY env var — non-interactive bearer auth (scripts, cron,
//     CI). Mirrors the gateway's own bearer-key fallback (lib/api-keys.ts /
//     verifyMcpAuth). No refresh capability: API keys don't rotate through
//     the OAuth flow, so there's nothing to refresh.
//  2. RELAY_EMAIL env var, or the email persisted at the last successful
//     `relay login` (config.ResolveEmail) → the matching OS keychain OAuth
//     pair, wrapped in a Client with automatic pre-expiry / on-401 refresh.
//
// Returns ErrNotLoggedIn when neither path resolves to usable credentials.
func Resolve(gatewayURL string) (*Client, error) {
	if apiKey := os.Getenv("GATEWAY_API_KEY"); apiKey != "" {
		return New(gatewayURL, apiKey), nil
	}

	email, err := config.ResolveEmail()
	if err != nil {
		return nil, ErrNotLoggedIn
	}

	tokenData, err := keychain.ReadToken(email)
	if err != nil {
		return nil, ErrNotLoggedIn
	}

	c := New(gatewayURL, tokenData.AccessToken)
	c.email = email
	c.refreshToken = tokenData.RefreshToken
	if tokenData.ExpiresAt > 0 {
		c.expiresAt = time.Unix(tokenData.ExpiresAt, 0)
	}
	return c, nil
}

// Email returns the identity this Client was resolved for, or "" for a
// bearer-only Client (New / GATEWAY_API_KEY) that has no CLI session.
func (c *Client) Email() string {
	return c.email
}

// canRefresh reports whether this Client holds enough context (email +
// refresh token) to attempt a rotation.
func (c *Client) canRefresh() bool {
	return c.email != "" && c.refreshToken != ""
}

// refreshLocked performs the refresh + keychain persistence. Caller must
// hold c.mu.
func (c *Client) refreshLocked() error {
	result, err := refreshOAuthToken(c.GatewayURL, c.refreshToken)
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
	// its whole token family revoked. Failing the refresh outright on a
	// write error is safer than risking that.
	if err := keychain.WriteToken(c.email, keychain.TokenData{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		Email:        c.email,
		ExpiresIn:    expiresIn,
		ExpiresAt:    expiresAt.Unix(),
	}); err != nil {
		return fmt.Errorf("refresh succeeded but persisting the rotated token failed: %w", err)
	}

	c.AccessToken = result.AccessToken
	c.refreshToken = result.RefreshToken
	c.expiresAt = expiresAt
	return nil
}

// maybeProactiveRefresh refreshes ahead of a known expiry. Best-effort: a
// failure here is NOT fatal — the request proceeds with the current token,
// and the reactive on-401 path in Do is the backstop.
func (c *Client) maybeProactiveRefresh() {
	if !c.canRefresh() || c.expiresAt.IsZero() {
		return
	}
	if time.Now().Add(refreshSkew).Before(c.expiresAt) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check after acquiring the lock — a concurrent call on this same
	// Client may have already refreshed while we were waiting.
	if time.Now().Add(refreshSkew).Before(c.expiresAt) {
		return
	}
	_ = c.refreshLocked() // best-effort — Do's reactive 401 path is the backstop
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
	c.maybeProactiveRefresh()
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !c.canRefresh() {
		return resp, err
	}

	// 401 — attempt exactly one refresh-and-retry.
	_ = resp.Body.Close()

	c.mu.Lock()
	refreshErr := c.refreshLocked()
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
	retryReq.Header.Set("Authorization", "Bearer "+c.AccessToken)
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
