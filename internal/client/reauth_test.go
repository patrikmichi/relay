package client_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/keychain"
)

func TestReauthRequiredIsReportedWithoutARefreshAttempt(t *testing.T) {
	withTempHome(t)
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_EMAIL", "")
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/refresh" {
			refreshes.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"reauth_required"}`)
	}))
	t.Cleanup(srv.Close)
	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{
		AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get("/api/anything")
	var reauth *client.ReauthRequiredError
	if !errors.As(err, &reauth) || !strings.Contains(err.Error(), "relay login") {
		t.Fatalf("err = %v, want a ReauthRequiredError that points at relay login", err)
	}
	if refreshes.Load() != 0 {
		t.Errorf("refreshed %d times: the gateway rejected the session itself", refreshes.Load())
	}
}

func TestAnExpiredTokenIsStillRefreshedOnce(t *testing.T) {
	withTempHome(t)
	t.Setenv("ZUANO_API_KEY", "")
	t.Setenv("ZUANO_EMAIL", "")
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/cli/refresh":
			refreshes.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"access-2","refresh_token":"refresh-2","token_type":"Bearer","expires_in":3600}`)
		case r.Header.Get("Authorization") == "Bearer access-2":
			_, _ = io.WriteString(w, "ok")
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
		}
	}))
	t.Cleanup(srv.Close)
	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{
		AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("/api/anything")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if refreshes.Load() != 1 || resp.StatusCode != http.StatusOK {
		t.Errorf("refreshes %d, status %d", refreshes.Load(), resp.StatusCode)
	}
}
