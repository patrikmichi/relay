// Package auth implements the CLI authentication flows.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/browser"

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
func Login(ctx context.Context, gatewayURL string) (*LoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	// Step 1: PKCE
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, fmt.Errorf("generate PKCE: %w", err)
	}

	state, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	// Step 2: Start loopback server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if errParam := q.Get("error"); errParam != "" {
				errCh <- fmt.Errorf("oauth error: %s — %s", errParam, q.Get("error_description"))
				fmt.Fprintln(w, "Authentication failed. You can close this tab.")
				return
			}
			code := q.Get("code")
			returnedState := q.Get("state")
			if returnedState != state {
				errCh <- fmt.Errorf("state mismatch: expected %s got %s", state, returnedState)
				http.Error(w, "State mismatch", http.StatusBadRequest)
				return
			}
			codeCh <- code
			fmt.Fprintln(w, "Authentication complete. You can close this tab.")
		}),
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			// Non-fatal: server closed after we got the code
		}
	}()

	// Cleanup: close codeCh so parent select unblocks on any exit path
	defer func() {
		_ = server.Close()
	}()

	// Step 3: Open browser
	authorizeURL := buildAuthorizeURL(gatewayURL, cliClientID, redirectURI, state, challenge)
	if err := openBrowser(authorizeURL); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser automatically. Please visit:\n%s\n", authorizeURL)
	}

	// Step 4: Wait for callback
	var authCode string
	select {
	case code := <-codeCh:
		authCode = code
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("login timed out after %s", loginTimeout)
	}

	// Step 5: Exchange code for tokens
	resp, err := exchangeCode(gatewayURL, authCode, verifier, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	// Step 6: Store in keychain
	if err := keychain.WriteToken(resp.Email, keychain.TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		Email:        resp.Email,
		ExpiresIn:    resp.ExpiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Unix(),
	}); err != nil {
		return nil, err // keychain error is user-visible, caller exits 1
	}

	// Persist the logged-in email so subsequent commands don't require
	// RELAY_EMAIL to be set (client.Resolve / config.ResolveEmail). Best
	// effort — a failure here doesn't invalidate the login itself (the
	// keychain write above already succeeded); RELAY_EMAIL remains an
	// explicit fallback for the user.
	if err := config.SetEmail(resp.Email); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist login email to config: %v\n", err)
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
	endpoint := fmt.Sprintf("%s/api/cli/token", strings.TrimRight(gatewayURL, "/"))

	data := url.Values{
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}

	resp, err := http.PostForm(endpoint, data)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		desc := errBody["error_description"]
		if desc == "" {
			desc = errBody["error"]
		}
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, desc)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &tr, nil
}
