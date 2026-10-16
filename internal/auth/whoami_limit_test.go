package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/client"
)

func TestWhoami_RefusesAnOversizedResponse(t *testing.T) {
	withTempHome(t)
	huge := `{"email":"` + strings.Repeat("a", client.MaxJSONBytes) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	_, err := Whoami(context.Background(), client.New(srv.URL, "static-key"), false)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Whoami = %v, want a size-limit error", err)
	}
}
