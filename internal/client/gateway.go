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

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/dpop"
	"github.com/patrikmichi/zuano/internal/keychain"
)

// ErrNotLoggedIn is returned by Resolve when neither ZUANO_API_KEY nor a
// usable CLI session (ZUANO_EMAIL / persisted login email + a matching OS
// keychain entry) can be found.
var ErrNotLoggedIn = errors.New("not logged in — run `" + brand.Cmd("login") + "` first, or set ZUANO_API_KEY for non-interactive auth")

// ErrAgentNotLoggedIn is returned by Resolve when ZUANO_AGENT selects the
// agent profile and no agent session is stored. It never falls back to the
// human session: that session is not held to approvals.
var ErrAgentNotLoggedIn = errors.New("no agent session. Run `" + brand.Cmd("login --agent") + "` (ZUANO_AGENT=1 selects the agent profile), or unset ZUANO_AGENT to use your own session")

// ActiveProfile is the keychain profile this process uses: the agent
// profile when ZUANO_AGENT is set, the human one otherwise.
func ActiveProfile() keychain.Profile {
	if config.AgentMode() {
		return keychain.ProfileAgent
	}
	return keychain.ProfileHuman
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
	profile       keychain.Profile
	email         string
	gatewayOrigin string
	refreshToken  string
	expiresAt     time.Time

	// store/locker coordinate refresh across independently invoked `zuano`
	// processes sharing the same (gatewayOrigin, email) keychain entry —
	// see refreshLocked. Defaulted by New/Resolve; overridable via
	// WithSessionStore/WithSessionLocker for tests.
	store  SessionStore
	locker SessionLocker

	stepUp StepUpHandler
	proofs ProofSigner
	// dpopKey is the reference of the device key the session is bound to.
	dpopKey string
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

const (
	// responseHeaderTimeout is the backstop for a gateway that accepts the
	// connection and never answers. It is far above any tool call a caller
	// allows, so a caller's own context is what normally ends a request.
	responseHeaderTimeout = 10 * time.Minute
)

// defaultRequestTimeout bounds Get and Post, whose callers pass no context.
var defaultRequestTimeout = 2 * time.Minute

var gatewayTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}()

// gatewayHTTPClient returns the client for gateway API requests. It has no
// Timeout: a tool call may legitimately run for minutes, so a request is
// bounded by its caller's context, with a response-header timeout as the
// backstop.
func gatewayHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: refuseRedirects, Transport: versionTransport{base: gatewayTransport}}
}

var clientVersion = "dev"

// SetVersion sets the zuano version reported in the User-Agent and
// Zuano-Client-Version headers of every gateway request.
func SetVersion(v string) {
	if v != "" {
		clientVersion = v
	}
}

func userAgent() string {
	return brand.Name + "/" + clientVersion + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// versionTransport adds the zuano version headers and a correlation id to
// every request. These are the only client details zuano sends.
type versionTransport struct{ base http.RoundTripper }

func (t versionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", userAgent())
	r.Header.Set(brand.Product+"-Client-Version", clientVersion)
	if r.Header.Get(RequestIDHeader) == "" {
		r.Header.Set(RequestIDHeader, NewCorrelationID())
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(r)
	if err == nil {
		recordRequestID(r, resp)
	}
	return resp, err
}

// New creates a gateway client with a fixed bearer token and NO refresh
// capability. Use Resolve for the auto-refreshing session client every CLI
// command should prefer.
func New(gatewayURL, accessToken string) *Client {
	return &Client{
		GatewayURL:  strings.TrimRight(gatewayURL, "/"),
		AccessToken: accessToken,
		httpClient:  gatewayHTTPClient(),
		profile:     keychain.ProfileHuman,
		store:       keychainSessionStore{profile: keychain.ProfileHuman},
		locker:      flockSessionLocker{},
	}
}

// checkAPIKeyOrigin keeps ZUANO_API_KEY from reaching a gateway that a flag
// or environment variable redirected the CLI to. A local development gateway
// is the one origin that needs no binding.
func checkAPIKeyOrigin(origin string) error {
	bound, err := config.APIKeyOrigin()
	if err != nil {
		return err
	}
	switch {
	case bound == origin:
		return nil
	case bound == "" && config.IsLoopbackOrigin(origin):
		return nil
	case bound == "":
		return fmt.Errorf("refusing to send ZUANO_API_KEY to %s: the key is not bound to a gateway. Set ZUANO_API_KEY_ORIGIN=%s, or save the gateway in the config file", origin, origin)
	}
	return fmt.Errorf("refusing to send ZUANO_API_KEY to %s: the key is bound to %s (ZUANO_API_KEY_ORIGIN or the saved gateway). Change ZUANO_API_KEY_ORIGIN deliberately to use another gateway", origin, bound)
}

// Resolve builds an authenticated Client for gateway calls.
func Resolve(gatewayURL string, opts ...ClientOption) (*Client, error) {
	origin, err := config.NormalizeGatewayURL(gatewayURL)
	if err != nil {
		return nil, err
	}

	profile := ActiveProfile()
	if apiKey := os.Getenv("ZUANO_API_KEY"); apiKey != "" {
		if err := checkAPIKeyOrigin(origin); err != nil {
			return nil, err
		}
		c := New(origin, apiKey)
		c.profile = profile
		c.stepUp = defaultStepUp
		for _, opt := range opts {
			opt(c)
		}
		return c, nil
	}

	agent := profile == keychain.ProfileAgent
	var email string
	if agent {
		email, err = config.ResolveAgentEmail()
	} else {
		email, err = config.ResolveEmail()
	}
	if err != nil {
		if agent {
			return nil, ErrAgentNotLoggedIn
		}
		return nil, ErrNotLoggedIn
	}

	tokenData, err := keychain.ReadProfileToken(profile, origin, email)
	if errors.Is(err, keychain.ErrNotFound) {
		if agent {
			return nil, ErrAgentNotLoggedIn
		}
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		var unavailable *keychain.UnavailableError
		switch {
		case errors.As(err, &unavailable):
			return nil, fmt.Errorf("keychain unavailable: %v. Use ZUANO_API_KEY for headless use", unavailable.Err)
		default:
			return nil, fmt.Errorf("%w; run `%s` to replace the stored session", err, brand.Cmd("login"))
		}
	}

	c := New(origin, tokenData.AccessToken)
	c.profile = profile
	c.stepUp = defaultStepUp
	if tokenData.DPoPKey != "" {
		key, err := dpop.Load(tokenData.DPoPKey)
		if err != nil {
			return nil, fmt.Errorf("this session is bound to a device key that cannot be used (%v); run `%s` to sign in again", err, brand.Cmd("login"))
		}
		c.proofs, c.dpopKey = dpop.NewSigner(key), tokenData.DPoPKey
	}
	c.store = keychainSessionStore{profile: profile}
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

// Profile returns the keychain profile this Client was resolved for.
func (c *Client) Profile() keychain.Profile { return c.profile }

// lockAccount names the refresh lock so the human and agent sessions of one
// account never wait on each other.
func (c *Client) lockAccount() string {
	if c.profile == keychain.ProfileAgent {
		return "agent:" + c.email
	}
	return c.email
}

// Email returns the identity this Client was resolved for, or "" for a
// bearer-only Client (New / ZUANO_API_KEY) that has no CLI session.
func (c *Client) Email() string {
	return c.email
}

// GatewayOrigin returns the normalized gateway identity this Client's
// keychain-backed OAuth session (if any) is bound to, or "" for a
// bearer-only Client (New / ZUANO_API_KEY) that has no CLI session.
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
	unlock, err := c.locker.Lock(ctx, c.gatewayOrigin, c.lockAccount())
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
	//
	// A store that cannot be read fails the refresh: spending a token that
	// may already be spent would revoke the family, and a missing entry means
	// the session was removed (logged out) and must not be revived.
	current, readErr := c.store.Read(c.gatewayOrigin, c.email)
	if readErr != nil {
		return fmt.Errorf("read the stored session before refreshing it (not refreshing, to avoid reusing a spent token): %w", readErr)
	}
	if current.RefreshToken != "" && current.RefreshToken != c.refreshToken {
		c.AccessToken = current.AccessToken
		c.refreshToken = current.RefreshToken
		if current.ExpiresAt > 0 {
			c.expiresAt = time.Unix(current.ExpiresAt, 0)
		}
		return nil
	}

	result, err := refreshOAuthToken(ctx, c.GatewayURL, c.refreshToken, c.proofs)
	if err != nil {
		return err
	}

	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	// The gateway has already invalidated the old refresh token, so the new
	// pair is the only working one: keep it in memory whatever the keychain says.
	c.AccessToken = result.AccessToken
	c.refreshToken = result.RefreshToken
	c.expiresAt = expiresAt

	if err := c.store.Write(c.gatewayOrigin, c.email, keychain.TokenData{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    expiresIn,
		ExpiresAt:    expiresAt.Unix(),
		DPoPKey:      c.dpopKey,
	}); err != nil {
		fmt.Fprintf(persistWarning, "%s: WARNING: the session was refreshed but could not be saved to the OS keychain (%v). This process keeps working, but the next %s command will find a revoked token and ask you to run `%s` again.\n", brand.Name, err, brand.Name, brand.Cmd("login"))
	}
	return nil
}

// persistWarning receives the warning printed when a rotated session cannot
// be saved.
var persistWarning io.Writer = os.Stderr

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

// StepUpChallenge is a confirmation the gateway wants a person to give
// before it runs an action.
type StepUpChallenge struct {
	ID  string
	URL string
	// Gateway is the origin the challenge came from; the handler opens URL
	// only when it is on that origin.
	Gateway string
}

// StepUpHandler gets a person to complete a StepUpChallenge and returns the
// id to send in the X-Step-Up header of the retried request. It may use the
// client to poll the gateway for the outcome.
type StepUpHandler func(ctx context.Context, challenge StepUpChallenge, c *Client) (string, error)

var defaultStepUp StepUpHandler

// SetDefaultStepUpHandler sets the handler every client built by Resolve
// starts with.
func SetDefaultStepUpHandler(h StepUpHandler) { defaultStepUp = h }

// SetStepUpHandler makes Do complete a step-up and retry once when the
// gateway answers 401 step_up_required. Without a handler the refusal is
// returned as a *StepUpRequiredError.
func (c *Client) SetStepUpHandler(h StepUpHandler) { c.stepUp = h }

// ProofSigner builds the DPoP proof (RFC 9449) that binds one request to the
// device key the session was issued to.
type ProofSigner interface {
	Proof(method, rawURL, accessToken string) (string, error)
}

// SetProofSigner makes Do authorize requests with a DPoP proof instead of a
// bare bearer token.
func (c *Client) SetProofSigner(p ProofSigner) { c.proofs = p }

func (c *Client) authorize(req *http.Request, token string) error {
	if c.proofs == nil {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	proof, err := c.proofs.Proof(req.Method, req.URL.String(), token)
	if err != nil {
		return fmt.Errorf("sign DPoP proof: %w", err)
	}
	req.Header.Set("Authorization", "DPoP "+token)
	req.Header.Set("DPoP", proof)
	return nil
}

// Do performs an authenticated HTTP request. A refusal by the gateway's
// billing gate, a quota or a step-up requirement comes back as a typed error
// (EntitlementError, QuotaExceededError, StepUpRequiredError) instead of a
// response, so every command reports it the same way.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.doAuthorized(req)
	if err != nil {
		return resp, err
	}
	refusal := refusalOf(resp)
	if refusal == nil {
		return resp, nil
	}
	_ = resp.Body.Close()
	var challenge *StepUpRequiredError
	if !errors.As(refusal, &challenge) || c.stepUp == nil {
		return nil, refusal
	}
	id, err := c.stepUp(req.Context(), StepUpChallenge{ID: challenge.ID, URL: challenge.URL, Gateway: c.GatewayURL}, c)
	if err != nil {
		return nil, err
	}
	retry, err := cloneRequest(req)
	if err != nil {
		return nil, err
	}
	retry.Header.Set("X-Step-Up", id)
	resp, err = c.doAuthorized(retry)
	if err != nil {
		return resp, err
	}
	return classify(resp)
}

func (c *Client) doAuthorized(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	c.maybeProactiveRefresh(ctx)
	if err := c.authorize(req, c.bearerSnapshot()); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	c.mu.Lock()
	refreshable := c.canRefreshLocked()
	c.mu.Unlock()
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !refreshable {
		return resp, err
	}
	// Neither a step-up nor a session the gateway rejects outright is an
	// expired token: refreshing would only repeat the refusal.
	switch refusalOf(resp).(type) {
	case *StepUpRequiredError, *ReauthRequiredError:
		return resp, nil
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
	if err := c.authorize(retryReq, retryToken); err != nil {
		return nil, err
	}
	return c.httpClient.Do(retryReq)
}

// cancelOnClose releases a request's deadline timer once the body is closed:
// the deadline has to outlive the call that returns the response.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

func withDefaultDeadline(do func(context.Context) (*http.Response, error)) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	resp, err := do(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

// Post is PostContext bounded by defaultRequestTimeout, for callers that have
// no context of their own. Prefer PostContext.
func (c *Client) Post(path, contentType string, body io.Reader) (*http.Response, error) {
	return withDefaultDeadline(func(ctx context.Context) (*http.Response, error) {
		return c.PostContext(ctx, path, contentType, body)
	})
}

// Get is GetContext bounded by defaultRequestTimeout. Prefer GetContext.
func (c *Client) Get(path string) (*http.Response, error) {
	return withDefaultDeadline(func(ctx context.Context) (*http.Response, error) {
		return c.GetContext(ctx, path)
	})
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
