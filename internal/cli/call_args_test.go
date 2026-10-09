package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// relay call must find <service> <tool> wherever the flags sit.
func TestCallFindsServiceAndToolRegardlessOfFlagOrder(t *testing.T) {
	isolate(t)
	cases := map[string][]string{
		"flags after":    {"superfaktura", "delete_invoice", "--arg", "id=7"},
		"flags before":   {"--arg", "id=7", "superfaktura", "delete_invoice"},
		"flags between":  {"superfaktura", "--arg", "id=7", "delete_invoice"},
		"equals form":    {"--arg=id=7", "superfaktura", "delete_invoice"},
		"gateway first":  {"--gateway-url", "{url}", "--arg", "id=7", "superfaktura", "delete_invoice"},
		"gateway middle": {"superfaktura", "--gateway-url", "{url}", "delete_invoice", "--arg", "id=7"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var path string
			var rpc struct {
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &rpc)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
			}))
			defer srv.Close()

			full := append([]string{}, args...)
			hasGateway := false
			for i, a := range full {
				if a == "{url}" {
					full[i] = srv.URL
					hasGateway = true
				}
			}
			if !hasGateway {
				full = append(full, "--gateway-url", srv.URL)
			}
			cmd := CallCmd()
			cmd.SetArgs(full)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if path != "/api/superfaktura/mcp" || rpc.Params.Name != "delete_invoice" || rpc.Params.Arguments["id"] != float64(7) {
				t.Errorf("path %q, params %+v", path, rpc.Params)
			}
		})
	}
}

func TestCallRejectsMissingOrExtraPositionals(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"--arg", "a=1", "svc"}, {"svc", "tool", "extra", "--arg", "a=1"}} {
		cmd := CallCmd()
		cmd.SetArgs(args)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		if err := cmd.Execute(); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}
