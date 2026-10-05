package client

// Regression tests pinning session safety: gateway-bound credentials,
// redirect-free refresh, context deadlines, and single-use refresh tokens
// under concurrency. Do not weaken these assertions to obtain a green
// baseline.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/keychain"
	"github.com/zalando/go-keyring"
)

// TestCredentialNotSentToUnboundGateway: a session stored under an
// email-only keychain key has no gateway binding, so Resolve must not
// attach it to a gateway the user did not save in the config file.
func TestCredentialNotSentToUnboundGateway(t *testing.T) {
	keyring.MockInit()
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "review@example.invalid")
	if err := keyring.Set("relay-cli", "oauth-refresh-token:review@example.invalid",
		`{"access_token":"gateway-A-access","refresh_token":"gateway-A-refresh"}`); err != nil {
		t.Fatal(err)
	}

	gatewayB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("gateway B must never be dialed for a refused legacy session (Authorization: %q)", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "ok")
	}))
	defer gatewayB.Close()

	if _, err := Resolve(gatewayB.URL); !errors.Is(err, ErrSessionMigration) {
		t.Fatalf("err = %v, want ErrSessionMigration for an unbound legacy session", err)
	}
}

// TestSameEmailSessionsCoexistAcrossGateways verifies the positive half
// of gateway binding: two sessions for the same email but different gateways are
// stored independently, and resolving against one never surfaces the
// other's bearer token.
func TestSameEmailSessionsCoexistAcrossGateways(t *testing.T) {
	keyring.MockInit()
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "review@example.invalid")

	var seenB string
	gatewayB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenB = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer gatewayB.Close()

	if err := keychain.WriteToken("https://gateway-a.example.invalid", "review@example.invalid", keychain.TokenData{
		AccessToken: "gateway-A-access", RefreshToken: "gateway-A-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := keychain.WriteToken(gatewayB.URL, "review@example.invalid", keychain.TokenData{
		AccessToken: "gateway-B-access", RefreshToken: "gateway-B-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve(gatewayB.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if seenB != "Bearer gateway-B-access" {
		t.Fatalf("gateway B did not receive its own bound credential, got %q", seenB)
	}
}

// TestRefreshRedirectDoesNotLeakBody: refreshOAuthToken used to follow a
// 307 redirect to another origin, forwarding the refresh-token
// POST body. Safe behavior: token exchange/refresh must refuse redirects.
func TestRefreshRedirectDoesNotLeakBody(t *testing.T) {
	var received string
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
		_, _ = io.WriteString(w, `{"access_token":"a","refresh_token":"r"}`)
	}))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()

	_, err := refreshOAuthToken(context.Background(), src.URL, "synthetic-private-refresh")
	if err == nil {
		t.Fatal("expected refresh to reject the cross-origin redirect, got nil error")
	}
	if strings.Contains(received, "synthetic-private-refresh") {
		t.Fatalf("refresh-token body leaked to redirect target: %q", received)
	}
}

// TestRefreshRespectsContextDeadline: refresh used to ignore the caller's
// context, so a 10ms deadline was exceeded by ~120ms because refresh ran
// on the unbounded default client. Safe behavior: the
// deadline must bound the whole request lifecycle including refresh.
func TestRefreshRespectsContextDeadline(t *testing.T) {
	keyring.MockInit()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/refresh" {
			time.Sleep(120 * time.Millisecond)
			_, _ = io.WriteString(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":3600}`)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer s.Close()

	c := New(s.URL, "old")
	c.email = "review@example.invalid"
	c.gatewayOrigin = s.URL
	c.refreshToken = "old-refresh"
	c.expiresAt = time.Now().Add(-time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, err := c.GetContext(ctx, "/test")
	if resp != nil {
		resp.Body.Close()
	}
	elapsed := time.Since(start)
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("10ms context deadline did not bound refresh; elapsed=%s err=%v", elapsed, err)
	}
}

// TestConcurrentClientsDoNotReuseStaleRefreshToken: two independently
// resolved clients used to both submit the same single-use refresh token
// because neither reloaded keyring state under a shared lock. Safe behavior:
// at most one client may submit a given refresh token value.
func TestConcurrentClientsDoNotReuseStaleRefreshToken(t *testing.T) {
	keyring.MockInit()
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "review@example.invalid")

	var mu sync.Mutex
	var submitted []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/refresh" {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			submitted = append(submitted, string(raw))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer s.Close()

	if err := keychain.WriteToken(s.URL, "review@example.invalid", keychain.TokenData{
		AccessToken:  "old",
		RefreshToken: "same-old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	a, err := Resolve(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Resolve(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Client{a, b} {
		r, err := c.Get("/test")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}

	reuseCount := 0
	for _, raw := range submitted {
		if strings.Contains(raw, "same-old-refresh") {
			reuseCount++
		}
	}
	if reuseCount > 1 {
		t.Fatalf("single-use refresh token %q submitted %d times: %v", "same-old-refresh", reuseCount, submitted)
	}
}

// fakeSessionStore is an in-memory SessionStore used to deterministically
// simulate "another process already rotated the token" without touching a
// real (or mocked) keychain — it lets a test observe exactly what
// refreshLocked's generation-comparison branch does. Guarded by its own
// mutex so concurrent Clients sharing one instance (simulating a shared
// keychain across processes) don't race on it.
type fakeSessionStore struct {
	mu   sync.Mutex
	data keychain.TokenData
}

func (f *fakeSessionStore) Read(_, _ string) (keychain.TokenData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data, nil
}

func (f *fakeSessionStore) Write(_, _ string, d keychain.TokenData) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = d
	return nil
}

// TestSharedClientConcurrentRequestsRefreshOnce runs under
// `go test -race`: many goroutines share ONE *Client whose token is already
// expired, so every goroutine's first request takes the proactive-refresh
// path. Safe behavior — guarded by the single c.mu synchronization policy —
// is that every read/write of AccessToken/refreshToken/expiresAt is race
// free AND exactly one goroutine actually performs the HTTP refresh; every
// other goroutine's double-checked read (after acquiring c.mu) must observe
// the already-rotated expiry and skip it.
func TestSharedClientConcurrentRequestsRefreshOnce(t *testing.T) {
	keyring.MockInit()
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "review@example.invalid")

	var refreshHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshHits, 1)
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, `{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":3600}`)
	})
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if err := keychain.WriteToken(srv.URL, "review@example.invalid", keychain.TokenData{
		AccessToken:  "old",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get("/test")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Fatalf("expected exactly 1 refresh across 20 goroutines sharing one Client, got %d", got)
	}
}

// TestRealLockAdoptsWinnerAcrossTwoClients uses the REAL cross-process
// file lock (internal/client/sessionlock, not a mock of it) to prove the
// generation-aware adoption path in refreshLocked: two independent Clients
// (simulating two separate `relay` processes) share one gatewayOrigin/email
// identity and one backing store. The first to acquire the real lock
// performs the actual HTTP refresh and persists the winning result; the
// second — released only once the first is done — must see the store's
// refresh token no longer matches its own stale copy and adopt the winner's
// result instead of replaying its now-dead refresh token against the
// server.
func TestRealLockAdoptsWinnerAcrossTwoClients(t *testing.T) {
	var refreshHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshHits, 1)
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(w, `{"access_token":"winner-access","refresh_token":"winner-refresh","expires_in":3600}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store := &fakeSessionStore{data: keychain.TokenData{
		AccessToken: "shared-access", RefreshToken: "shared-refresh",
	}}
	// A single real flockSessionLocker{} keyed by (gatewayOrigin, email) —
	// both Clients below share the same identity, so they contend for the
	// exact same on-disk lock file.
	const gatewayOrigin, email = "https://gw-r24-real-lock.example.invalid", "user@example.invalid"

	newClient := func() *Client {
		return &Client{
			GatewayURL:    srv.URL,
			AccessToken:   "shared-access",
			httpClient:    NoRedirectHTTPClient(),
			email:         email,
			gatewayOrigin: gatewayOrigin,
			refreshToken:  "shared-refresh",
			expiresAt:     time.Now().Add(time.Hour),
			store:         store,
			locker:        flockSessionLocker{},
		}
	}
	c1, c2 := newClient(), newClient()

	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		close(started)
		c1.mu.Lock()
		if err := c1.refreshLocked(context.Background()); err != nil {
			t.Errorf("c1 refreshLocked: %v", err)
		}
		c1.mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		<-started
		time.Sleep(5 * time.Millisecond) // bias c1 to grab the real lock first
		c2.mu.Lock()
		if err := c2.refreshLocked(context.Background()); err != nil {
			t.Errorf("c2 refreshLocked: %v", err)
		}
		c2.mu.Unlock()
	}()
	wg.Wait()

	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Fatalf("expected exactly 1 HTTP refresh (the loser must adopt, not replay), got %d", got)
	}
	if c1.AccessToken != "winner-access" || c2.AccessToken != "winner-access" {
		t.Fatalf("both clients must converge on the winning token, got c1=%q c2=%q", c1.AccessToken, c2.AccessToken)
	}
	if c1.refreshToken != "winner-refresh" || c2.refreshToken != "winner-refresh" {
		t.Fatalf("both clients must converge on the winning refresh token, got c1=%q c2=%q", c1.refreshToken, c2.refreshToken)
	}
}
