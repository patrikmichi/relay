package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrikmichi/zuano/internal/client"
)

// stepUpGateway answers /act with a step-up challenge until the retry carries
// the id of a completed one, and serves the step-up status in between.
type stepUpGateway struct {
	statuses   []string
	linkHost   string // overrides the host of the step-up link
	polls      atomic.Int32
	actCalls   atomic.Int32
	retryID    atomic.Value
	retryBody  atomic.Value
	statusPath atomic.Value
}

func newStepUpGateway(t *testing.T, g *stepUpGateway) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/act":
			g.actCalls.Add(1)
			if id := r.Header.Get("X-Step-Up"); id != "" {
				g.retryID.Store(id)
				b, _ := io.ReadAll(r.Body)
				g.retryBody.Store(string(b))
				_, _ = io.WriteString(w, "acted")
				return
			}
			host := "http://" + r.Host
			if g.linkHost != "" {
				host = g.linkHost
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_id":"su_1","step_up_url":"`+host+`/stepup/su_1"}`)
		case strings.HasPrefix(r.URL.Path, "/api/stepup/"):
			g.statusPath.Store(r.URL.Path)
			i := min(int(g.polls.Add(1))-1, len(g.statuses)-1)
			_, _ = io.WriteString(w, `{"status":"`+g.statuses[i]+`"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	return srv
}

func stepUpEnv(t *testing.T, interactive bool) (*bytes.Buffer, *[]string) {
	t.Helper()
	var out bytes.Buffer
	oldOut, oldPoll, oldInteractive, oldTimeout := stepUpOut, stepUpPollInterval, stepUpInteractive, stepUpTimeout
	stepUpOut, stepUpPollInterval, stepUpInteractive = &out, time.Millisecond, func() bool { return interactive }
	t.Cleanup(func() {
		stepUpOut, stepUpPollInterval, stepUpInteractive, stepUpTimeout = oldOut, oldPoll, oldInteractive, oldTimeout
	})
	return &out, stubBrowserWith(t, nil)
}

func resolveForStepUp(t *testing.T, srv *httptest.Server) *client.Client {
	t.Helper()
	c, err := client.Resolve(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStepUp_OpensThePageWaitsForThePasskeyAndRetriesTheRequest(t *testing.T) {
	g := &stepUpGateway{statuses: []string{"pending", "pending", "approved"}}
	srv := newStepUpGateway(t, g)
	out, opened := stepUpEnv(t, true)

	resp, err := resolveForStepUp(t, srv).Post("/act", "application/json", strings.NewReader(`{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "acted" {
		t.Fatalf("body = %q", body)
	}
	if len(*opened) != 1 || (*opened)[0] != srv.URL+"/stepup/su_1" {
		t.Errorf("opened %v", *opened)
	}
	if g.retryID.Load() != "su_1" || g.retryBody.Load() != `{"n":1}` || g.actCalls.Load() != 2 || g.polls.Load() != 3 {
		t.Errorf("retry id %v body %v act calls %d polls %d", g.retryID.Load(), g.retryBody.Load(), g.actCalls.Load(), g.polls.Load())
	}
	if g.statusPath.Load() != "/api/stepup/su_1" || !strings.Contains(out.String(), "passkey") {
		t.Errorf("status path %v, output %q", g.statusPath.Load(), out.String())
	}
}

func TestStepUp_PrintsTheLinkAndAQRCodeWhenTheBrowserFails(t *testing.T) {
	g := &stepUpGateway{statuses: []string{"approved"}}
	srv := newStepUpGateway(t, g)
	out, _ := stepUpEnv(t, true)
	stubBrowserWith(t, io.ErrClosedPipe)

	resp, err := resolveForStepUp(t, srv).Get("/act")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(out.String(), srv.URL+"/stepup/su_1") || !strings.Contains(out.String(), "\x1b[30;47m") {
		t.Errorf("output lacks the link or the QR code:\n%s", out)
	}
}

func TestStepUp_WithoutAPersonStopsWithExit14AndTheLink(t *testing.T) {
	g := &stepUpGateway{statuses: []string{"approved"}}
	srv := newStepUpGateway(t, g)
	_, opened := stepUpEnv(t, false)

	_, err := resolveForStepUp(t, srv).Get("/act")
	if got := ExitCode(err); got != 14 {
		t.Fatalf("exit = %d (%v), want 14", got, err)
	}
	if !strings.Contains(err.Error(), srv.URL+"/stepup/su_1") {
		t.Errorf("error %q lacks the link for the human", err)
	}
	if len(*opened) != 0 || g.polls.Load() != 0 || g.actCalls.Load() != 1 {
		t.Errorf("opened %v, polls %d, act calls %d: nothing may be opened, polled or retried", *opened, g.polls.Load(), g.actCalls.Load())
	}
}

func TestStepUp_RefusesALinkOutsideTheGateway(t *testing.T) {
	g := &stepUpGateway{statuses: []string{"approved"}, linkHost: "https://evil.example"}
	srv := newStepUpGateway(t, g)
	_, opened := stepUpEnv(t, true)

	_, err := resolveForStepUp(t, srv).Get("/act")
	if err == nil || !strings.Contains(err.Error(), "refusing to open it") {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 0 || g.polls.Load() != 0 {
		t.Errorf("opened %v, polls %d", *opened, g.polls.Load())
	}
}

func TestStepUp_DeclinedAndExpiredAreErrors(t *testing.T) {
	for status, want := range map[string]string{"denied": "declined", "expired": "expired"} {
		g := &stepUpGateway{statuses: []string{status}}
		srv := newStepUpGateway(t, g)
		stepUpEnv(t, true)
		_, err := resolveForStepUp(t, srv).Get("/act")
		if err == nil || !strings.Contains(err.Error(), want) || g.actCalls.Load() != 1 {
			t.Errorf("%s: err = %v after %d act calls", status, err, g.actCalls.Load())
		}
	}
}

func TestStepUp_GivesUpAfterTheTimeout(t *testing.T) {
	g := &stepUpGateway{statuses: []string{"pending"}}
	srv := newStepUpGateway(t, g)
	stepUpEnv(t, true)
	stepUpTimeout = 20 * time.Millisecond
	_, err := resolveForStepUp(t, srv).Get("/act")
	if err == nil || !strings.Contains(err.Error(), "no confirmation within") {
		t.Fatalf("err = %v", err)
	}
}

func TestStepUp_RelayCallExitsWith14WhenNobodyIsAtTheKeyboard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"step_up_required","step_up_id":"su_9","step_up_url":"http://`+r.Host+`/stepup/su_9"}`)
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	stepUpEnv(t, false)

	_, err := runCommand(t, CallCmd(), "stripe", "create_refund")
	if got := ExitCode(err); got != 14 {
		t.Fatalf("exit = %d (%v), want 14", got, err)
	}
	if err == nil || !strings.Contains(err.Error(), "/stepup/su_9") {
		t.Errorf("err = %v, want the link", err)
	}
}
