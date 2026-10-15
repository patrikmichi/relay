package client_test

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

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/keychain"
)

type memoryStore struct {
	mu       sync.Mutex
	data     keychain.TokenData
	writeErr error
}

func (s *memoryStore) Read(_, _ string) (keychain.TokenData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data, nil
}

func (s *memoryStore) Write(_, _ string, d keychain.TokenData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.data = d
	return nil
}

func (s *memoryStore) snapshot() keychain.TokenData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

type failingLocker struct{}

func (failingLocker) Lock(context.Context, string, string) (client.SessionUnlocker, error) {
	return nil, errors.New("lock directory unwritable")
}

type countingGateway struct {
	srv         *httptest.Server
	refreshes   atomic.Int32
	requests    atomic.Int32
	refreshBody string
}

// newExpiredSessionGateway serves 401 on /api/test until the rotated access
// token is presented, and a fixed body on /api/cli/refresh.
func newExpiredSessionGateway(t *testing.T, refreshBody string) *countingGateway {
	t.Helper()
	g := &countingGateway{refreshBody: refreshBody}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/refresh", func(w http.ResponseWriter, r *http.Request) {
		g.refreshes.Add(1)
		_, _ = io.WriteString(w, g.refreshBody)
	})
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer rotated-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func resolveWithSession(t *testing.T, gatewayURL string, opts ...client.ClientOption) *client.Client {
	t.Helper()
	keyring.MockInit()
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	seedSession(t, gatewayURL, "user@example.com", keychain.TokenData{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	c, err := client.Resolve(gatewayURL, opts...)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return c
}

const rotatedPair = `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`

func TestDo_RefreshLockFailureReports401WithoutSpendingRefreshToken(t *testing.T) {
	g := newExpiredSessionGateway(t, rotatedPair)
	c := resolveWithSession(t, g.srv.URL, client.WithSessionLocker(failingLocker{}))

	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if n := g.refreshes.Load(); n != 0 {
		t.Errorf("refresh token spent %d times without holding the lock", n)
	}
}

func TestDo_PersistFailureKeepsCurrentTokenAndDoesNotRetry(t *testing.T) {
	g := newExpiredSessionGateway(t, rotatedPair)
	store := &memoryStore{
		data:     keychain.TokenData{AccessToken: "stale-access", RefreshToken: "stale-refresh"},
		writeErr: errors.New("keychain locked"),
	}
	c := resolveWithSession(t, g.srv.URL, client.WithSessionStore(store))

	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if g.refreshes.Load() != 1 || g.requests.Load() != 1 {
		t.Errorf("refreshes=%d requests=%d, want 1 and 1", g.refreshes.Load(), g.requests.Load())
	}
	if c.AccessToken != "stale-access" {
		t.Errorf("unpersisted token adopted: %q", c.AccessToken)
	}
}

func TestDo_UnreplayableBodyRefreshesButDoesNotRetry(t *testing.T) {
	g := newExpiredSessionGateway(t, rotatedPair)
	store := &memoryStore{data: keychain.TokenData{AccessToken: "stale-access", RefreshToken: "stale-refresh"}}
	c := resolveWithSession(t, g.srv.URL, client.WithSessionStore(store))

	body := io.MultiReader(strings.NewReader("payload"))
	resp, err := c.PostContext(context.Background(), "/api/test", "text/plain", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if g.requests.Load() != 1 {
		t.Errorf("request sent %d times, want 1 (body cannot be replayed)", g.requests.Load())
	}
	if got := store.snapshot(); got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" {
		t.Errorf("rotated pair not persisted for the next command: %+v", got)
	}
}

func TestDo_RefreshWithoutExpiryDefaultsToOneHour(t *testing.T) {
	g := newExpiredSessionGateway(t, rotatedPair)
	store := &memoryStore{data: keychain.TokenData{AccessToken: "stale-access", RefreshToken: "stale-refresh"}}
	c := resolveWithSession(t, g.srv.URL, client.WithSessionStore(store))

	before := time.Now()
	resp, err := c.Get("/api/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 after refresh", resp.StatusCode)
	}
	got := store.snapshot()
	if got.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn %d, want 3600", got.ExpiresIn)
	}
	if exp := time.Unix(got.ExpiresAt, 0); exp.Before(before.Add(59*time.Minute)) || exp.After(time.Now().Add(61*time.Minute)) {
		t.Errorf("ExpiresAt %s not about one hour out", exp)
	}
}

func TestResolve_RejectsInvalidGatewayURL(t *testing.T) {
	withTempHome(t)
	if _, err := client.Resolve("ftp://gw.example.com"); err == nil {
		t.Fatal("expected an unsupported scheme to be rejected")
	}
}

func TestResolve_GatewayOrigin(t *testing.T) {
	g := newExpiredSessionGateway(t, rotatedPair)
	resolveWithSession(t, g.srv.URL)
	c, err := client.Resolve(g.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if c.GatewayOrigin() != g.srv.URL || c.Email() != "user@example.com" {
		t.Errorf("origin=%q email=%q", c.GatewayOrigin(), c.Email())
	}

	t.Setenv("GATEWAY_API_KEY", strings.Join([]string{"static", "key"}, "-"))
	keyed, err := client.Resolve(g.srv.URL, client.WithSessionLocker(failingLocker{}))
	if err != nil {
		t.Fatal(err)
	}
	if keyed.GatewayOrigin() != "" || keyed.Email() != "" {
		t.Errorf("API-key client must carry no session binding: origin=%q email=%q", keyed.GatewayOrigin(), keyed.Email())
	}
}

func TestResolve_CorruptBoundSessionPointsAtLogin(t *testing.T) {
	keyring.MockInit()
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "user@example.com")
	const origin = "https://gw.example.com"
	if err := keyring.Set("relay-cli", "oauth-refresh-token:"+origin+":user@example.com", "{not json"); err != nil {
		t.Fatal(err)
	}
	_, err := client.Resolve(origin)
	if err == nil || !strings.Contains(err.Error(), "relay login") {
		t.Fatalf("got %v, want an error pointing at relay login", err)
	}
}

func TestRequestCreationErrors(t *testing.T) {
	c := client.New("https://gw.example.com", "bearer")
	if _, err := c.GetContext(context.Background(), "/bad path\x7f"); err == nil || !strings.Contains(err.Error(), "create request") {
		t.Errorf("GetContext: got %v, want a create-request error", err)
	}
	if _, err := c.PostContext(context.Background(), "/bad path\x7f", "text/plain", nil); err == nil || !strings.Contains(err.Error(), "create request") {
		t.Errorf("PostContext: got %v, want a create-request error", err)
	}
}
