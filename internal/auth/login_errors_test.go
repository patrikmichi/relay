package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

// stubBrowser replaces openBrowser with one that follows the redirect with a
// code and then reports browserErr, as a headless machine would.
func stubBrowser(t *testing.T, browserErr error) {
	t.Helper()
	original := openBrowser
	t.Cleanup(func() { openBrowser = original })
	openBrowser = func(authorizeURL string) error {
		u, err := url.Parse(authorizeURL)
		if err != nil {
			t.Errorf("parse authorize URL: %v", err)
			return browserErr
		}
		cb, _ := url.Parse(u.Query().Get("redirect_uri"))
		q := cb.Query()
		q.Set("state", u.Query().Get("state"))
		q.Set("code", "code-1")
		cb.RawQuery = q.Encode()
		go func() {
			resp, err := http.Get(cb.String())
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return browserErr
	}
}

func tokenServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 3600, "email": "user@example.com",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLogin_RejectsInsecureGatewayWithoutOpeningBrowser(t *testing.T) {
	withTempHome(t)
	original := openBrowser
	defer func() { openBrowser = original }()
	opened := false
	openBrowser = func(string) error { opened = true; return nil }

	if _, err := Login(context.Background(), "http://gw.example.com"); err == nil {
		t.Fatal("expected a remote plain-HTTP gateway to be rejected")
	}
	if opened {
		t.Error("browser opened for a rejected gateway URL")
	}
}

func TestLogin_BrowserFailurePrintsURLAndContinues(t *testing.T) {
	withTempHome(t)
	srv := tokenServer(t, http.StatusOK)
	stubBrowser(t, errors.New("no display"))

	var diag bytes.Buffer
	result, err := Login(context.Background(), srv.URL, &diag)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if result.Email != "user@example.com" {
		t.Errorf("unexpected result: %+v", result)
	}
	if !strings.Contains(diag.String(), "Please visit") || !strings.Contains(diag.String(), srv.URL+"/api/cli/authorize?") {
		t.Errorf("diagnostic output must carry the authorize URL, got:\n%s", diag.String())
	}
}

func TestLogin_TokenExchangeFailure(t *testing.T) {
	withTempHome(t)
	srv := tokenServer(t, http.StatusUnauthorized)
	stubBrowser(t, nil)

	_, err := Login(context.Background(), srv.URL, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exchange code") {
		t.Fatalf("got %v, want an exchange error", err)
	}
}

func TestLogin_KeychainWriteFailureAborts(t *testing.T) {
	withTempHome(t)
	srv := tokenServer(t, http.StatusOK)
	stubBrowser(t, nil)
	keyring.MockInitWithError(errors.New("keychain locked"))
	defer keyring.MockInit()

	if _, err := Login(context.Background(), srv.URL, &bytes.Buffer{}); err == nil {
		t.Fatal("expected a keychain failure to abort login")
	}
	t.Setenv("RELAY_EMAIL", "")
	if email, _ := config.ResolveEmail(); email != "" {
		t.Errorf("email persisted despite failed login: %q", email)
	}
}

func TestLogin_ConfigPersistFailureOnlyWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RELAY_EMAIL", "")
	cfgDir := filepath.Join(home, ".config", "relay")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := tokenServer(t, http.StatusOK)
	stubBrowser(t, nil)

	var diag bytes.Buffer
	if _, err := Login(context.Background(), srv.URL, &diag); err != nil {
		t.Fatalf("Login must succeed once the session is stored: %v", err)
	}
	if !strings.Contains(diag.String(), "could not persist login email") {
		t.Errorf("expected a config warning, got:\n%s", diag.String())
	}
	if _, err := keychain.ReadToken(srv.URL, "user@example.com"); err != nil {
		t.Errorf("session missing: %v", err)
	}
}

func TestOAuthCallbackHandler_RejectsNonGet(t *testing.T) {
	codes, failures := make(chan string, 1), make(chan error, 1)
	rec := httptest.NewRecorder()
	oauthCallbackHandler("s", codes, failures).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/callback?state=s&code=c", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("got %d Allow=%q, want 405 Allow=GET", rec.Code, rec.Header().Get("Allow"))
	}
	if len(codes) != 0 {
		t.Error("POST callback delivered a code")
	}
}

func TestOAuthCallbackHandler_ProviderErrorIsReported(t *testing.T) {
	codes, failures := make(chan string, 1), make(chan error, 1)
	h := oauthCallbackHandler("s", codes, failures)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?state=s&error=access_denied", nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", rec.Code)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("got %d reported failures, want exactly 1", len(failures))
	}
}

func TestOAuthCallbackHandler_RejectsOversizedCode(t *testing.T) {
	codes, failures := make(chan string, 1), make(chan error, 1)
	rec := httptest.NewRecorder()
	target := "/callback?state=s&code=" + strings.Repeat("c", 4097)
	oauthCallbackHandler("s", codes, failures).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusBadRequest || len(codes) != 0 {
		t.Fatalf("oversized code accepted: status %d", rec.Code)
	}
}

func TestRemoveLegacySession_WarnsOnFailure(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain locked"))
	defer keyring.MockInit()

	var diag bytes.Buffer
	removeLegacySession("user@example.com", &diag)
	if !strings.Contains(diag.String(), "could not remove the old session entry") {
		t.Errorf("expected a warning, got %q", diag.String())
	}
}

func TestWriteToken_ConfigPersistFailureIsNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".config"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeToken("https://gw.example.com", "user@example.com", "access-1", "refresh-1", 60); err != nil {
		t.Fatalf("writeToken: %v", err)
	}
	if _, err := keychain.ReadToken("https://gw.example.com", "user@example.com"); err != nil {
		t.Errorf("session missing: %v", err)
	}
}

func TestWhoami_TransportError(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	seedSession(t, srv.URL, "user@example.com", keychain.TokenData{AccessToken: "a", RefreshToken: "r", ExpiresIn: 3600, ExpiresAt: 1 << 40})
	c := resolveSeededClient(t, srv.URL)
	srv.Close()

	if _, err := Whoami(context.Background(), c, false); err == nil || !strings.Contains(err.Error(), "GET /api/cli/whoami") {
		t.Fatalf("got %v, want a transport error naming the endpoint", err)
	}
}
