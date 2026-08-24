package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// init installs a mock keyring so these tests never touch the real OS keychain.
func init() {
	keyring.MockInit()
}

// withTempHome redirects os.UserHomeDir (via $HOME) to an isolated temp dir
// for the duration of the test, so config.Load/Save never touch the real
// developer machine's ~/.config/relay.
func withTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// ─── generatePKCE / randomHex ──────────────────────────────────────────────

func TestGeneratePKCE_ChallengeIsS256OfVerifier(t *testing.T) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE: %v", err)
	}

	if _, err := base64.RawURLEncoding.DecodeString(verifier); err != nil {
		t.Errorf("verifier is not valid base64url: %v", err)
	}

	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Errorf("challenge: got %q, want S256(verifier) = %q", challenge, want)
	}
}

func TestGeneratePKCE_ProducesUniqueVerifiers(t *testing.T) {
	v1, _, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE (1): %v", err)
	}
	v2, _, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE (2): %v", err)
	}
	if v1 == v2 {
		t.Error("expected two calls to generatePKCE to produce distinct verifiers")
	}
}

func TestRandomHex_LengthAndCharset(t *testing.T) {
	s, err := randomHex(16)
	if err != nil {
		t.Fatalf("randomHex: %v", err)
	}
	if len(s) != 32 { // 16 bytes -> 32 hex chars
		t.Errorf("length: got %d, want 32", len(s))
	}
	if strings.Trim(s, "0123456789abcdef") != "" {
		t.Errorf("randomHex produced non-hex characters: %q", s)
	}
}

// ─── buildAuthorizeURL ──────────────────────────────────────────────────────

func TestBuildAuthorizeURL_IncludesAllRequiredParams(t *testing.T) {
	got := buildAuthorizeURL("https://gw.example.com/", "relay-cli", "http://127.0.0.1:1234/callback", "state-1", "challenge-1")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://gw.example.com/api/cli/authorize" {
		t.Errorf("base URL: got %q (trailing slash on gatewayURL should be trimmed once)", u.Scheme+"://"+u.Host+u.Path)
	}

	q := u.Query()
	cases := map[string]string{
		"client_id":             "relay-cli",
		"redirect_uri":          "http://127.0.0.1:1234/callback",
		"state":                 "state-1",
		"code_challenge":        "challenge-1",
		"code_challenge_method": "S256",
		"response_type":         "code",
	}
	for k, want := range cases {
		if got := q.Get(k); got != want {
			t.Errorf("query param %q: got %q, want %q", k, got, want)
		}
	}
}

// ─── exchangeCode ───────────────────────────────────────────────────────────

func TestExchangeCode_Success(t *testing.T) {
	var gotCode, gotVerifier, gotRedirect string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli/token" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotCode = r.FormValue("code")
		gotVerifier = r.FormValue("code_verifier")
		gotRedirect = r.FormValue("redirect_uri")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"email":         "user@example.com",
		})
	}))
	defer srv.Close()

	tr, err := exchangeCode(srv.URL, "code-1", "verifier-1", "http://127.0.0.1:9999/callback")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if gotCode != "code-1" || gotVerifier != "verifier-1" || gotRedirect != "http://127.0.0.1:9999/callback" {
		t.Errorf("request form values not forwarded correctly: code=%q verifier=%q redirect=%q", gotCode, gotVerifier, gotRedirect)
	}
	if tr.AccessToken != "access-1" || tr.RefreshToken != "refresh-1" || tr.Email != "user@example.com" || tr.ExpiresIn != 3600 {
		t.Errorf("unexpected tokenResponse: %+v", tr)
	}
}

func TestExchangeCode_NonOKWithErrorDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "code already used",
		})
	}))
	defer srv.Close()

	_, err := exchangeCode(srv.URL, "code-1", "verifier-1", "http://127.0.0.1:9999/callback")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "code already used") {
		t.Errorf("error should surface status + description, got: %v", err)
	}
}

func TestExchangeCode_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json{{{"))
	}))
	defer srv.Close()

	_, err := exchangeCode(srv.URL, "code-1", "verifier-1", "http://127.0.0.1:9999/callback")
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode token response") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

func TestExchangeCode_NetworkDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachable := srv.URL
	srv.Close() // closed before use -> connection refused

	_, err := exchangeCode(unreachable, "code-1", "verifier-1", "http://127.0.0.1:9999/callback")
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
}

// ─── Login (full PKCE loopback flow) ───────────────────────────────────────

// driveCallback stubs openBrowser to asynchronously "click through" the
// browser consent flow: it parses the authorize URL Login generated, then
// issues an HTTP GET to the loopback redirect_uri with the given query
// overrides (e.g. a fake authorization code, or an error) — exactly what a
// real browser redirect would do after the user approves/denies on the
// gateway's consent screen.
func driveCallback(t *testing.T, overrides map[string]string) (restore func()) {
	t.Helper()
	original := openBrowser
	openBrowser = func(authorizeURL string) error {
		u, err := url.Parse(authorizeURL)
		if err != nil {
			t.Errorf("stub: parse authorize URL: %v", err)
			return nil
		}
		redirectURI := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")

		go func() {
			cb, _ := url.Parse(redirectURI)
			q := cb.Query()
			q.Set("state", state)
			for k, v := range overrides {
				q.Set(k, v)
			}
			cb.RawQuery = q.Encode()
			_, _ = http.Get(cb.String())
		}()
		return nil
	}
	return func() { openBrowser = original }
}

func TestLogin_HappyPath(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli/token" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"email":         "user@example.com",
		})
	}))
	defer srv.Close()

	restore := driveCallback(t, map[string]string{"code": "fake-code"})
	defer restore()

	result, err := Login(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if result.Email != "user@example.com" || result.AccessToken != "access-1" || result.RefreshToken != "refresh-1" || result.ExpiresIn != 3600 {
		t.Errorf("unexpected LoginResult: %+v", result)
	}

	tok, err := keychain.ReadToken("user@example.com")
	if err != nil {
		t.Fatalf("expected token persisted to keychain: %v", err)
	}
	if tok.AccessToken != "access-1" {
		t.Errorf("keychain AccessToken: got %q, want access-1", tok.AccessToken)
	}

	email, err := config.ResolveEmail()
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if email != "user@example.com" {
		t.Errorf("persisted login email: got %q, want user@example.com", email)
	}
}

func TestLogin_OAuthErrorFromCallback(t *testing.T) {
	withTempHome(t)

	restore := driveCallback(t, map[string]string{
		"error":             "access_denied",
		"error_description": "user declined",
	})
	defer restore()

	_, err := Login(context.Background(), "https://gw.example.com")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("expected oauth error to surface, got: %v", err)
	}
}

func TestLogin_StateMismatchFromCallback(t *testing.T) {
	withTempHome(t)

	restore := driveCallback(t, map[string]string{"code": "fake-code", "state": "tampered-state"})
	defer restore()

	_, err := Login(context.Background(), "https://gw.example.com")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("expected state mismatch error, got: %v", err)
	}
}

func TestLogin_TimesOutWithNoCallback(t *testing.T) {
	withTempHome(t)

	// Stub openBrowser to a no-op — no callback ever arrives.
	original := openBrowser
	openBrowser = func(string) error { return nil }
	defer func() { openBrowser = original }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := Login(ctx, "https://gw.example.com")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}
