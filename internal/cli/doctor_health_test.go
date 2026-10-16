package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func healthGateway(t *testing.T, healthStatus int, healthBody, versionBody string) {
	t.Helper()
	setupDoctor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("%s was sent credentials", r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/health":
			w.WriteHeader(healthStatus)
			fmt.Fprint(w, healthBody)
		case "/api/version":
			fmt.Fprint(w, versionBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZUANO_URL", srv.URL)
}

func TestDoctor_ShowsHealthAndTheStatusPage(t *testing.T) {
	healthGateway(t, 200, `{"degraded":["blob","langfuse"]}`, `{"version":"1","statusPageUrl":"https://status.example.com"}`)
	output, _ := runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "health"); !c.OK || c.Detail != "ready, but degraded: blob, langfuse" {
		t.Errorf("health: %+v", c)
	}
	if c := doctorCheckNamed(t, output, "status-page"); !c.OK || c.Detail != "https://status.example.com" {
		t.Errorf("status-page: %+v", c)
	}
}

func TestDoctor_ANotReadyGatewayFailsAndAnUnsafeStatusURLIsIgnored(t *testing.T) {
	healthGateway(t, 503, `{}`, `{"statusPageUrl":"javascript:alert(1)"}`)
	output, err := runDoctorJSON(t)
	if err == nil {
		t.Error("doctor passed with a gateway that is not ready")
	}
	if c := doctorCheckNamed(t, output, "health"); c.OK || !strings.Contains(c.Detail, "HTTP 503") {
		t.Errorf("health: %+v", c)
	}
	if c := doctorCheckNamed(t, output, "status-page"); !strings.Contains(c.Detail, "none published") {
		t.Errorf("status-page: %+v", c)
	}
}
