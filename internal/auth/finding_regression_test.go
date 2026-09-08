package auth

// Regression test derived from the 2026-09-07 deep review's reproduction
// probe (vault/reviews/relay/2026-09-07/evidence/auth_probes_test.go). That
// probe deliberately asserted the CURRENT (buggy) behavior; this test
// inverts the assertion to the SAFE behavior R26 demands. It is expected
// to FAIL against baseline 931aac4 and must turn green in the same
// commit/PR that lands the fix.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestR26_EmptyDeviceResponseRejected reproduces R26: pollDeviceToken
// ignores decode/required-field errors, so an HTTP 200 body of `{}`
// produces a "successful" LoginResult with every field empty. Safe
// behavior: a response missing required token/identity fields must
// return an error, never a result treated as a valid session.
func TestR26_EmptyDeviceResponseRejected(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "synthetic", 5)
	if err == nil {
		t.Fatalf("R26: expected an error for an empty HTTP 200 device-token response, got result=%+v", result)
	}
}
