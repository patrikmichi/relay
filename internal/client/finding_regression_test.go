package client

// Named regression tests inverting the 2026-09-07 review's bug-confirming
// probes (evidence/client_probes_test.go) into the safe behavior each
// finding demands. Expected to fail until the fix lands in the same
// commit — do not weaken these assertions.

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

// R01: sessions are keyed by email only, with no gateway-origin binding.
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

// R02: refreshOAuthToken follows redirects, forwarding the refresh-token
// POST body to whatever origin the response redirects to.
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

// R23: refresh runs on the unbounded default client and ignores the
// caller's context, so a short deadline doesn't bound it.
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

// R24: independent clients each snapshot the keyring without a shared
// lock, so both submit the same single-use refresh token.
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
