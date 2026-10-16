package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/dpop"
	"github.com/patrikmichi/zuano/internal/keychain"
)

func keyFiles(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".config", "zuano", "dpop"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func deviceGatewayWithTokenType(t *testing.T, tokenType string, tokenStatus int, tokenBody string, got *http.Header, gotBody *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cli/device/code":
			_, _ = w.Write([]byte(validDeviceCode))
		case "/api/cli/device/token":
			*got = r.Header.Clone()
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			w.WriteHeader(tokenStatus)
			if tokenBody != "" {
				_, _ = w.Write([]byte(tokenBody))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "token_type": tokenType, "email": "user@example.com", "expires_in": 600})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDeviceLogin_StrictOrganisationBindsTheSessionToTheDeviceKey(t *testing.T) {
	withTempHome(t)
	var header http.Header
	var body map[string]any
	srv := deviceGatewayWithTokenType(t, "DPoP", 200, "", &header, &body)

	var out bytes.Buffer
	if _, err := DeviceLogin(context.Background(), srv.URL, keychain.ProfileHuman, &out); err != nil {
		t.Fatalf("DeviceLogin: %v", err)
	}
	if header.Get("DPoP") == "" || body["dpop_key_type"] == nil {
		t.Errorf("the poll carried no proof or key type: %v %v", header, body)
	}
	stored, err := keychain.ReadToken(srv.URL, "user@example.com")
	if err != nil || !strings.HasPrefix(stored.DPoPKey, "file:") && !strings.HasPrefix(stored.DPoPKey, "tpm:") {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if _, err := dpop.Load(stored.DPoPKey); err != nil {
		t.Errorf("the stored key reference does not load: %v", err)
	}
	if !strings.Contains(out.String(), "bound to") {
		t.Errorf("the person was not told the session is bound:\n%s", out.String())
	}
}

func TestDeviceLogin_OrdinaryOrganisationDropsTheKeyAgain(t *testing.T) {
	withTempHome(t)
	var header http.Header
	var body map[string]any
	srv := deviceGatewayWithTokenType(t, "Bearer", 200, "", &header, &body)

	if _, err := DeviceLogin(context.Background(), srv.URL, keychain.ProfileHuman, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	stored, _ := keychain.ReadToken(srv.URL, "user@example.com")
	if stored.DPoPKey != "" || len(keyFiles(t)) != 0 {
		t.Errorf("a bearer session kept a device key: %+v, files %v", stored, keyFiles(t))
	}
}

func TestDeviceLogin_HardwareKeyRefusalExplainsAndLeavesNothingBehind(t *testing.T) {
	withTempHome(t)
	var header http.Header
	var body map[string]any
	srv := deviceGatewayWithTokenType(t, "", 400, `{"error":"hardware_key_required"}`, &header, &body)

	_, err := DeviceLogin(context.Background(), srv.URL, keychain.ProfileHuman, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "TPM 2.0") {
		t.Fatalf("err = %v", err)
	}
	if len(keyFiles(t)) != 0 {
		t.Errorf("a refused login left key files: %v", keyFiles(t))
	}
}

func TestForgetSessionRemovesTheBoundKey(t *testing.T) {
	withTempHome(t)
	key, err := dpop.Create(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := keychain.WriteToken("https://gw.example.com", "user@example.com", keychain.TokenData{AccessToken: "a", RefreshToken: "r", DPoPKey: key.Ref()}); err != nil {
		t.Fatal(err)
	}
	if err := ForgetSession(keychain.ProfileHuman, "https://gw.example.com", "user@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := keychain.ReadToken("https://gw.example.com", "user@example.com"); err == nil {
		t.Error("the session survived")
	}
	if _, err := dpop.Load(key.Ref()); err == nil {
		t.Error("the device key survived the logout")
	}
}

func TestExchangeCodeSendsAProofAndTheKeyType(t *testing.T) {
	withTempHome(t)
	var header http.Header
	var form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		_ = r.ParseForm()
		form = r.PostForm.Get("dpop_key_type")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","token_type":"DPoP","email":"u@example.com","expires_in":600}`))
	}))
	t.Cleanup(srv.Close)
	binding := newDeviceBinding(&bytes.Buffer{})
	defer binding.discard()
	tr, err := exchangeCodeContext(context.Background(), srv.URL, "code", "verifier", "http://127.0.0.1/cb", binding)
	if err != nil || tr.TokenType != "DPoP" {
		t.Fatalf("%+v, %v", tr, err)
	}
	if header.Get("DPoP") == "" || (form != "software" && form != "hardware") {
		t.Errorf("DPoP header %q, key type %q", header.Get("DPoP"), form)
	}
}
