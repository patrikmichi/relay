package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patrikmichi/relay/internal/keychain"
)

type scriptedStore struct {
	data     keychain.TokenData
	readErr  error
	writeErr error
	writes   int
}

func (s *scriptedStore) Read(_, _ string) (keychain.TokenData, error) { return s.data, s.readErr }

func (s *scriptedStore) Write(_, _ string, d keychain.TokenData) error {
	s.writes++
	if s.writeErr != nil {
		return s.writeErr
	}
	s.data = d
	return nil
}

type noLock struct{}

func (noLock) Lock(context.Context, string, string) (SessionUnlocker, error) { return noLock{}, nil }
func (noLock) Unlock() error                                                 { return nil }

func refreshableClient(t *testing.T, store SessionStore) (*Client, *atomic.Int32) {
	t.Helper()
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "old-access")
	c.email, c.gatewayOrigin, c.refreshToken = "user@example.com", srv.URL, "old-refresh"
	c.store, c.locker = store, noLock{}
	return c, &refreshes
}

func TestRefresh_AdoptsTheRotatedPairAndWarnsWhenTheKeychainWriteFails(t *testing.T) {
	var warn bytes.Buffer
	orig := persistWarning
	persistWarning = &warn
	t.Cleanup(func() { persistWarning = orig })

	store := &scriptedStore{data: keychain.TokenData{RefreshToken: "old-refresh"}, writeErr: errors.New("keychain locked")}
	c, _ := refreshableClient(t, store)
	if err := c.refreshLocked(context.Background()); err != nil {
		t.Fatalf("refresh = %v, want success with a warning", err)
	}
	if c.AccessToken != "new-access" || c.refreshToken != "new-refresh" {
		t.Errorf("in-memory pair = %q / %q", c.AccessToken, c.refreshToken)
	}
	if !strings.Contains(warn.String(), "WARNING") || !strings.Contains(warn.String(), "keychain locked") {
		t.Errorf("warning = %q", warn.String())
	}
}

func TestRefresh_FailsClosedWhenTheStoredSessionCannotBeRead(t *testing.T) {
	for name, readErr := range map[string]error{
		"unreadable": errors.New("keychain unavailable"),
		"removed":    keychain.ErrNotFound,
	} {
		store := &scriptedStore{readErr: readErr}
		c, refreshes := refreshableClient(t, store)
		if err := c.refreshLocked(context.Background()); err == nil {
			t.Errorf("%s: refresh succeeded without reading the stored session", name)
		}
		if refreshes.Load() != 0 {
			t.Errorf("%s: the refresh token was spent %d times", name, refreshes.Load())
		}
		if store.writes != 0 {
			t.Errorf("%s: wrote a session that was not readable", name)
		}
	}
}
