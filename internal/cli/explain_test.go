package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func riskFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "risk-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type riskServer struct {
	*httptest.Server
	requests      int
	lastIfNoneHit string
}

// newRiskServer serves the recorded GET /api/catalog/risk payload with an ETag and honours If-None-Match.
func newRiskServer(t *testing.T) *riskServer {
	t.Helper()
	var cat riskCatalog
	if err := json.Unmarshal(riskFixture(t), &cat); err != nil {
		t.Fatal(err)
	}
	rs := &riskServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.requests++
		rs.lastIfNoneHit = r.Header.Get("If-None-Match")
		if r.URL.Path != "/api/catalog/risk" || r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		etag := `"` + cat.Version + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(riskFixture(t))
	}))
	t.Cleanup(rs.Close)
	return rs
}

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATEWAY_API_KEY", "key")
	t.Setenv("GATEWAY_URL", "")
	SetOffline(false)
	t.Cleanup(func() { SetOffline(false) })
}

func runExplain(t *testing.T, args ...string) explainResult {
	t.Helper()
	cmd := ExplainCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var res explainResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	return res
}

// Every row the endpoint serves must produce the same class in the CLI, with the decision derived from it.
func TestExplainMatchesEndpointForEveryRow(t *testing.T) {
	isolate(t)
	srv := newRiskServer(t)
	var served riskCatalog
	if err := json.Unmarshal(riskFixture(t), &served); err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	for _, row := range served.Tools {
		got := runExplain(t, "call", row.Service, row.Tool, "--json", "--gateway-url", srv.URL)
		want := "ask"
		if row.Risk == "read" {
			want = "allow"
		}
		if got.Risk != row.Risk || got.Decision != want || got.Service != row.Service || got.Tool != row.Tool {
			t.Errorf("%s.%s: got %+v, endpoint says risk %s (want decision %s)", row.Service, row.Tool, got, row.Risk, want)
		}
		classes[row.Risk] = true
	}
	for _, class := range []string{"read", "write", "destructive", "outward"} {
		if !classes[class] {
			t.Errorf("fixture has no %s tool", class)
		}
	}
}

func TestExplainUnknownToolAsks(t *testing.T) {
	isolate(t)
	srv := newRiskServer(t)
	got := runExplain(t, "call", "nosuch", "tool", "--json", "--gateway-url", srv.URL)
	if got.Risk != "unknown" || got.Decision != "ask" {
		t.Errorf("got %+v", got)
	}
}

func TestExplainFlagsMayPrecedeArguments(t *testing.T) {
	isolate(t)
	srv := newRiskServer(t)
	got := runExplain(t, "call", "--json", "--gateway-url", srv.URL, "superfaktura", "delete_invoice")
	if got.Service != "superfaktura" || got.Tool != "delete_invoice" || got.Risk != "destructive" || got.Decision != "ask" {
		t.Errorf("got %+v", got)
	}
}

func TestExplainRevalidatesWithETagAndFallsBackToCache(t *testing.T) {
	isolate(t)
	srv := newRiskServer(t)
	runExplain(t, "call", "resend", "send_email", "--json", "--gateway-url", srv.URL)
	if srv.lastIfNoneHit != "" {
		t.Errorf("first request sent If-None-Match %q", srv.lastIfNoneHit)
	}
	got := runExplain(t, "call", "resend", "send_email", "--json", "--gateway-url", srv.URL)
	if srv.lastIfNoneHit == "" || got.Risk != "outward" {
		t.Errorf("second request: If-None-Match %q, got %+v", srv.lastIfNoneHit, got)
	}

	srv.Close()
	if got := runExplain(t, "call", "resend", "send_email", "--json", "--gateway-url", srv.URL); got.Risk != "outward" {
		t.Errorf("unreachable gateway should use the cache, got %+v", got)
	}

	SetOffline(true)
	if got := runExplain(t, "call", "superfaktura", "list_invoices", "--json"); got.Decision != "allow" {
		t.Errorf("offline should use the cache, got %+v", got)
	}
}

func TestExplainWithoutCatalogAsks(t *testing.T) {
	isolate(t)
	SetOffline(true)
	if got := runExplain(t, "call", "superfaktura", "list_invoices", "--json"); got.Risk != "unknown" || got.Decision != "ask" {
		t.Errorf("got %+v", got)
	}
}

func TestExplainTextOutput(t *testing.T) {
	isolate(t)
	SetOffline(true)
	cmd := ExplainCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"call", "a", "b"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a b: unknown -> ask\n" {
		t.Errorf("got %q", out.String())
	}
}

func TestDecisionForRisk(t *testing.T) {
	for risk, want := range map[string]string{"read": "allow", "write": "ask", "destructive": "ask", "outward": "ask", "unknown": "ask", "": "ask"} {
		if got := decisionForRisk(risk); got != want {
			t.Errorf("%q: got %s want %s", risk, got, want)
		}
	}
}
