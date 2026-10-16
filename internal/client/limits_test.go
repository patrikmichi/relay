package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecodeJSONRefusesOversizedBodies(t *testing.T) {
	var v struct{ A string }
	if err := DecodeJSON(strings.NewReader(`{"A":"x"}`), &v); err != nil || v.A != "x" {
		t.Fatalf("small body: %v %+v", err, v)
	}
	big := `{"A":"` + strings.Repeat("x", MaxJSONBytes) + `"}`
	if err := DecodeJSON(strings.NewReader(big), &v); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized body = %v, want a limit error", err)
	}
}

func TestGatewayTransportHasAResponseHeaderTimeout(t *testing.T) {
	tr, ok := gatewayHTTPClient().Transport.(versionTransport)
	if !ok {
		t.Fatalf("unexpected transport %T", gatewayHTTPClient().Transport)
	}
	base, ok := tr.base.(*http.Transport)
	if !ok || base.ResponseHeaderTimeout <= 0 {
		t.Fatalf("gateway transport has no response-header timeout: %#v", tr.base)
	}
}

func TestGetAndPostCarryADeadline(t *testing.T) {
	old := defaultRequestTimeout
	defaultRequestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { defaultRequestTimeout = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")

	for name, call := range map[string]func() (*http.Response, error){
		"Get":  func() (*http.Response, error) { return c.Get("/hang") },
		"Post": func() (*http.Response, error) { return c.Post("/hang", "text/plain", strings.NewReader("x")) },
	} {
		start := time.Now()
		resp, err := call()
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("%s did not time out", name)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("%s took %s to give up", name, time.Since(start))
		}
		var netErr interface{ Timeout() bool }
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Errorf("%s error = %v, want a timeout", name, err)
		}
	}
}

func TestGetDeadlineSurvivesUntilTheBodyIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	resp, err := New(srv.URL, "tok").Get("/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 2)
	if n, err := resp.Body.Read(buf); n != 2 && err != nil {
		t.Fatalf("body read after Get returned: %d %v", n, err)
	}
}
