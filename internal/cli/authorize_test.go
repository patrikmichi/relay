package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthorizeVerifiesGrantBeforeSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "test-token")
	previous := authorizationPollInterval
	authorizationPollInterval = time.Millisecond
	defer func() { authorizationPollInterval = previous }()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != serviceAuthorizationPath {
			t.Errorf("unexpected path")
		}
		state := "pending"
		linked := false
		missing := []string{"read"}
		if r.Method == "POST" {
			var request serviceAuthorizationRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if request.Service != "sample" || request.Account != "work" || strings.Join(request.Scopes, ",") != "read" {
				t.Errorf("incorrect requested grant: %+v", request)
			}
		} else {
			polls++
			if r.URL.Query().Get("service") != "sample" || r.URL.Query().Get("account") != "work" || r.URL.Query().Get("scope") != "read" {
				t.Errorf("incorrect verification query")
			}
			state = "authorized"
			linked = true
			missing = []string{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"service": "sample", "account": "work", "scopes": []string{"read"}, "state": state, "accountLinked": linked, "missingScopes": missing, "authorizationPath": "/settings/connections", "scopeKind": "gateway_tool"})
	}))
	defer srv.Close()
	cmd := AuthorizeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"sample", "--scope", "read", "--account", "work", "--gateway-url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if polls != 1 || !strings.Contains(out.String(), "Authorized gateway tools for sample") {
		t.Fatalf("missing grant verification: %d %s", polls, out.String())
	}
}

func TestAuthorizeRejectsFalseConfirmationAndDenial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"generic_login", 200, `{"email":"user@test"}`},
		{"wrong_service", 200, `{"service":"other","scopes":["read"],"state":"authorized","scopeKind":"gateway_tool","accountLinked":true}`},
		{"wrong_scope", 200, `{"service":"sample","scopes":["write"],"state":"authorized","scopeKind":"gateway_tool","accountLinked":true}`},
		{"unlinked", 200, `{"service":"sample","scopes":["read"],"state":"authorized","scopeKind":"gateway_tool","accountLinked":false}`},
		{"denied", 403, `synthetic-secret`},
		{"expired", 200, `{"service":"sample","scopes":["read"],"state":"expired","scopeKind":"gateway_tool"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GATEWAY_API_KEY", "test-token")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			cmd := AuthorizeCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"sample", "--scope", "read", "--gateway-url", srv.URL})
			err := cmd.Execute()
			if err == nil || strings.Contains(out.String(), "Authorized gateway") || strings.Contains(out.String(), "synthetic-secret") {
				t.Fatalf("unsafe success: %v %s", err, out.String())
			}
		})
	}
}

func authorizeServer(t *testing.T, pollStatuses []int) (*httptest.Server, *int) {
	t.Helper()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, linked, missing := "pending", false, []string{"read"}
		if r.Method == http.MethodGet {
			i := polls
			polls++
			if i < len(pollStatuses) && pollStatuses[i] != 200 {
				w.WriteHeader(pollStatuses[i])
				_, _ = w.Write([]byte(`{"error":"try later"}`))
				return
			}
			state, linked, missing = "authorized", true, []string{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"service": "sample", "account": nil, "scopes": []string{"read"}, "state": state, "accountLinked": linked, "missingScopes": missing, "authorizationPath": "/admin", "scopeKind": "gateway_tool"})
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func runAuthorize(t *testing.T, srv *httptest.Server) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "test-token")
	previous := authorizationPollInterval
	authorizationPollInterval = time.Millisecond
	t.Cleanup(func() { authorizationPollInterval = previous })
	cmd := AuthorizeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"sample", "--scope", "read", "--gateway-url", srv.URL})
	err := cmd.Execute()
	return out.String(), err
}

func TestAuthorizeRetriesTransientPollFailures(t *testing.T) {
	srv, polls := authorizeServer(t, []int{503, 429, 502})
	out, err := runAuthorize(t, srv)
	if err != nil || !strings.Contains(out, "Authorized gateway tools") || *polls != 4 {
		t.Fatalf("err = %v, polls = %d\n%s", err, *polls, out)
	}

	srv, polls = authorizeServer(t, []int{503, 503, 503, 503, 503, 503, 503})
	if _, err = runAuthorize(t, srv); err == nil || !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("endless 503s = %v, want the transient error after the retries", err)
	}
	if *polls != maxAuthorizationFailures+1 {
		t.Errorf("polled %d times, want %d", *polls, maxAuthorizationFailures+1)
	}
}

func TestAuthorizeMapsRefusalsAndDoesNotRetryThem(t *testing.T) {
	for status, want := range map[int]string{401: "relay login", 403: "denied (HTTP 403)", 404: "does not support", 422: "rejected (HTTP 422)"} {
		srv, polls := authorizeServer(t, []int{status})
		_, err := runAuthorize(t, srv)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("poll status %d = %v, want %q", status, err, want)
		}
		if *polls != 1 {
			t.Errorf("status %d was polled %d times; a refusal is final", status, *polls)
		}
	}
}

func TestAuthorizeCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "test-token")
	cmd := AuthorizeCmd()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"sample", "--scope", "read", "--gateway-url", "http://127.0.0.1:1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("canceled authorization succeeded")
	}
}
