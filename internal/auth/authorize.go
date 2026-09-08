package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
)

// maxAuthResponseBytes bounds every gateway response decoded in this file —
// a misbehaving or compromised gateway must not be able to wedge a login by
// streaming an unbounded body (R23).
const maxAuthResponseBytes = 1 << 20 // 1 MiB

// defaultDeviceFlowTimeout bounds the entire device-code flow (request +
// poll loop) when the gateway's device-code response omits, or sends a
// nonsensical, expires_in. RFC 8628 deployments typically use 600-1800s;
// this is a conservative fallback, not the expected case.
const defaultDeviceFlowTimeout = 15 * time.Minute

// slowDownIncrementSecs is added to the poll interval on a slow_down
// response, per RFC 8628 §3.5's recommendation. A var (not const) so tests
// can shrink it instead of sleeping the production 5s per iteration.
var slowDownIncrementSecs = 5

// DeviceCodeResponse is the JSON returned by /api/cli/device/code.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (dc DeviceCodeResponse) validate() error {
	if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" {
		return fmt.Errorf("device code response missing required field(s)")
	}
	return nil
}

// deviceTokenResponse is the typed JSON shape returned by
// /api/cli/device/token for BOTH a terminal success (200) and every
// RFC 8628 §3.5 error/pending case (authorization_pending, slow_down,
// expired_token, access_denied, ...). A single typed struct — never
// map[string]interface{} — means a missing or wrong-typed field decodes to
// its zero value deterministically instead of an unchecked type assertion
// silently producing "" (R26).
type deviceTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	Email            string `json:"email"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (dtr deviceTokenResponse) validateSuccess() error {
	if dtr.AccessToken == "" || dtr.RefreshToken == "" || dtr.Email == "" {
		return fmt.Errorf("device token response missing required field(s)")
	}
	return nil
}

// decodeJSONCapped decodes body into v, refusing to buffer more than
// maxAuthResponseBytes — applied before any gateway response byte is
// trusted or parsed.
func decodeJSONCapped(body io.Reader, v interface{}) error {
	data, err := io.ReadAll(io.LimitReader(body, maxAuthResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxAuthResponseBytes {
		return fmt.Errorf("response exceeds %d byte limit", maxAuthResponseBytes)
	}
	return json.Unmarshal(data, v)
}

// DeviceLogin performs the device-code grant flow (RFC 8628) for headless
// environments. Used by `relay login --device`. ctx is a caller-supplied
// deadline (e.g. from cmd.Context()) that this narrows further to the
// device code's own advertised lifetime, so a gateway that answers
// "pending" forever cannot keep polling past when the device code it
// issued actually expires (R23).
func DeviceLogin(ctx context.Context, gatewayURL string) (*LoginResult, error) {
	origin, err := config.NormalizeGatewayURL(gatewayURL)
	if err != nil {
		return nil, err
	}

	// 1. Request device code
	endpoint := fmt.Sprintf("%s/api/cli/device/code", origin)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.NoRedirectHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]string
		_ = decodeJSONCapped(resp.Body, &errBody)
		return nil, fmt.Errorf("device code request failed (%d): %s", resp.StatusCode, errBody["error_description"])
	}

	var dc DeviceCodeResponse
	if err := decodeJSONCapped(resp.Body, &dc); err != nil {
		return nil, fmt.Errorf("decode device code response: %w", err)
	}
	if err := dc.validate(); err != nil {
		return nil, err
	}

	// 2. Prompt user
	fmt.Printf("\nOpen this URL in your browser:\n  %s\n\n", dc.VerificationURI)
	fmt.Printf("Enter this code when prompted: %s\n\n", dc.UserCode)
	fmt.Println("Waiting for authorization...")

	deviceExpiry := time.Duration(dc.ExpiresIn) * time.Second
	if deviceExpiry <= 0 {
		deviceExpiry = defaultDeviceFlowTimeout
	}
	pollCtx, cancel := context.WithTimeout(ctx, deviceExpiry)
	defer cancel()

	// 3. Poll /api/cli/device/token
	interval := dc.Interval
	if interval < 5 {
		interval = 5
	}
	return pollDeviceToken(pollCtx, origin, dc.DeviceCode, interval)
}

// pollDeviceToken polls the device token endpoint until authorized,
// rejected, expired, or ctx is done — whichever comes first. Every
// individual HTTP request is bound by the SAME ctx as the loop itself, so
// one stalled request cannot outlive — and therefore cannot defeat — the
// poll loop's own deadline. gatewayURL must already be a normalized origin
// (see DeviceLogin).
func pollDeviceToken(ctx context.Context, gatewayURL, deviceCode string, intervalSecs int) (*LoginResult, error) {
	tokenEndpoint := fmt.Sprintf("%s/api/cli/device/token", strings.TrimRight(gatewayURL, "/"))

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("device login timed out")
		default:
		}

		reqBody, err := json.Marshal(map[string]string{"device_code": deviceCode})
		if err != nil {
			return nil, fmt.Errorf("encode poll request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(string(reqBody)))
		if err != nil {
			return nil, fmt.Errorf("create poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.NoRedirectHTTPClient().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("device login timed out")
			}
			return nil, fmt.Errorf("poll device token: %w", err)
		}

		var dtr deviceTokenResponse
		decodeErr := decodeJSONCapped(resp.Body, &dtr)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			if decodeErr != nil {
				return nil, fmt.Errorf("decode device token response: %w", decodeErr)
			}
			if err := dtr.validateSuccess(); err != nil {
				return nil, err
			}
			expiresIn := dtr.ExpiresIn
			if expiresIn <= 0 {
				expiresIn = 3600
			}

			if err := writeKeychainToken(gatewayURL, dtr.Email, dtr.AccessToken, dtr.RefreshToken, expiresIn); err != nil {
				return nil, err
			}

			return &LoginResult{
				Email:        dtr.Email,
				AccessToken:  dtr.AccessToken,
				RefreshToken: dtr.RefreshToken,
				ExpiresIn:    expiresIn,
			}, nil

		// authorization_pending — RFC 8628 §3.5 specifies HTTP 400 with this
		// error code; this gateway's deployed contract additionally uses
		// 408/428 as alternate pending signals, kept here for compatibility.
		case dtr.Error == "authorization_pending",
			resp.StatusCode == http.StatusRequestTimeout,
			resp.StatusCode == http.StatusPreconditionRequired:
			if !waitOrDone(ctx, intervalSecs) {
				return nil, fmt.Errorf("device login timed out")
			}
			continue

		// slow_down — RFC 8628 §3.5: the client is polling too fast. The
		// spec requires increasing the poll interval, not retrying at the
		// same cadence.
		case dtr.Error == "slow_down":
			intervalSecs += slowDownIncrementSecs
			if !waitOrDone(ctx, intervalSecs) {
				return nil, fmt.Errorf("device login timed out")
			}
			continue

		case resp.StatusCode == http.StatusBadRequest:
			if dtr.Error == "expired_token" {
				return nil, fmt.Errorf("device code expired — run `relay login --device` again")
			}
			return nil, fmt.Errorf("device token error: %s — %s", dtr.Error, dtr.ErrorDescription)

		default:
			return nil, fmt.Errorf("unexpected status %d from device token endpoint", resp.StatusCode)
		}
	}
}

func writeKeychainToken(gatewayOrigin, email, accessToken, refreshToken string, expiresIn int) error {
	return writeToken(gatewayOrigin, email, accessToken, refreshToken, expiresIn)
}

// waitOrDone blocks for n seconds or until ctx is done, whichever comes
// first — false means ctx won the race and polling must stop.
func waitOrDone(ctx context.Context, n int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(n) * time.Second):
		return true
	}
}
