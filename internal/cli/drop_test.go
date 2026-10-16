package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, uri string
	body        map[string]any
}

type dropGW struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []recorded
	puts     map[string]string
	routes   map[string]func(w http.ResponseWriter, body map[string]any)
}

func (g *dropGW) fill(s string) string {
	return strings.NewReplacer("https://store.example", g.srv.URL+"/store", "https://docs--acme.sites.example", g.srv.URL+"/site").Replace(s)
}

func (g *dropGW) calls(method, uri string) []recorded {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []recorded
	for _, r := range g.requests {
		if r.method == method && r.uri == uri {
			out = append(out, r)
		}
	}
	return out
}

func newDropGW(t *testing.T, routes map[string]func(w http.ResponseWriter, body map[string]any)) *dropGW {
	t.Helper()
	g := &dropGW{puts: map[string]string{}, routes: routes}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/store/") {
			g.mu.Lock()
			g.puts[r.URL.Path] = string(raw)
			g.mu.Unlock()
			if r.Header.Get("Authorization") != "" {
				t.Error("the store was sent the gateway credential")
			}
			return
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		g.mu.Lock()
		g.requests = append(g.requests, recorded{r.Method, r.URL.RequestURI(), body})
		h, ok := g.routes[r.Method+" "+r.URL.RequestURI()]
		g.mu.Unlock()
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h(w, body)
	}))
	t.Cleanup(g.srv.Close)
	withAPIKeyGateway(t, g.srv)
	t.Setenv("ZUANO_AGENT", "")
	dropPollInterval = time.Millisecond
	t.Cleanup(func() { dropPollInterval = time.Second })
	return g
}

func writeSite(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fixtureReply(t *testing.T, g func() *dropGW, status int, name string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, g().fill(contractFixture(t, name)))
	}
}

func TestDropPublish_UploadsOnlyWhatIsMissingFinalizesAndPrintsTheAddress(t *testing.T) {
	var g *dropGW
	polls := 0
	g = newDropGW(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/artifacts/uploads":                         fixtureReply(t, func() *dropGW { return g }, 200, "artifact-upload-create.json"),
		"POST /api/artifacts/uploads/aup_x1y2z3a4b5/finalize": reply(200, `{"release_id":"arl_r1e2l3e4a5","status":"preparing"}`),
		"GET /api/artifacts/releases/arl_r1e2l3e4a5": func(w http.ResponseWriter, _ map[string]any) {
			polls++
			doc := g.fill(contractFixture(t, "artifact-release-ready.json"))
			if polls == 1 {
				doc = strings.Replace(doc, `"state":"ready"`, `"state":"preparing"`, 1)
			}
			_, _ = io.WriteString(w, doc)
		},
	})
	root := writeSite(t, map[string]string{"index.html": "<h1>hi</h1>", "css/site.css": "body{}"})
	out, err := runCommand(t, DropCmd(), "publish", root, "--site", "docs", "--title", "Team docs", "--spa")
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	for _, want := range []string{"Published docs (arl_r1e2l3e4a5, 2 files)", g.srv.URL + "/site/", "Visible to: organisation", "warning: index.html: img/logo.png is not in the release (line 4)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	create := g.calls("POST", "/api/artifacts/uploads")[0].body
	site := create["site"].(map[string]any)
	if site["slug"] != "docs" || site["title"] != "Team docs" || site["routing"] != "spa" || !strings.HasPrefix(create["idempotency_key"].(string), "cli_") {
		t.Errorf("create body = %v", create)
	}
	files := create["files"].([]any)
	if len(files) != 2 || files[0].(map[string]any)["path"] != "css/site.css" || len(files[0].(map[string]any)["sha256"].(string)) != 64 {
		t.Errorf("files = %v", files)
	}
	if len(g.puts) != 1 || g.puts["/store/artifacts/org_1/staging/aup_x1y2z3a4b5/f1"] != "<h1>hi</h1>" {
		t.Errorf("uploads = %v", g.puts)
	}
	if fin := g.calls("POST", "/api/artifacts/uploads/aup_x1y2z3a4b5/finalize")[0].body; fin["activate"] != true {
		t.Errorf("finalize body = %v", fin)
	}
	if polls != 2 {
		t.Errorf("polled %d times", polls)
	}
}

func TestDropPublish_NoActivateJSONAndAFailedEntry(t *testing.T) {
	var g *dropGW
	failed := false
	g = newDropGW(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/artifacts/uploads":                         reply(200, `{"upload_id":"aup_x1y2z3a4b5","files":[{"path":"a.md","file_key":"f1","status":"present"},{"path":"b.md","file_key":"f2","status":"present"}]}`),
		"POST /api/artifacts/uploads/aup_x1y2z3a4b5/finalize": reply(200, `{"release_id":"arl_r1e2l3e4a5"}`),
		"GET /api/artifacts/releases/arl_r1e2l3e4a5": func(w http.ResponseWriter, _ map[string]any) {
			name := "artifact-release-ready.json"
			if failed {
				name = "artifact-release-entry-required.json"
			}
			_, _ = io.WriteString(w, g.fill(contractFixture(t, name)))
		},
	})
	root := writeSite(t, map[string]string{"a.md": "# a", "b.md": "# b"})
	out, err := runCommand(t, DropCmd(), "publish", root, "--no-activate")
	if err != nil || !strings.Contains(out, "is ready but not live") || !strings.Contains(out, "Preview: https://preview--acme.sites.example/_drop/preview/arl_r1e2l3e4a5/") {
		t.Fatalf("no-activate: %q, %v", out, err)
	}
	if fin := g.calls("POST", "/api/artifacts/uploads/aup_x1y2z3a4b5/finalize")[0].body; fin["activate"] != false {
		t.Errorf("finalize body = %v", fin)
	}
	wantSlug, _ := slugFrom(filepath.Base(root))
	if slug := g.calls("POST", "/api/artifacts/uploads")[0].body["site"].(map[string]any)["slug"]; slug != wantSlug {
		t.Errorf("derived slug = %v, want %v", slug, wantSlug)
	}

	out, err = runCommand(t, DropCmd(), "publish", root, "--site", "docs", "--json")
	if err != nil || !strings.Contains(out, `"state":"ready"`) {
		t.Fatalf("json: %q, %v", out, err)
	}
	failed = true
	_, err = runCommand(t, DropCmd(), "publish", root, "--site", "docs")
	if err == nil || !strings.Contains(err.Error(), "several pages could be the entry: a.md, b.md. Choose one with --entry") {
		t.Fatalf("entry err = %v", err)
	}
}

func TestDropPublish_ARefusalHeldForApprovalAndBadInputStopEarly(t *testing.T) {
	g := newDropGW(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /api/artifacts/uploads": reply(202, pendingApprovalJSON2),
	})
	root := writeSite(t, map[string]string{"index.html": "x"})
	out, err := runCommand(t, DropCmd(), "publish", root, "--site", "docs")
	if got := exitCodeOf(err); got != 12 || !strings.Contains(out, "apr_9") {
		t.Fatalf("held: exit %d (%v)\n%s", got, err, out)
	}
	bad := writeSite(t, map[string]string{"index.html": "x", ".env": "TOKEN=1"})
	before := len(g.requests)
	if _, err := runCommand(t, DropCmd(), "publish", bad, "--site", "docs"); err == nil || !strings.Contains(err.Error(), ".env (environment file)") {
		t.Errorf("credential err = %v", err)
	}
	for name, args := range map[string][]string{
		"site name":  {"publish", root, "--site", "Bad Name"},
		"short name": {"publish", root, "--site", "ab"},
		"entry":      {"publish", root, "--site", "docs", "--entry", "../x.html"},
	} {
		if _, err := runCommand(t, DropCmd(), args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if len(g.requests) != before {
		t.Errorf("requests were sent for input that is invalid: %d", len(g.requests)-before)
	}
}

func TestSlugFrom(t *testing.T) {
	for in, want := range map[string]string{
		"My Report v2.md": "my-report-v2",
		"dist":            "dist",
		"__Docs__Site__":  "docs-site",
		"a-very-long-folder-name-that-goes-on-and-on": "a-very-long-folder-name-that-g",
	} {
		got, err := slugFrom(in)
		if err != nil || got != want {
			t.Errorf("slugFrom(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ab", "!!!", "x.md"} {
		if _, err := slugFrom(bad); err == nil {
			t.Errorf("slugFrom(%q) succeeded", bad)
		}
	}
}

func TestDrop_ListReleasesRollbackOpenDeleteRestore(t *testing.T) {
	var g *dropGW
	g = newDropGW(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/artifacts/sites":                          fixtureReply(t, func() *dropGW { return g }, 200, "artifact-sites.json"),
		"GET /api/artifacts/sites?status=deleted":           fixtureReply(t, func() *dropGW { return g }, 200, "artifact-sites.json"),
		"GET /api/artifacts/sites/ast_s1i2t3e4a5/releases":  reply(200, contractFixture(t, "artifact-releases.json")),
		"POST /api/artifacts/sites/ast_s1i2t3e4a5/activate": reply(200, `{"ok":true}`),
		"GET /api/artifacts/releases/arl_r1e2l3e4a5":        fixtureReply(t, func() *dropGW { return g }, 200, "artifact-release-ready.json"),
		"DELETE /api/artifacts/sites/ast_s1i2t3e4a5":        reply(204, ``),
		"POST /api/artifacts/sites/ast_s1i2t3e4a5/restore":  reply(200, `{"ok":true}`),
	})
	out, err := runCommand(t, DropCmd(), "list")
	if err != nil || !strings.Contains(out, "docs") || !strings.Contains(out, "Team docs") || !strings.Contains(out, "organisation") {
		t.Fatalf("list: %q, %v", out, err)
	}
	out, err = runCommand(t, DropCmd(), "releases", "docs")
	if err != nil || !strings.Contains(out, "arl_r1e2l3e4a5") || !strings.Contains(out, "live") || strings.Count(out, "live") != 1 {
		t.Fatalf("releases: %q, %v", out, err)
	}
	if out, err = runCommand(t, DropCmd(), "rollback", "docs", "arl_o1l2d3e4r5"); err != nil || !strings.Contains(out, "is live again") {
		t.Fatalf("rollback: %q, %v", out, err)
	}
	body := g.calls("POST", "/api/artifacts/sites/ast_s1i2t3e4a5/activate")[0].body
	if body["release_id"] != "arl_o1l2d3e4r5" || body["expected_content_revision"] != float64(4) {
		t.Errorf("rollback body = %v", body)
	}

	opened := stubBrowserWith(t, nil)
	if out, err = runCommand(t, DropCmd(), "open", "docs", "--no-browser"); err != nil || strings.TrimSpace(out) != g.srv.URL+"/site/" || len(*opened) != 0 {
		t.Fatalf("open: %q, %v, %v", out, err, *opened)
	}
	if out, err = runCommand(t, DropCmd(), "open", "docs", "--preview", "arl_r1e2l3e4a5", "--no-browser"); err != nil || strings.TrimSpace(out) != "https://preview--acme.sites.example/_drop/preview/arl_r1e2l3e4a5/" {
		t.Fatalf("open preview: %q, %v", out, err)
	}

	if _, err = runCommand(t, DropCmd(), "delete", "docs"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("delete without a terminal err = %v", err)
	}
	if out, err = runWithInput(t, DropCmd(), "n\n", "delete", "docs"); err == nil || strings.Contains(out, "Deleted") {
		t.Errorf("delete declined: %q, %v", out, err)
	}
	if len(g.calls("DELETE", "/api/artifacts/sites/ast_s1i2t3e4a5")) != 0 {
		t.Fatal("a site was deleted without confirmation")
	}
	if out, err = runCommand(t, DropCmd(), "delete", "docs", "--yes"); err != nil || !strings.Contains(out, "Deleted docs") {
		t.Fatalf("delete: %q, %v", out, err)
	}
	if out, err = runWithInput(t, DropCmd(), "y\n", "delete", "docs"); err != nil {
		t.Errorf("delete confirmed: %q, %v", out, err)
	}
	if out, err = runCommand(t, DropCmd(), "restore", "docs"); err != nil || !strings.Contains(out, "Restored docs") {
		t.Fatalf("restore: %q, %v", out, err)
	}
	if _, err = runCommand(t, DropCmd(), "releases", "nosuch"); err == nil || !strings.Contains(err.Error(), `no site "nosuch"`) {
		t.Errorf("unknown site err = %v", err)
	}
}

func TestDropShare_ShowsProposesAndAppliesAfterConfirmation(t *testing.T) {
	var g *dropGW
	g = newDropGW(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/artifacts/sites":                                     fixtureReply(t, func() *dropGW { return g }, 200, "artifact-sites.json"),
		"GET /api/artifacts/sites/ast_s1i2t3e4a5":                      fixtureReply(t, func() *dropGW { return g }, 200, "artifact-site-detail.json"),
		"PUT /api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=propose": reply(200, contractFixture(t, "artifact-share-proposal.json")),
		"PUT /api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=apply":   reply(200, `{"site":{}}`),
	})
	out, err := runCommand(t, DropCmd(), "share", "docs")
	if err != nil || !strings.Contains(out, "Public: off") || !strings.Contains(out, "owner    owner@example.com") || !strings.Contains(out, "group eng") {
		t.Fatalf("show: %q, %v", out, err)
	}

	out, err = runWithInput(t, DropCmd(), "y\n", "share", "docs", "--add", "bo@example.com", "--remove", "group:eng", "--public", "--expires", "7d")
	if err != nil {
		t.Fatalf("share: %v\n%s", err, out)
	}
	for _, want := range []string{"+ bo@example.com (viewer)", "- group eng", "Public access: off -> on", "Sharing of docs updated"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	propose := g.calls("PUT", "/api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=propose")[0].body
	grants := propose["grants"].([]any)
	if len(grants) != 3 || propose["expected_policy_revision"] != float64(2) {
		t.Fatalf("propose body = %v", propose)
	}
	pub := propose["public"].(map[string]any)
	exp, perr := time.Parse(time.RFC3339, pub["expires_at"].(string))
	if pub["enabled"] != true || perr != nil || time.Until(exp) < 6*24*time.Hour {
		t.Errorf("public = %v", pub)
	}
	var added map[string]any
	for _, g := range grants {
		if m := g.(map[string]any); m["principal_email"] == "bo@example.com" {
			added = m
		}
	}
	if added == nil || added["role"] != "viewer" || added["principal_type"] != "user" {
		t.Errorf("added grant = %v", added)
	}
	for _, gr := range grants {
		if gr.(map[string]any)["principal_type"] == "group" {
			t.Error("the removed group is still in the desired state")
		}
	}

	if _, err = runCommand(t, DropCmd(), "share", "docs", "--org"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("no terminal err = %v", err)
	}
	if len(g.calls("PUT", "/api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=apply")) != 1 {
		t.Errorf("apply was sent %d times", len(g.calls("PUT", "/api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=apply")))
	}
	applied := g.calls("PUT", "/api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=apply")[0].body
	if applied["expected_policy_revision"] != float64(2) {
		t.Errorf("apply body = %v", applied)
	}
}

func TestDropShare_PrivateAnEmptyDiffAndBadFlags(t *testing.T) {
	var g *dropGW
	empty := `{"diff":{"gain":[],"lose":[],"public":{"before":false,"after":false}},"policy_revision":2}`
	g = newDropGW(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /api/artifacts/sites":                                     fixtureReply(t, func() *dropGW { return g }, 200, "artifact-sites.json"),
		"GET /api/artifacts/sites/ast_s1i2t3e4a5":                      fixtureReply(t, func() *dropGW { return g }, 200, "artifact-site-detail.json"),
		"PUT /api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=propose": reply(200, empty),
	})
	out, err := runCommand(t, DropCmd(), "share", "docs", "--private", "--yes")
	if err != nil || !strings.Contains(out, "No change") {
		t.Fatalf("private: %q, %v", out, err)
	}
	grants := g.calls("PUT", "/api/artifacts/sites/ast_s1i2t3e4a5/sharing?mode=propose")[0].body["grants"].([]any)
	if len(grants) != 1 || grants[0].(map[string]any)["role"] != "owner" {
		t.Errorf("--private kept %v", grants)
	}
	for name, args := range map[string][]string{
		"expires without public": {"share", "docs", "--expires", "7d"},
		"remove nobody":          {"share", "docs", "--remove", "nobody@example.com"},
		"add not an email":       {"share", "docs", "--add", "ana"},
		"bad role":               {"share", "docs", "--org", "--role", "owner"},
		"bad expiry":             {"share", "docs", "--public", "--expires", "soon"},
	} {
		if _, err := runCommand(t, DropCmd(), args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseExpiry(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{"7d": "2026-10-23T12:00:00Z", "12h": "2026-10-17T00:00:00Z", "2w": "2026-10-30T12:00:00Z", "2026-12-31": "2026-12-31T00:00:00Z"} {
		if got, err := parseExpiry(in, now); err != nil || got != want {
			t.Errorf("parseExpiry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0d", "soon", "2020-01-01", "7y"} {
		if _, err := parseExpiry(bad, now); err == nil {
			t.Errorf("parseExpiry(%q) succeeded", bad)
		}
	}
}
