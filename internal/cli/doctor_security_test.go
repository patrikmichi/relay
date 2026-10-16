package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/dpop"
	"github.com/patrikmichi/zuano/internal/keychain"
)

func doctorGateway(t *testing.T, strict bool, passkeys string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cli/whoami":
			if strict {
				_, _ = io.WriteString(w, `{"email":"doctor@example.com","strict_mode":true}`)
				return
			}
			_, _ = io.WriteString(w, `{"email":"doctor@example.com"}`)
		case "/api/me/passkeys":
			_, _ = io.WriteString(w, passkeys)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loginForDoctor(t *testing.T, srv *httptest.Server, keyRef string) {
	t.Helper()
	t.Setenv("ZUANO_URL", srv.URL)
	origin, _ := config.NormalizeGatewayURL(srv.URL)
	t.Setenv("ZUANO_EMAIL", "doctor@example.com")
	if err := keychain.WriteToken(origin, "doctor@example.com", keychain.TokenData{AccessToken: syntheticKey("access"), ExpiresAt: time.Now().Add(time.Hour).Unix(), DPoPKey: keyRef}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keychain.DeleteToken(origin, "doctor@example.com") })
}

func TestDoctor_ReportsPasskeysAndAnOrdinarySession(t *testing.T) {
	passkeys := contractFixture(t, "passkeys.json") // before setupDoctor changes the directory
	setupDoctor(t)
	srv := doctorGateway(t, false, passkeys)
	loginForDoctor(t, srv, "")
	output, err := runDoctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	if c := doctorCheckNamed(t, output, "passkeys"); !c.OK || c.Detail != "2 enrolled" {
		t.Errorf("passkeys: %+v", c)
	}
	if c := doctorCheckNamed(t, output, "device-key"); !c.OK || !strings.Contains(c.Detail, "not required") {
		t.Errorf("device-key: %+v", c)
	}
}

func TestDoctor_NoPasskeyIsInformationNotAFailure(t *testing.T) {
	setupDoctor(t)
	srv := doctorGateway(t, false, `{"passkeys":[]}`)
	loginForDoctor(t, srv, "")
	output, err := runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "passkeys"); err != nil || !c.OK || !strings.Contains(c.Detail, "none enrolled") {
		t.Errorf("passkeys: %+v, err %v", c, err)
	}
}

func TestDoctor_StrictOrganisationNeedsADeviceBoundSession(t *testing.T) {
	setupDoctor(t)
	srv := doctorGateway(t, true, `{"passkeys":[]}`)
	loginForDoctor(t, srv, "")
	output, err := runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "device-key"); err == nil || c.OK || !strings.Contains(c.Detail, "requires a device-bound session") {
		t.Errorf("device-key: %+v, err %v", c, err)
	}
}

func TestDoctor_BoundSessionReportsItsKeyAndAMissingKeyFails(t *testing.T) {
	setupDoctor(t)
	key, err := dpop.Create(false)
	if err != nil {
		t.Fatal(err)
	}
	srv := doctorGateway(t, true, `{"passkeys":[]}`)
	loginForDoctor(t, srv, key.Ref())
	output, err := runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "device-key"); err != nil || !c.OK || !strings.Contains(c.Detail, "bound to a device key") {
		t.Errorf("device-key: %+v, err %v", c, err)
	}

	if err := dpop.Delete(key.Ref()); err != nil {
		t.Fatal(err)
	}
	output, err = runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "gateway"); err == nil || c.OK {
		t.Errorf("a session whose key is gone must not authenticate: %+v, err %v", c, err)
	}
}

func TestDoctor_ReportsAStrayEmailOnlyKeychainEntry(t *testing.T) {
	setupDoctor(t)
	t.Setenv("ZUANO_EMAIL", "stray@example.com")
	if err := keyring.Set("relay-cli", "oauth-refresh-token:stray@example.com", `{"access_token":"stray"}`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keychain.DeleteUnboundToken("stray@example.com") })
	output, err := runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "stray-session"); err == nil || c.OK || !strings.Contains(c.Detail, "login") {
		t.Errorf("stray-session: %+v, err %v", c, err)
	}
}
