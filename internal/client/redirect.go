package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// GetRedirect sends an authenticated GET and, when the gateway answers with a
// redirect, returns the target instead of following it: the credential stays
// with the gateway and the caller fetches the target without it. A response
// that is not a redirect comes back as resp for the caller to read.
func (c *Client) GetRedirect(ctx context.Context, path string) (resp *http.Response, location string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.GatewayURL+path, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	resp, err = c.Do(req)
	if err == nil {
		return resp, "", nil
	}
	if resp != nil && errors.Is(err, errRedirectRefused) && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if target, perr := resp.Request.URL.Parse(resp.Header.Get("Location")); perr == nil && resp.Header.Get("Location") != "" {
			return nil, target.String(), nil
		}
	}
	return nil, "", err
}
