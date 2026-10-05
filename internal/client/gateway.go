// Package client provides an HTTP client for making authenticated calls to the gateway, with automatic OAuth refresh-token rotation.
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
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

// ErrSessionMigration is returned by Resolve when an email-only session from
// an older relay version exists but cannot be moved to the selected gateway.
var ErrSessionMigration = errors.New("relay now keeps one session per gateway. Run `relay login` once to continue")

// migrationNotice receives the one-line notice printed after a session move.
var migrationNotice io.Writer = os.Stderr

// migrateLegacySession moves an email-only session from an older relay
// version to the key bound to origin. An unbound token says nothing about
// which gateway issued it, so it moves only to an https gateway saved in the
// config file: never to one supplied per invocation by --gateway-url or
// GATEWAY_URL, which could hand it to a host the user never logged in to.
func migrateLegacySession(origin, email string) (keychain.TokenData, error) {
	legacy, err := keychain.ReadLegacyToken(email)
	if errors.Is(err, keychain.ErrNotFound) {
		return keychain.TokenData{}, ErrNotLoggedIn
	}
	if err != nil {
		return keychain.TokenData{}, err
	}
	if !strings.HasPrefix(origin, "https://") || !isSavedGateway(origin) {
		return keychain.TokenData{}, ErrSessionMigration
	}
	if err := keychain.WriteToken(origin, email, legacy); err != nil {
		return keychain.TokenData{}, err
	}
	if err := keychain.DeleteLegacyToken(email); err != nil {
		fmt.Fprintf(migrationNotice, "relay: moved your session to %s but could not remove the old entry (%v); run `relay logout --legacy` to remove it\n", origin, err)
		return legacy, nil
	}
	fmt.Fprintf(migrationNotice, "relay: moved your saved session for %s to %s\n", email, origin)
	return legacy, nil
}

func isSavedGateway(origin string) bool {
	cfg, err := config.Load()
	if err != nil || cfg.GatewayURL == "" {
		return false
	}
	saved, err := config.NormalizeGatewayURL(cfg.GatewayURL)
	return err == nil && saved == origin
}

// refreshSkew is how far ahead of the recorded expiry Client proactively
// refreshes — avoids a request racing an access token that expires mid-flight.
const refreshSkew = 30 * time.Second

// Client is an authenticated HTTP client for gateway API calls.
type Client struct {
	GatewayURL  string
	AccessToken string
	httpClient  *http.Client

	// Refresh context — populated only by Resolve for a keychain-backed OAuth session.
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

// tokenEndpointTimeout bounds each OAuth token-endpoint request. Gateway API
// requests have no client-wide limit; callers bound them with a context.
const tokenEndpointTimeout = 30 * time.Second

// NoRedirectHTTPClient returns an HTTP client for OAuth token-endpoint calls
// (code exchange, device flow, refresh): it never follows redirects and caps
// each request at tokenEndpointTimeout.
func NoRedirectHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: refuseRedirects, Timeout: tokenEndpointTimeout, Transport: versionTransport{}}
}

// gatewayHTTPClient returns the client for gateway API requests. It has no
// Timeout: a tool call may legitimately run for minutes, so every request is
// bounded by its caller's context instead.
func gatewayHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: refuseRedirects, Transport: versionTransport{}}
}

var clientVersion = "dev"

// SetVersion sets the relay version reported in the User-Agent and
// Relay-Client-Version headers of every gateway request.
func SetVersion(v string) {
	if v != "" {
		clientVersion = v
	}
}

func userAgent() string {
	return "relay/" + clientVersion + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// versionTransport adds the relay version headers to every request. These
// two headers are the only client details relay sends.
type versionTransport struct{}

func (versionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", userAgent())
	r.Header.Set("Relay-Client-Version", clientVersion)
	return http.DefaultTransport.RoundTrip(r)
}

// New creates a gateway client with a fixed bearer token and NO refresh
// capability. Use Resolve for the auto-refreshing session client every CLI
// command should prefer.
func New(gatewayURL, accessToken string) *Client {
	return &Client{
		GatewayURL:  strings.TrimRight(gatewayURL, "/"),
		AccessToken: accessToken,
		httpClient:  gatewayHTTPClient(),
		store:       keychainSessionStore{},
		locker:      flockSessionLocker{},
	}
}

// Resolve builds an authenticated Client for gateway calls.
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
	if errors.Is(err, keychain.ErrNotFound) {
		tokenData, err = migrateLegacySession(origin, email)
	}
	if err != nil {
		var unavailable *keychain.UnavailableError
		switch {
		case errors.As(err, &unavailable):
			return nil, fmt.Errorf("keychain unavailable: %v. Use GATEWAY_API_KEY for headless use", unavailable.Err)
		case errors.Is(err, ErrNotLoggedIn), errors.Is(err, ErrSessionMigration):
			return nil, err
		default:
			return nil, fmt.Errorf("%w; run `relay login` to replace the stored session", err)
		}
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

func (c *Client) bearerSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

func (c *Client) refreshLocked(ctx context.Context) error {
	unlock, err := c.locker.Lock(ctx, c.gatewayOrigin, c.email)
	if err != nil {
		return fmt.Errorf("acquire cross-process refresh lock: %w", err)
	}
	defer func() { _ = unlock.Unlock() }()

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

// Do performs an authenticated HTTP request.
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

// Post is PostContext without a deadline. Prefer PostContext so a hanging
// gateway cannot block the CLI.
func (c *Client) Post(path, contentType string, body io.Reader) (*http.Response, error) {
	return c.PostContext(context.Background(), path, contentType, body)
}

// Get is GetContext without a deadline. Prefer GetContext.
func (c *Client) Get(path string) (*http.Response, error) {
	return c.GetContext(context.Background(), path)
}

// PostContext sends an authenticated POST bounded only by ctx: the Client
// itself sets no request timeout, so long tool calls are limited by the
// caller's chosen deadline rather than a client-wide one.
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
