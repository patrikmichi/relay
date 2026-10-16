package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/keychain"
)

func pollDeviceToken(ctx context.Context, gatewayURL, deviceCode string, intervalSecs int) (*LoginResult, error) {
	return pollDeviceTokenProfile(ctx, gatewayURL, deviceCode, intervalSecs, keychain.ProfileHuman)
}

func TestPollDeviceToken_SuccessOnFirstPoll(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"email":         "user@example.com",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0)
	if err != nil {
		t.Fatalf("pollDeviceToken: %v", err)
	}
	if result.Email != "user@example.com" || result.AccessToken != "access-1" || result.RefreshToken != "refresh-1" || result.ExpiresIn != 3600 {
		t.Errorf("unexpected result: %+v", result)
	}

	if _, err := keychain.ReadToken(srv.URL, "user@example.com"); err != nil {
		t.Errorf("expected token persisted to keychain: %v", err)
	}
}

func TestPollDeviceToken_PendingThenSuccess(t *testing.T) {
	withTempHome(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusRequestTimeout) // authorization_pending
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "authorization_pending"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "access-2", "refresh_token": "refresh-2", "email": "user@example.com", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0)
	if err != nil {
		t.Fatalf("pollDeviceToken: %v", err)
	}
	if hits != 2 {
		t.Errorf("expected exactly 2 polls (pending then success), got %d", hits)
	}
	if result.AccessToken != "access-2" {
		t.Errorf("AccessToken: got %q, want access-2", result.AccessToken)
	}
}

func TestPollDeviceToken_428PendingThenSuccess(t *testing.T) {
	withTempHome(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(428) // Precondition Required — alternate pending code
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "authorization_pending"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "access-3", "refresh_token": "refresh-3", "email": "user@example.com", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	if _, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0); err != nil {
		t.Fatalf("pollDeviceToken: %v", err)
	}
	if hits != 2 {
		t.Errorf("expected exactly 2 polls, got %d", hits)
	}
}

// TestPollDeviceToken_SlowDownIncreasesInterval verifies the RFC 8628 section 3.5
// slow_down contract: the poll interval must increase (not just retry at
// the same cadence) once the gateway signals slow_down, and the flow must
// still complete successfully afterward.
func TestPollDeviceToken_SlowDownIncreasesInterval(t *testing.T) {
	withTempHome(t)

	orig := slowDownIncrementSecs
	slowDownIncrementSecs = 1 // real but small, so the test doesn't sleep the production 5s
	t.Cleanup(func() { slowDownIncrementSecs = orig })

	var hits int32
	var pollTimes []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollTimes = append(pollTimes, time.Now())
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "slow_down"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "access-slow", "refresh_token": "refresh-slow", "email": "user@example.com", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0)
	if err != nil {
		t.Fatalf("pollDeviceToken: %v", err)
	}
	if hits != 2 {
		t.Fatalf("expected exactly 2 polls (slow_down then success), got %d", hits)
	}
	if result.AccessToken != "access-slow" {
		t.Errorf("AccessToken: got %q, want access-slow", result.AccessToken)
	}
	if len(pollTimes) != 2 {
		t.Fatalf("expected 2 recorded poll times, got %d", len(pollTimes))
	}
	// intervalSecs starts at 0 (this test's base interval); slow_down must
	// add slowDownIncrementSecs before waiting for the retry, so the gap
	// between polls should reflect that added second, not an instant retry.
	gap := pollTimes[1].Sub(pollTimes[0])
	if gap < time.Duration(slowDownIncrementSecs)*time.Second {
		t.Errorf("slow_down did not increase the poll interval: gap=%s, want >= %ds", gap, slowDownIncrementSecs)
	}
}

func TestPollDeviceToken_ExpiredToken(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "expired_token", "error_description": "device code expired",
		})
	}))
	defer srv.Close()

	_, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected expired-token error, got: %v", err)
	}
}

func TestPollDeviceToken_GenericBadRequest(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "access_denied", "error_description": "user denied the request",
		})
	}))
	defer srv.Close()

	_, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "access_denied") || !strings.Contains(err.Error(), "user denied the request") {
		t.Errorf("expected error/description to surface, got: %v", err)
	}
}

func TestPollDeviceToken_UnexpectedStatus(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected status code to surface, got: %v", err)
	}
}

func TestPollDeviceToken_RetriesTransientFailuresThenSucceeds(t *testing.T) {
	withTempHome(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch hits.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "email": "user@example.com", "expires_in": 3600})
		}
	}))
	defer srv.Close()

	result, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0)
	if err != nil || result.Email != "user@example.com" || hits.Load() != 3 {
		t.Fatalf("result = %+v, err = %v, hits = %d", result, err, hits.Load())
	}
}

func TestPollDeviceToken_GivesUpAfterRepeatedNetworkErrors(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more

	_, err := pollDeviceToken(context.Background(), url, "device-code-1", 0)
	if err == nil || !strings.Contains(err.Error(), "poll device token") {
		t.Fatalf("err = %v, want the network error after the retries", err)
	}
}

func TestPollDeviceToken_RefusalIsNotRetried(t *testing.T) {
	withTempHome(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer srv.Close()
	if _, err := pollDeviceToken(context.Background(), srv.URL, "device-code-1", 0); err == nil || hits.Load() != 1 {
		t.Fatalf("err = %v, hits = %d, want one request and an error", err, hits.Load())
	}
}

func TestPollDeviceToken_ContextCancelledWhilePending(t *testing.T) {
	withTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "authorization_pending"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	// Large interval so the test's ctx deadline fires while pollDeviceToken
	// is waiting inside waitOrDone, not between polls.
	_, err := pollDeviceToken(ctx, srv.URL, "device-code-1", 5)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}
