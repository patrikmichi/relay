package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/keychain"
)

func deviceGateway(t *testing.T, codeStatus int, codeBody string, token http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("%s: method %s, want POST", r.URL.Path, r.Method)
		}
		switch r.URL.Path {
		case "/api/cli/device/code":
			w.WriteHeader(codeStatus)
			_, _ = w.Write([]byte(codeBody))
		case "/api/cli/device/token":
			if token == nil {
				t.Errorf("unexpected token poll")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			token(w, r)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const validDeviceCode = `{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_uri":"https://gw.example.com/device","interval":1}`

func TestDeviceLogin_PromptsAndStoresSession(t *testing.T) {
	withTempHome(t)
	var polledCode string
	srv := deviceGateway(t, http.StatusOK, validDeviceCode, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		polledCode = body["device_code"]
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "access-d", "refresh_token": "refresh-d", "email": "user@example.com",
		})
	})

	var out bytes.Buffer
	result, err := DeviceLogin(context.Background(), srv.URL+"/", &out)
	if err != nil {
		t.Fatalf("DeviceLogin: %v", err)
	}
	if polledCode != "dev-1" {
		t.Errorf("polled device_code %q, want dev-1", polledCode)
	}
	if result.Email != "user@example.com" || result.AccessToken != "access-d" {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.ExpiresIn != 3600 {
		t.Errorf("missing expires_in must default to 3600, got %d", result.ExpiresIn)
	}
	for _, want := range []string{"https://gw.example.com/device", "ABCD-EFGH", "Waiting for authorization"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("prompt output missing %q:\n%s", want, out.String())
		}
	}
	if _, err := keychain.ReadToken(srv.URL, "user@example.com"); err != nil {
		t.Errorf("session not stored under the normalized origin: %v", err)
	}
}

func TestDeviceLogin_CodeRequestFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"error status carries description", http.StatusForbidden, `{"error_description":"device flow disabled"}`, "(403): device flow disabled"},
		{"redirect is not followed", http.StatusFound, ``, "(302)"},
		{"malformed body", http.StatusOK, `not json`, "decode device code response"},
		{"missing user code", http.StatusOK, `{"device_code":"d","verification_uri":"https://gw.example.com/device"}`, "missing required field"},
		{"oversized body", http.StatusOK, `"` + strings.Repeat("a", maxAuthResponseBytes) + `"`, "byte limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			srv := deviceGateway(t, tc.status, tc.body, nil)
			_, err := DeviceLogin(context.Background(), srv.URL, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestDeviceLogin_RejectsInsecureGatewayBeforeAnyRequest(t *testing.T) {
	withTempHome(t)
	if _, err := DeviceLogin(context.Background(), "http://gw.example.com", &bytes.Buffer{}); err == nil {
		t.Fatal("expected plain HTTP to a remote host to be rejected")
	}
}

func TestDeviceLogin_UnreachableGateway(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := DeviceLogin(context.Background(), url, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "request device code") {
		t.Fatalf("got %v, want a request-device-code error", err)
	}
}

func TestDeviceLogin_ExpiresInBoundsThePollLoop(t *testing.T) {
	withTempHome(t)
	var polls int32
	srv := deviceGateway(t, http.StatusOK,
		`{"device_code":"dev-1","user_code":"U","verification_uri":"https://gw.example.com/device","expires_in":1}`,
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&polls, 1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		})

	start := time.Now()
	_, err := DeviceLogin(context.Background(), srv.URL, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("poll loop ran %s, past the 1s device-code expiry", elapsed)
	}
	if atomic.LoadInt32(&polls) != 1 {
		t.Errorf("polled %d times, want 1 within the expiry window", polls)
	}
}

func TestPollDeviceToken_UnreachableEndpoint(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := pollDeviceToken(context.Background(), url, "dev-1", 1)
	if err == nil || !strings.Contains(err.Error(), "poll device token") {
		t.Fatalf("got %v, want a poll error", err)
	}
}

func TestPollDeviceToken_AlreadyCancelledContext(t *testing.T) {
	withTempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pollDeviceToken(ctx, "http://127.0.0.1:1", "dev-1", 1)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v, want a timeout", err)
	}
}

func TestPollDeviceToken_StalledRequestHonoursDeadline(t *testing.T) {
	withTempHome(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := pollDeviceToken(ctx, srv.URL, "dev-1", 1)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v, want a timeout", err)
	}
}

func TestPollDeviceToken_SuccessWithUndecodableBody(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":`))
	}))
	defer srv.Close()

	_, err := pollDeviceToken(context.Background(), srv.URL, "dev-1", 1)
	if err == nil || !strings.Contains(err.Error(), "decode device token response") {
		t.Fatalf("got %v, want a decode error", err)
	}
}

func TestPollDeviceToken_KeychainFailureAborts(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "access-1", "refresh_token": "refresh-1", "email": "user@example.com", "expires_in": 60,
		})
	}))
	defer srv.Close()
	keyring.MockInitWithError(errors.New("keychain locked"))
	defer keyring.MockInit()

	if result, err := pollDeviceToken(context.Background(), srv.URL, "dev-1", 1); err == nil {
		t.Fatalf("expected a keychain failure to abort, got %+v", result)
	}
}
