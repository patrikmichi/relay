package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// refreshResult is the token pair returned by POST /api/cli/refresh
// (gateway app/api/cli/refresh/route.ts).
type refreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// refreshOAuthToken exchanges a refresh token for a rotated access+refresh
// pair. The gateway enforces reuse detection (a refresh token is single-use;
// presenting an already-spent one revokes the whole token family) — so the
// caller MUST persist the rotated pair before it can be used again.
func refreshOAuthToken(gatewayURL, refreshToken string) (*refreshResult, error) {
	endpoint := strings.TrimRight(gatewayURL, "/") + "/api/cli/refresh"

	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
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
		return nil, fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, desc)
	}

	var rr refreshResult
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if rr.AccessToken == "" || rr.RefreshToken == "" {
		return nil, fmt.Errorf("refresh response missing token fields")
	}
	return &rr, nil
}
