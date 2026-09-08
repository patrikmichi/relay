package client

// Regression tests derived from the 2026-09-07 deep review's reproduction
// probes (vault/reviews/relay/2026-09-07/evidence/client_probes_test.go).
// Those probes deliberately asserted the CURRENT (buggy) behavior; these
// tests invert the assertions to the SAFE behavior the finding demands.
// They are expected to FAIL against baseline 931aac4 and must turn green
// in the same commit/PR that lands each finding's fix — do not weaken
// these assertions to obtain a green baseline.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/keychain"
	"github.com/zalando/go-keyring"
)

// TestR01_CredentialNotSentToUnboundGateway reproduces R01: a session
// stored under an email-only keychain key has no gateway-origin binding,
// so Resolve() against an unrelated gateway B still attaches gateway A's
// bearer token. Safe behavior: gateway B must never receive a credential
// that was not issued for it.
func TestR01_CredentialNotSentToUnboundGateway(t *testing.T) {
	keyring.MockInit()
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "review@example.invalid")
	keychain.WriteToken("review@example.invalid", keychain.TokenData{
		AccessToken:  "gateway-A-access",
		RefreshToken: "gateway-A-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})

	var seen string
	gatewayB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		io.WriteString(w, "ok")
	}))
	defer gatewayB.Close()

	c, err := Resolve(gatewayB.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if seen == "Bearer gateway-A-access" {
		t.Fatalf("R01: gateway B received gateway A's credential (%q) — sessions must be gateway-origin bound", seen)
	}
}

// TestR02_RefreshRedirectDoesNotLeakBody reproduces R02: refreshOAuthToken
// follows a 307 redirect to another origin, forwarding the refresh-token
// POST body. Safe behavior: token exchange/refresh must refuse redirects.
func TestR02_RefreshRedirectDoesNotLeakBody(t *testing.T) {
	var received string
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
		io.WriteString(w, `{"access_token":"a","refresh_token":"r"}`)
	}))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()

	_, err := refreshOAuthToken(src.URL, "synthetic-private-refresh")
	if err == nil {
		t.Fatal("R02: expected refresh to reject the cross-origin redirect, got nil error")
	}
	if strings.Contains(received, "synthetic-private-refresh") {
		t.Fatalf("R02: refresh-token body leaked to redirect target: %q", received)
	}
}

// TestR23_RefreshRespectsContextDeadline reproduces R23: refresh ignores
// the caller's context, so a 10ms deadline is exceeded by ~120ms because
// refresh runs on the unbounded default client. Safe behavior: the
// deadline must bound the whole request lifecycle including refresh.
func TestR23_RefreshRespectsContextDeadline(t *testing.T) {
	keyring.MockInit()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/refresh" {
			time.Sleep(120 * time.Millisecond)
			io.WriteString(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":3600}`)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer s.Close()

	c := New(s.URL, "old")
	c.email = "review@example.invalid"
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
		t.Fatalf("R23: 10ms context deadline did not bound refresh; elapsed=%s err=%v", elapsed, err)
	}
}

// TestR24_ConcurrentClientsDoNotReuseStaleRefreshToken reproduces R24:
// two independently resolved clients both submit the same single-use
// refresh token because neither reloads keyring state under a shared
// lock. Safe behavior: at most one client may submit a given refresh
// token value.
func TestR24_ConcurrentClientsDoNotReuseStaleRefreshToken(t *testing.T) {
	keyring.MockInit()
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "review@example.invalid")
	keychain.WriteToken("review@example.invalid", keychain.TokenData{
		AccessToken:  "old",
		RefreshToken: "same-old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})

	var mu sync.Mutex
	var submitted []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/refresh" {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			submitted = append(submitted, string(raw))
			mu.Unlock()
			io.WriteString(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer s.Close()

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
		t.Fatalf("R24: single-use refresh token %q submitted %d times: %v", "same-old-refresh", reuseCount, submitted)
	}
}
