package auth

// Named regression test inverting the 2026-09-07 review's bug-confirming
// probe (evidence/auth_probes_test.go) into the safe behavior R26
// demands. Expected to fail until the fix lands in the same commit — do
// not weaken this assertion.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"
)

// R26: pollDeviceToken ignores decode/required-field errors, so an HTTP
// 200 body of `{}` produces a "successful" LoginResult with every field
// empty.
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
