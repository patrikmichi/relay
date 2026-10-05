package auth

// Device-token responses must carry every required field; do not weaken
// these assertions.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"
)

// An HTTP 200 body of `{}` must not produce a "successful" LoginResult
// with every field empty.
func TestEmptyDeviceResponseRejected(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "synthetic", 5)
	if err == nil {
		t.Fatalf("expected an error for an empty HTTP 200 device-token response, got result=%+v", result)
	}
}

func TestPartialDeviceResponseRejected(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"email":"user@example.com"}`))
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "synthetic", 5)
	if err == nil {
		t.Fatalf("expected an error when access_token/refresh_token are missing, got result=%+v", result)
	}
}

func TestMalformedDeviceResponseRejected(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "synthetic", 5)
	if err == nil {
		t.Fatalf("expected an error for a non-JSON HTTP 200 device-token response, got result=%+v", result)
	}
}
