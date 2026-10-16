package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/keychain"
)

func TestDeviceLogin_WarnsWhenVerificationURLIsOffGateway(t *testing.T) {
	withTempHome(t)
	srv := deviceGateway(t, http.StatusOK, validDeviceCode, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "email": "user@example.com"})
	})

	var out bytes.Buffer
	if _, err := DeviceLogin(context.Background(), srv.URL, keychain.ProfileHuman, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "gw.example.com") {
		t.Errorf("off-gateway verification URL was not flagged:\n%s", out.String())
	}
}

func TestDeviceLogin_NoWarningOnGatewayHost(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/device/code" {
			_, _ = fmt.Fprintf(w, `{"device_code":"d","user_code":"U","verification_uri":"http://%s/device","interval":1}`, r.Host)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "email": "user@example.com"})
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	if _, err := DeviceLogin(context.Background(), srv.URL, keychain.ProfileHuman, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "WARNING") {
		t.Errorf("same-host verification URL was flagged:\n%s", out.String())
	}
}

func TestDeviceLogin_RefusesNonWebVerificationURL(t *testing.T) {
	withTempHome(t)
	for _, uri := range []string{"javascript:alert(1)", "file:///etc/passwd", "//evil.example.net/x", "not a url"} {
		body := fmt.Sprintf(`{"device_code":"d","user_code":"U","verification_uri":%q,"interval":1}`, uri)
		srv := deviceGateway(t, http.StatusOK, body, nil)
		_, err := DeviceLogin(context.Background(), srv.URL, keychain.ProfileHuman, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "not a web address") {
			t.Errorf("verification_uri %q = %v, want a refusal", uri, err)
		}
	}
}

func TestDeviceLogin_StripsControlCharactersFromPrompt(t *testing.T) {
	withTempHome(t)
	body := `{"device_code":"d","user_code":"AB\u001b[2JCD","verification_uri":"https://gw.example.com/dev\u001bice","interval":1}`
	srv := deviceGateway(t, http.StatusOK, body, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "email": "user@example.com"})
	})
	var out bytes.Buffer
	_, _ = DeviceLogin(context.Background(), srv.URL, keychain.ProfileHuman, &out)
	if strings.ContainsRune(out.String(), 0x1b) {
		t.Errorf("escape character reached the terminal: %q", out.String())
	}
}
