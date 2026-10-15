package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefreshOAuthToken_Failures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"description preferred", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"reuse detected"}`, "(400): reuse detected"},
		{"error code fallback", http.StatusUnauthorized, `{"error":"invalid_grant"}`, "(401): invalid_grant"},
		{"non-JSON error body", http.StatusBadGateway, `<html>`, "refresh failed (502)"},
		{"malformed success", http.StatusOK, `{"access_token":`, "decode refresh response"},
		{"missing refresh token", http.StatusOK, `{"access_token":"a"}`, "missing token fields"},
		{"oversized body", http.StatusOK, strings.Repeat("x", maxRefreshResponseBytes+1), "byte limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := refreshOAuthToken(context.Background(), srv.URL, "refresh")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRefreshOAuthToken_TransportAndRequestErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := refreshOAuthToken(context.Background(), url, "refresh"); err == nil || !strings.Contains(err.Error(), "POST ") {
		t.Errorf("got %v, want a transport error", err)
	}
	if _, err := refreshOAuthToken(context.Background(), "https://gw.example.com\x7f", "refresh"); err == nil || !strings.Contains(err.Error(), "create refresh request") {
		t.Errorf("got %v, want a request construction error", err)
	}
}

func TestCloneRequest_BodyHandling(t *testing.T) {
	plain, _ := http.NewRequest(http.MethodGet, "https://gw.example.com/x", nil)
	if _, err := cloneRequest(plain); err != nil {
		t.Errorf("bodiless request: %v", err)
	}

	replayable, _ := http.NewRequest(http.MethodPost, "https://gw.example.com/x", strings.NewReader("payload"))
	clone, err := cloneRequest(replayable)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(clone.Body); string(got) != "payload" {
		t.Errorf("cloned body %q, want payload", got)
	}

	streaming, _ := http.NewRequest(http.MethodPost, "https://gw.example.com/x", io.MultiReader(strings.NewReader("p")))
	if _, err := cloneRequest(streaming); err == nil {
		t.Error("expected an error for a body without GetBody")
	}

	broken, _ := http.NewRequest(http.MethodPost, "https://gw.example.com/x", strings.NewReader("p"))
	broken.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("gone") }
	if _, err := cloneRequest(broken); err == nil || !strings.Contains(err.Error(), "re-read request body") {
		t.Errorf("got %v, want a re-read error", err)
	}
}

func TestReadCapped_PropagatesReadError(t *testing.T) {
	if _, err := readCapped(failingReader{}, 10); err == nil {
		t.Fatal("expected the reader's error")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
