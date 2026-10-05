// Package auth implements the CLI authentication flows.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/browser"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
)

const (
	loginTimeout = 120 * time.Second
	cliClientID  = "relay-cli"
)

// openBrowser launches the system browser to the given URL. A package-level
// var (rather than calling browser.OpenURL directly) so tests can stub it
// out instead of launching a real browser.
var openBrowser = browser.OpenURL

// LoginResult holds the result of a successful login.
type LoginResult struct {
	Email        string
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

// tokenResponse is the JSON structure returned by /api/cli/token.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Email        string `json:"email"`
}

// Login performs the PKCE loopback flow:
//  1. Generate code_verifier + challenge (S256)
//  2. Start local loopback server on random port
//  3. Open browser to /api/cli/authorize
//  4. Wait for callback (max 120s)
//  5. Exchange code at /api/cli/token
//  6. Store token in OS keychain
func Login(ctx context.Context, gatewayURL string, writers ...io.Writer) (*LoginResult, error) {
	var diagnostic io.Writer = os.Stderr
	if len(writers) > 0 && writers[0] != nil {
		diagnostic = writers[0]
	}
	// The keychain entry this login produces is bound to this normalized
	// origin — reject an invalid/insecure gateway value before opening a
	// browser or starting the loopback server.
	origin, err := config.NormalizeGatewayURL(gatewayURL)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, fmt.Errorf("generate PKCE: %w", err)
	}

	state, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    16 << 10,
		Handler:           oauthCallbackHandler(state, codeCh, errCh),
	}

	go func() {
		// Serve returns once the server is shut down after the callback; a
		// listener error then only means no callback arrives, which the
		// login timeout reports.
		_ = server.Serve(listener)
	}()

	defer func() {
		_ = server.Close()
	}()

	authorizeURL := buildAuthorizeURL(origin, cliClientID, redirectURI, state, challenge)
	if err := openBrowser(authorizeURL); err != nil {
		fmt.Fprintf(diagnostic, "Could not open browser automatically. Please visit:\n%s\n", authorizeURL)
	}

	var authCode string
	select {
	case code := <-codeCh:
		authCode = code
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("login timed out after %s", loginTimeout)
	}

	resp, err := exchangeCodeContext(ctx, origin, authCode, verifier, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	if err := keychain.WriteToken(origin, resp.Email, keychain.TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    resp.ExpiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Unix(),
	}); err != nil {
		return nil, err // keychain error is user-visible, caller exits 1
	}
	removeLegacySession(resp.Email, diagnostic)

	// Persist the logged-in email so subsequent commands don't require
	// RELAY_EMAIL to be set (client.Resolve / config.ResolveEmail). Best
	// effort — a failure here doesn't invalidate the login itself (the
	// keychain write above already succeeded); RELAY_EMAIL remains an
	// explicit fallback for the user.
	if err := config.SetEmail(resp.Email); err != nil {
		fmt.Fprintf(diagnostic, "Warning: could not persist login email to config: %v\n", err)
	}

	return &LoginResult{
		Email:        resp.Email,
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    resp.ExpiresIn,
	}, nil
}

// generatePKCE generates a PKCE verifier and S256 challenge.
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

// randomHex generates a random hex string of n bytes.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// buildAuthorizeURL constructs the /api/cli/authorize URL.
func buildAuthorizeURL(gatewayURL, clientID, redirectURI, state, challenge string) string {
	u := fmt.Sprintf("%s/api/cli/authorize", strings.TrimRight(gatewayURL, "/"))
	params := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
	}
	return u + "?" + params.Encode()
}

// exchangeCode exchanges an authorization code for a token pair.
func exchangeCode(gatewayURL, code, verifier, redirectURI string) (*tokenResponse, error) {
	return exchangeCodeContext(context.Background(), gatewayURL, code, verifier, redirectURI)
}

func exchangeCodeContext(ctx context.Context, gatewayURL, code, verifier, redirectURI string) (*tokenResponse, error) {
	endpoint := fmt.Sprintf("%s/api/cli/token", strings.TrimRight(gatewayURL, "/"))

	data := url.Values{
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}

	// Token exchange must never follow a redirect — a compromised or
	// misconfigured gateway could otherwise relocate the response carrying
	// the minted token pair to another origin. http.PostForm uses
	// http.DefaultClient, which follows redirects, so build the request
	// explicitly instead.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.NoRedirectHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (HTTP %d)", resp.StatusCode)
	}

	var tr tokenResponse
	if err := decodeJSONCapped(resp.Body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" || tr.Email == "" || tr.ExpiresIn <= 0 || tr.ExpiresIn > 86400*365 {
		return nil, fmt.Errorf("invalid token response: required session fields missing or expiry invalid")
	}
	return &tr, nil
}

func oauthCallbackHandler(state string, codeCh chan<- string, errCh chan<- error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "Invalid authorization response", http.StatusBadRequest)
			return
		}
		if q.Get("error") != "" {
			select {
			case errCh <- fmt.Errorf("authorization was denied by the identity provider"):
			default:
			}
			http.Error(w, "Authentication failed. You can close this tab.", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" || len(code) > 4096 {
			http.Error(w, "Invalid authorization response", http.StatusBadRequest)
			return
		}
		select {
		case codeCh <- code:
		default:
			http.Error(w, "Authorization response already received", http.StatusConflict)
			return
		}
		_, _ = fmt.Fprintln(w, "Authentication complete. You can close this tab.")
	})
}
