package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DeviceCodeResponse is the JSON returned by /api/cli/device/code.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DeviceLogin performs the device-code grant flow (RFC 8628) for headless environments.
// Used by `relay login --device`.
func DeviceLogin(ctx context.Context, gatewayURL string) (*LoginResult, error) {
	// 1. Request device code
	endpoint := fmt.Sprintf("%s/api/cli/device/code", strings.TrimRight(gatewayURL, "/"))
	resp, err := http.Post(endpoint, "application/json", nil)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("device code request failed (%d): %s", resp.StatusCode, errBody["error_description"])
	}

	var dc DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, fmt.Errorf("decode device code response: %w", err)
	}

	// 2. Prompt user
	fmt.Printf("\nOpen this URL in your browser:\n  %s\n\n", dc.VerificationURI)
	fmt.Printf("Enter this code when prompted: %s\n\n", dc.UserCode)
	fmt.Println("Waiting for authorization...")

	// 3. Poll /api/cli/device/token
	interval := dc.Interval
	if interval < 5 {
		interval = 5
	}
	return pollDeviceToken(ctx, gatewayURL, dc.DeviceCode, interval)
}

// pollDeviceToken polls the device token endpoint until authorized or timed out.
func pollDeviceToken(ctx context.Context, gatewayURL, deviceCode string, intervalSecs int) (*LoginResult, error) {
	tokenEndpoint := fmt.Sprintf("%s/api/cli/device/token", strings.TrimRight(gatewayURL, "/"))

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("device login timed out")
		default:
		}

		body := strings.NewReader(`{"device_code":"` + deviceCode + `"}`)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, body)
		if err != nil {
			return nil, fmt.Errorf("create poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("poll device token: %w", err)
		}

		var result map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			// Success — extract token pair
			accessToken, _ := result["access_token"].(string)
			refreshToken, _ := result["refresh_token"].(string)
			email, _ := result["email"].(string)
			expiresIn := 3600
			if v, ok := result["expires_in"].(float64); ok {
				expiresIn = int(v)
			}

			if err := writeKeychainToken(email, accessToken, refreshToken, expiresIn); err != nil {
				return nil, err
			}

			return &LoginResult{
				Email:        email,
				AccessToken:  accessToken,
				RefreshToken: refreshToken,
				ExpiresIn:    expiresIn,
			}, nil

		case http.StatusRequestTimeout, 428: // authorization_pending (428 Precondition Required)
			// Wait and retry
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("device login timed out")
			case <-waitSeconds(intervalSecs):
			}
			continue

		case http.StatusBadRequest:
			errCode, _ := result["error"].(string)
			errDesc, _ := result["error_description"].(string)
			if errCode == "expired_token" {
				return nil, fmt.Errorf("device code expired — run `relay login --device` again")
			}
			return nil, fmt.Errorf("device token error: %s — %s", errCode, errDesc)

		default:
			return nil, fmt.Errorf("unexpected status %d from device token endpoint", resp.StatusCode)
		}
	}
}

func writeKeychainToken(email, accessToken, refreshToken string, expiresIn int) error {
	return writeToken(email, accessToken, refreshToken, expiresIn)
}

// waitSeconds returns a channel that closes after n seconds.
func waitSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}
