package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/patrikmichi/relay/internal/client"
	"github.com/patrikmichi/relay/internal/config"
	"github.com/patrikmichi/relay/internal/keychain"
	"github.com/patrikmichi/relay/internal/textsafe"
)

const maxAuthResponseBytes = 1 << 20 // 1 MiB

// defaultDeviceFlowTimeout bounds the entire device-code flow (request +
// poll loop) when the gateway's device-code response omits, or sends a
// nonsensical, expires_in. RFC 8628 deployments typically use 600-1800s;
// this is a conservative fallback, not the expected case.
const defaultDeviceFlowTimeout = 15 * time.Minute

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

func parseVerificationURI(uri string) (*url.URL, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("the gateway returned a verification URL that is not a web address: %q", textsafe.Clean(uri))
	}
	return u, nil
}

func checkVerificationScheme(uri string) error {
	_, err := parseVerificationURI(uri)
	return err
}

// verificationHostWarning is non-empty when the page the user is told to open
// is not on the gateway they are logging in to: typing the one-time code
// there would hand the login to whoever runs that host.
func verificationHostWarning(origin, uri string) string {
	u, err := parseVerificationURI(uri)
	gw, gerr := url.Parse(origin)
	if err != nil || gerr != nil {
		return ""
	}
	if u.Scheme == gw.Scheme && u.Host == gw.Host {
		return ""
	}
	return fmt.Sprintf("WARNING: this address is on %s, not on your gateway (%s).\nDo not enter the code unless you recognise that host.", textsafe.Clean(u.Host), gw.Host)
}

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

// DeviceLogin performs the device-code flow. With the agent profile the
// device-code request carries agent: true, like Login's agent=1; the token
// poll carries no flag.
func DeviceLogin(ctx context.Context, gatewayURL string, profile keychain.Profile, writers ...io.Writer) (*LoginResult, error) {
	var out io.Writer = os.Stdout
	if len(writers) > 0 && writers[0] != nil {
		out = writers[0]
	}
	origin, err := config.NormalizeGatewayURL(gatewayURL)
	if err != nil {
		return nil, err
	}

	// 1. Request device code
	endpoint := fmt.Sprintf("%s/api/cli/device/code", origin)
	codeBody := "{}"
	if profile == keychain.ProfileAgent {
		codeBody = `{"agent":true}`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(codeBody))
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
	fmt.Fprintf(out, "\n%s\n", sessionKindNotice(profile))
	if err := checkVerificationScheme(dc.VerificationURI); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "\nOpen this URL in your browser:\n  %s\n\n", textsafe.Clean(dc.VerificationURI))
	if warning := verificationHostWarning(origin, dc.VerificationURI); warning != "" {
		fmt.Fprintf(out, "%s\n\n", warning)
	}
	fmt.Fprintf(out, "Enter this code when prompted: %s\n\n", textsafe.Clean(dc.UserCode))
	fmt.Fprintln(out, "Waiting for authorization...")

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
	return pollDeviceTokenProfile(pollCtx, origin, dc.DeviceCode, interval, profile)
}

// pollDeviceTokenProfile polls the device token endpoint until authorized,
// rejected, expired, or ctx is done — whichever comes first. Every
// individual HTTP request is bound by the SAME ctx as the loop itself, so
// one stalled request cannot outlive — and therefore cannot defeat — the
// poll loop's own deadline. gatewayURL must already be a normalized origin
// (see DeviceLogin).
func pollDeviceTokenProfile(ctx context.Context, gatewayURL, deviceCode string, intervalSecs int, profile keychain.Profile) (*LoginResult, error) {
	tokenEndpoint := fmt.Sprintf("%s/api/cli/device/token", strings.TrimRight(gatewayURL, "/"))

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("device login timed out")
		default:
		}

		reqBody, err := json.Marshal(map[string]any{"device_code": deviceCode})
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
		_ = resp.Body.Close()

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

			if err := writeToken(profile, gatewayURL, dtr.Email, dtr.AccessToken, dtr.RefreshToken, expiresIn); err != nil {
				return nil, err
			}

			return &LoginResult{
				Email:        dtr.Email,
				AccessToken:  dtr.AccessToken,
				RefreshToken: dtr.RefreshToken,
				ExpiresIn:    expiresIn,
			}, nil

		case dtr.Error == "authorization_pending",
			resp.StatusCode == http.StatusRequestTimeout,
			resp.StatusCode == http.StatusPreconditionRequired:
			if !waitOrDone(ctx, intervalSecs) {
				return nil, fmt.Errorf("device login timed out")
			}
			continue

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
