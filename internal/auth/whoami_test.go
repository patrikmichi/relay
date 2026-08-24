package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/client"
)

func TestWhoami_Success(t *testing.T) {
	withTempHome(t)

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"email":     "user@example.com",
			"googleSub": "sub-123",
			"services":  []string{"clockify", "n8n"},
			"issuedAt":  "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := client.New(srv.URL, "static-key")
	resp, err := Whoami(c, false)
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if resp.Email != "user@example.com" || resp.GoogleSub != "sub-123" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if len(resp.Services) != 2 {
		t.Errorf("Services: got %v", resp.Services)
	}
	if gotQuery != "" {
		t.Errorf("expected no query string when full=false, got %q", gotQuery)
	}
}

func TestWhoami_FullRequestsGroups(t *testing.T) {
	withTempHome(t)

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"email": "user@example.com", "groups": []string{"admins"},
		})
	}))
	defer srv.Close()

	c := client.New(srv.URL, "static-key")
	resp, err := Whoami(c, true)
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if gotQuery != "groups=true" {
		t.Errorf("expected groups=true query param, got %q", gotQuery)
	}
	if len(resp.Groups) != 1 || resp.Groups[0] != "admins" {
		t.Errorf("unexpected Groups: %v", resp.Groups)
	}
}

func TestWhoami_Unauthorized(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "static-key")
	_, err := Whoami(c, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "relay login") {
		t.Errorf("expected re-auth guidance in error, got: %v", err)
	}
}

func TestWhoami_OtherNonOK(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
	}))
	defer srv.Close()

	c := client.New(srv.URL, "static-key")
	_, err := Whoami(c, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected status + error body to surface, got: %v", err)
	}
}

func TestWhoami_DecodeError(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json{{{"))
	}))
	defer srv.Close()

	c := client.New(srv.URL, "static-key")
	_, err := Whoami(c, false)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode whoami response") {
		t.Errorf("expected decode error, got: %v", err)
	}
}
