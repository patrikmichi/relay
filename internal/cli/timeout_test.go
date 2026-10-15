package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrikmichi/relay/internal/client"
)

func withTimeoutFlag(t *testing.T, d time.Duration) {
	t.Helper()
	if err := SetTimeoutFlag(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resetTimeoutFlag)
}

func TestRequestLimit_Defaults(t *testing.T) {
	t.Setenv(timeoutEnvVar, "")
	cases := map[requestKind]time.Duration{
		controlRequest:  30 * time.Second,
		toolCallRequest: 150 * time.Second,
		transferRequest: 150 * time.Second,
	}
	for kind, want := range cases {
		got, _, err := requestLimit(kind)
		if err != nil || got != want {
			t.Errorf("requestLimit(%d) = %s, %v; want %s", kind, got, err, want)
		}
	}
}

func TestRequestLimit_EnvOverridesDefaults(t *testing.T) {
	t.Setenv(timeoutEnvVar, "5m")
	for _, kind := range []requestKind{controlRequest, toolCallRequest, transferRequest} {
		got, source, err := requestLimit(kind)
		if err != nil || got != 5*time.Minute || !strings.Contains(source, timeoutEnvVar) {
			t.Errorf("requestLimit(%d) = %s, %q, %v; want 5m from %s", kind, got, source, err, timeoutEnvVar)
		}
	}
}

func TestRequestLimit_FlagOverridesEnv(t *testing.T) {
	t.Setenv(timeoutEnvVar, "5m")
	withTimeoutFlag(t, 7*time.Second)
	got, source, err := requestLimit(toolCallRequest)
	if err != nil || got != 7*time.Second || !strings.Contains(source, "--timeout") {
		t.Fatalf("requestLimit = %s, %q, %v; want 7s from --timeout", got, source, err)
	}
}

func TestRequestLimit_InvalidEnv(t *testing.T) {
	for _, raw := range []string{"soon", "-5s", "10"} {
		t.Setenv(timeoutEnvVar, raw)
		if _, _, err := requestLimit(controlRequest); err == nil || !strings.Contains(err.Error(), timeoutEnvVar) {
			t.Errorf("%s=%q: err = %v, want an error naming %s", timeoutEnvVar, raw, err, timeoutEnvVar)
		}
	}
}

func TestSetTimeoutFlag_RejectsNegative(t *testing.T) {
	t.Cleanup(resetTimeoutFlag)
	if err := SetTimeoutFlag(-time.Second); err == nil {
		t.Fatal("expected an error for a negative --timeout")
	}
}

func TestRequestContext_ZeroMeansNoDeadline(t *testing.T) {
	t.Setenv(timeoutEnvVar, "0")
	ctx, cancel, err := requestContext(context.Background(), toolCallRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("RELAY_TIMEOUT=0 must leave the request without a deadline")
	}
}

func slowToolServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		case <-release:
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"done"}]}}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

func TestCallTool_OutlivesControlLimit(t *testing.T) {
	t.Setenv(timeoutEnvVar, "")
	srv := slowToolServer(t, 200*time.Millisecond)
	var out bytes.Buffer
	if err := callToolWithContext(context.Background(), client.New(srv.URL, "tok"), "demo", "slow", nil, false, &out); err != nil {
		t.Fatalf("slow tool call failed under the default limit: %v", err)
	}
	if !strings.Contains(out.String(), "done") {
		t.Fatalf("output = %q, want the tool result", out.String())
	}
}

func TestCallTool_TimeoutNamesFlagAndExitsFour(t *testing.T) {
	withTimeoutFlag(t, 50*time.Millisecond)
	srv := slowToolServer(t, 2*time.Second)

	err := callToolWithContext(context.Background(), client.New(srv.URL, "tok"), "demo", "slow", nil, false, &bytes.Buffer{})
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *TimeoutError", err)
	}
	if te.Limit != 50*time.Millisecond || !strings.Contains(err.Error(), "--timeout") {
		t.Fatalf("message %q must name the --timeout limit of 50ms", err)
	}
	if code := ExitCode(fmt.Errorf("wrapped: %w", err)); code != 4 {
		t.Fatalf("ExitCode = %d, want 4", code)
	}
}

func TestCallTool_EnvTimeoutNamedInMessage(t *testing.T) {
	t.Setenv(timeoutEnvVar, "50ms")
	srv := slowToolServer(t, 2*time.Second)
	err := callToolWithContext(context.Background(), client.New(srv.URL, "tok"), "demo", "slow", nil, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "set by "+timeoutEnvVar) {
		t.Fatalf("err = %v, want a timeout naming %s", err, timeoutEnvVar)
	}
}

func TestCallTool_ParentCancelIsNotATimeout(t *testing.T) {
	t.Setenv(timeoutEnvVar, "")
	srv := slowToolServer(t, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := callToolWithContext(ctx, client.New(srv.URL, "tok"), "demo", "slow", nil, false, &bytes.Buffer{})
	var te *TimeoutError
	if err == nil || errors.As(err, &te) {
		t.Fatalf("err = %v, want a cancellation error that is not a *TimeoutError", err)
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("ExitCode = %d, want 1 for a cancelled call", code)
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(errors.New("boom")) != 1 {
		t.Fatal("ordinary errors exit 1")
	}
	if ExitCode(&TimeoutError{Limit: time.Second, Source: "set by --timeout"}) != 4 {
		t.Fatal("timeouts exit 4")
	}
}

func TestCommandDoer_TimeoutNamesTransferLimit(t *testing.T) {
	withTimeoutFlag(t, 50*time.Millisecond)
	srv := slowToolServer(t, 2*time.Second)
	d := commandDoer{base: client.New(srv.URL, "tok"), ctx: context.Background()}
	_, err := d.Get("/api/marketplace/manifest")
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *TimeoutError", err)
	}
}
