package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxRefreshResponseBytes bounds the refresh endpoint's response body — a
// compromised or misbehaving gateway must not be able to wedge a refresh
// (and everything waiting on it) by streaming an unbounded body.
const maxRefreshResponseBytes = 1 << 20 // 1 MiB

// refreshResult is the token pair returned by the gateway's
// POST /api/cli/refresh endpoint.
type refreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// readCapped reads at most limit+1 bytes from r, erroring if the stream
// exceeds limit — applied before any refresh response body is trusted or
// decoded.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d byte limit", limit)
	}
	return data, nil
}

// refreshOAuthToken exchanges a refresh token for a rotated access+refresh
// pair. The gateway enforces reuse detection (a refresh token is single-use;
// presenting an already-spent one revokes the whole token family) — so the
// caller MUST persist the rotated pair before it can be used again. ctx
// bounds the whole request, including the case where the caller's own
// deadline is what should stop a stalled refresh.
func refreshOAuthToken(ctx context.Context, gatewayURL, refreshToken string) (*refreshResult, error) {
	endpoint := strings.TrimRight(gatewayURL, "/") + "/api/cli/refresh"

	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Refresh must never follow a redirect — the request body carries a
	// secret refresh token, which a redirect-following client would forward
	// to whatever origin the response pointed at.
	resp, err := NoRedirectHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	respBody, err := readCapped(resp.Body, maxRefreshResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]string
		_ = json.Unmarshal(respBody, &errBody)
		desc := errBody["error_description"]
		if desc == "" {
			desc = errBody["error"]
		}
		return nil, fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, desc)
	}

	var rr refreshResult
	if err := json.Unmarshal(respBody, &rr); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if rr.AccessToken == "" || rr.RefreshToken == "" {
		return nil, fmt.Errorf("refresh response missing token fields")
	}
	return &rr, nil
}
