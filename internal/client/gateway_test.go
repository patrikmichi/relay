package client_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/client"
)

// TestNew verifies that New sets GatewayURL and AccessToken, and strips trailing slashes.
func TestNew(t *testing.T) {
	c := client.New("https://example.com/", "tok123")
	if c.GatewayURL != "https://example.com" {
		t.Errorf("GatewayURL trailing slash not stripped: got %q", c.GatewayURL)
	}
	if c.AccessToken != "tok123" {
		t.Errorf("AccessToken mismatch: got %q", c.AccessToken)
	}
}

// TestDo_BearerHeader verifies that every request carries the correct Authorization header.
func TestDo_BearerHeader(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "my-token")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	want := "Bearer my-token"
	if capturedAuth != want {
		t.Errorf("Authorization header: got %q, want %q", capturedAuth, want)
	}
}

// TestGet_URLConstruction verifies that Get appends the path to the base URL.
func TestGet_URLConstruction(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok")
	resp, err := c.Get("/api/test/path")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if capturedPath != "/api/test/path" {
		t.Errorf("path mismatch: got %q, want %q", capturedPath, "/api/test/path")
	}
}

// TestPost_JSONBody verifies that Post sends the correct Content-Type and body.
func TestPost_JSONBody(t *testing.T) {
	type reqBody struct {
		Tool string `json:"tool"`
	}
	var gotBody reqBody
	var gotContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok")
	payload := `{"tool":"list_projects"}`
	resp, err := c.Post("/api/clockify/call", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotContentType != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", gotContentType)
	}
	if gotBody.Tool != "list_projects" {
		t.Errorf("body tool: got %q, want list_projects", gotBody.Tool)
	}
}

// TestDo_ReturnsError verifies that network errors are returned as errors.
func TestDo_ReturnsError(t *testing.T) {
	c := client.New("http://127.0.0.1:0", "tok") // no server at port 0
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:0/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(req)
	if err == nil {
		t.Error("expected error for unreachable server, got nil")
	}
}
