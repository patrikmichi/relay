package client

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewULID_EncodesTimeAndEntropy(t *testing.T) {
	at := time.UnixMilli(1760616000000)
	a, err := newULID(at, bytes.NewReader(make([]byte, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 26 || !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(a) {
		t.Fatalf("ulid = %q", a)
	}
	b, _ := newULID(at.Add(time.Second), bytes.NewReader(make([]byte, 10)))
	if a >= b {
		t.Errorf("ids do not sort by time: %s %s", a, b)
	}
	c, _ := newULID(at, bytes.NewReader([]byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 255}))
	if !strings.HasSuffix(c, "ZZZZZZZZZZZZZZZZ") || a[:10] != c[:10] {
		t.Errorf("entropy is not the last 16 characters: %s %s", a, c)
	}
	if id := NewCorrelationID(); !strings.HasPrefix(id, "cli_") || len(id) != 30 {
		t.Errorf("correlation id = %q", id)
	}
}

func TestGateway_SendsACorrelationIDAndRecordsTheGatewaysOwn(t *testing.T) {
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get(RequestIDHeader)
		w.Header().Set(RequestIDHeader, "req_01TEST")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var debug bytes.Buffer
	SetDebug(&debug)
	defer SetDebug(nil)

	resp, err := New(srv.URL, "tok").Get("/api/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(sent, "cli_") {
		t.Errorf("sent X-Request-Id = %q", sent)
	}
	if LastRequestID() != "req_01TEST" || RequestIDNote() != "; request id: req_01TEST" {
		t.Errorf("last = %q, note = %q", LastRequestID(), RequestIDNote())
	}
	if !strings.Contains(debug.String(), "GET /api/x -> 500, request id req_01TEST") {
		t.Errorf("debug output = %q", debug.String())
	}
}

func TestRefusals_CarryTheRequestID(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
		exit   int
	}{
		"plan quota":  {402, `{"error":"quota_exceeded","message":"used up"}`, "used up (quota_exceeded); retry after 7s; request id: req_9", ExitUsageExhausted},
		"rate limit":  {429, `{"error":"rate_limited","message":"slow down"}`, "slow down; retry after 7s; request id: req_9", ExitRateLimited},
		"metered cap": {429, `{"error":"quota_exceeded","message":"cap","metric":"calls"}`, "cap (metric calls); request id: req_9", ExitUsageExhausted},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(RequestIDHeader, "req_9")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.URL, "tok").Get("/api/x")
			coded, ok := err.(interface{ ExitCode() int })
			if err == nil || !ok || coded.ExitCode() != tc.exit {
				t.Fatalf("err = %v (%T), want exit %d", err, err, tc.exit)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}
