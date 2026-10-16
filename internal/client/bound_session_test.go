package client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/dpop"
	"github.com/patrikmichi/relay/internal/keychain"
)

type seen struct {
	mu       sync.Mutex
	requests []map[string]string
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, map[string]string{"path": r.URL.Path, "auth": r.Header.Get("Authorization"), "dpop": r.Header.Get("DPoP")})
}

func TestBoundSessionSendsDPoPProofsAndKeepsItsKeyAcrossARefresh(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	key, err := dpop.Create(false)
	if err != nil {
		t.Fatal(err)
	}
	var s seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		switch {
		case r.URL.Path == "/api/cli/refresh":
			_, _ = io.WriteString(w, `{"access_token":"access-2","refresh_token":"refresh-2","token_type":"DPoP","expires_in":600}`)
		case r.Header.Get("Authorization") == "DPoP access-2":
			_, _ = io.WriteString(w, "ok")
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
		}
	}))
	t.Cleanup(srv.Close)
	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{
		AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: 600,
		ExpiresAt: time.Now().Add(time.Hour).Unix(), DPoPKey: key.Ref(),
	})

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("/api/x?q=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: the retry after the refresh was not accepted", resp.StatusCode)
	}

	for _, r := range s.requests {
		if r["dpop"] == "" {
			t.Errorf("%s went out without a DPoP proof: %v", r["path"], r)
		}
		if r["path"] != "/api/cli/refresh" && !strings.HasPrefix(r["auth"], "DPoP ") {
			t.Errorf("%s used the %q scheme, want DPoP", r["path"], r["auth"])
		}
	}
	stored, err := keychain.ReadToken(srv.URL, "user@example.com")
	if err != nil || stored.DPoPKey != key.Ref() || stored.RefreshToken != "refresh-2" {
		t.Errorf("stored session = %+v, %v: the refresh must keep the key reference", stored, err)
	}
}

func TestSessionWhoseKeyIsGoneAsksForANewLogin(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	seedSession(t, "https://gw.example.com", "user@example.com", keychain.TokenData{
		AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		DPoPKey: "file:00000000000000000000000000000000",
	})
	_, err := client.Resolve("https://gw.example.com")
	if err == nil || !strings.Contains(err.Error(), "device key") || !strings.Contains(err.Error(), "login") {
		t.Fatalf("err = %v", err)
	}
}

func TestShortLivedStrictTokensAreRefreshedAheadOfExpiry(t *testing.T) {
	withTempHome(t)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("RELAY_EMAIL", "")
	var refreshes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/refresh" {
			refreshes++
			_, _ = io.WriteString(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer access-2" {
			t.Errorf("request used %q, not the refreshed token", r.Header.Get("Authorization"))
		}
	}))
	t.Cleanup(srv.Close)
	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{
		AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: 600, ExpiresAt: time.Now().Add(10 * time.Second).Unix(),
	})
	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("/api/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want one before the request, with no failed request first", refreshes)
	}
}
