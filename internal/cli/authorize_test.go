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
