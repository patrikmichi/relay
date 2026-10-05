package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func subcommandNames(cmd *cobra.Command) []string {
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	return names
}

func TestParentCommands_ExposeExpectedSubcommands(t *testing.T) {
	cases := []struct {
		cmd  *cobra.Command
		want []string
	}{
		{SkillCmd(), []string{"diff", "install", "list", "migrate", "publish", "rollback", "scan", "score", "search", "uninstall"}},
		{AgentCmd(), []string{"diff", "install", "list", "migrate", "publish", "rollback", "scan", "uninstall"}},
		{MCPCmd(), []string{"inspect", "list", "publish"}},
		{PublishCmd(), []string{"status"}},
	}
	for _, tc := range cases {
		if got := subcommandNames(tc.cmd); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s subcommands = %v, want %v", tc.cmd.Name(), got, tc.want)
		}
	}
	if !strings.Contains(AgentCmd().Long, "claude") {
		t.Error("agent help must list supported providers")
	}
}

func TestAgentPublish_DryRunUsesAliasKind(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"agent.md":  "---\nname: reviewer\n---\nbody",
		"README.md": "readme",
		".env":      "TOKEN=x",
	})
	out, err := runCommand(t, AgentCmd(), "publish", dir, "--dry-run")
	if err != nil {
		t.Fatalf("agent publish --dry-run: %v", err)
	}
	for _, want := range []string{"type:    agent", "name:    reviewer", "bundle files (2)", "excluded files (1)", ".env (default-excluded secret-bearing filename (.env))", "no API call made"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestAliasPublish_RejectsContradictoryTypes(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"agent.md": "---\ntype: skill\nname: x\n---\n"})

	if _, err := runCommand(t, AgentCmd(), "publish", dir, "--dry-run", "--type", "skill"); err == nil || !strings.Contains(err.Error(), `--type="skill"`) {
		t.Errorf("expected flag contradiction, got %v", err)
	}
	if _, err := runCommand(t, AgentCmd(), "publish", dir, "--dry-run"); err == nil || !strings.Contains(err.Error(), `declares type "skill"`) {
		t.Errorf("expected frontmatter contradiction, got %v", err)
	}
}

func TestPublish_MissingPathIsReported(t *testing.T) {
	_, err := runCommand(t, PublishCmd(), filepath.Join(t.TempDir(), "absent"), "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "stat ") {
		t.Fatalf("expected stat error, got %v", err)
	}
}

func TestMcpPublish_DryRunFromFlagsNeverShowsAuthReference(t *testing.T) {
	readme := filepath.Join(t.TempDir(), "README.md")
	writeTree(t, filepath.Dir(readme), map[string]string{"README.md": "Billing MCP"})
	out, err := runCommand(t, MCPCmd(), "publish", readme, "--dry-run",
		"--name", "Billing", "--slug", "billing",
		"--transport", "http", "--endpoint", "https://mcp.example.com/rpc",
		"--source", "url", "--auth-type", "api_key", "--auth-scope", "org",
		"--auth-ref", "billing-service", "--tool", "list_invoices", "--tool", "get_invoice")
	if err != nil {
		t.Fatalf("mcp publish --dry-run: %v", err)
	}
	for _, want := range []string{"type:    mcp_server", "name:    Billing", "slug:    billing", `"hasAuthReference": true`, `"declaredToolCount": 2`, `"transport": "http"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "billing-service") {
		t.Error("dry-run must not print the credential reference")
	}
}

func TestMcpPublish_DescriptorFileIsReadAndFlagsOverride(t *testing.T) {
	dir := t.TempDir()
	descriptor := filepath.Join(dir, "server.json")
	writeTree(t, dir, map[string]string{"server.json": `{"name":"Files","slug":"files","version":"2.0.0","description":"File access","transport":"stdio-command","command":"files-mcp"}`})

	var gotFields map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFields = multipartFields(t, r)
		jsonHandler(http.StatusCreated, publishResponse{ResourceID: "res_1", VersionID: "ver_1", Semver: "2.0.0", State: "published"})(w, r)
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)

	out, err := runCommand(t, MCPCmd(), "publish", "--descriptor", descriptor, "--command", "files-mcp --stdio")
	if err != nil {
		t.Fatalf("mcp publish: %v", err)
	}
	if !strings.Contains(out, "Catalog URL: "+srv.URL+"/catalog/res_1") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if gotFields["type"] != "mcp_server" {
		t.Errorf("type field = %q", gotFields["type"])
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(gotFields["manifest"]), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if manifest["command"] != "files-mcp --stdio" || manifest["version"] != "2.0.0" || manifest["slug"] != "files" || manifest["description"] != "File access" {
		t.Errorf("unexpected manifest %v", manifest)
	}
	var sent mcpDescriptor
	if err := json.Unmarshal([]byte(gotFields["mcpServerDescriptor"]), &sent); err != nil {
		t.Fatalf("descriptor field: %v", err)
	}
	if sent.Command != "files-mcp --stdio" || sent.Transport != "stdio-command" {
		t.Errorf("flag override not applied: %+v", sent)
	}
}

func TestMcpPublish_RejectsBadDescriptors(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"unknown.json": `{"transport":"http","endpoint":"https://x.example.com","surprise":true}`,
		"two.json":     `{"transport":"http","endpoint":"https://x.example.com"} {}`,
		"noname.json":  `{"transport":"http","endpoint":"https://x.example.com"}`,
		"invalid.json": `{"transport":"carrier-pigeon","name":"x"}`,
	})
	cases := map[string]string{
		"unknown.json": "invalid MCP descriptor fields",
		"two.json":     "must contain one JSON object",
		"noname.json":  "MCP server name is required",
		"invalid.json": "MCP transport must be",
		"missing.json": "no such file or directory",
	}
	for file, want := range cases {
		t.Run(file, func(t *testing.T) {
			_, err := runCommand(t, MCPCmd(), "publish", "--dry-run", "--descriptor", filepath.Join(dir, file))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}

func TestMcpPublish_DescriptorSymlinkIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"real.json": `{"name":"x","transport":"http","endpoint":"https://x.example.com"}`})
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(filepath.Join(dir, "real.json"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := runCommand(t, MCPCmd(), "publish", "--dry-run", "--descriptor", link); err == nil || !strings.Contains(err.Error(), "read MCP descriptor") {
		t.Fatalf("expected the symlinked descriptor to be refused, got %v", err)
	}
}

func TestMcpPublish_RequiresAPathOrDescriptor(t *testing.T) {
	_, err := runCommand(t, MCPCmd(), "publish", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "publish path is required") {
		t.Fatalf("got %v", err)
	}
}

func multipartFields(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type: %v", err)
	}
	fields := map[string]string{}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return fields
		}
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		data, _ := io.ReadAll(part)
		fields[part.FormName()] = string(data)
	}
}

func tarNames(t *testing.T, bundle string) []string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(bundle))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			sort.Strings(names)
			return names
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, filepath.ToSlash(h.Name))
	}
}

func TestPublishCmd_UploadsBundleWithOverridesAndIncludes(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"SKILL.md":         "---\nname: demo\n---\n",
		"scripts/run.sh":   "echo hi",
		".env":             "A=1",
		"keys/server.pem":  "pem",
		".env.example":     "A=",
		"nested/notes.txt": "n",
	})

	var fields map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != publishAPIPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fields = multipartFields(t, r)
		jsonHandler(http.StatusCreated, publishResponse{ResourceID: "res_9", VersionID: "ver_9", Semver: "1.0.0", State: "pending_review"})(w, r)
	}))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)

	out, err := runCommand(t, SkillCmd(), "publish", dir, "--name", "Demo", "--slug", "demo-x", "--channel", "beta", "--include", ".env")
	if err != nil {
		t.Fatalf("skill publish: %v", err)
	}
	if !strings.Contains(out, "publishing 5 files, 1 excluded") || !strings.Contains(out, filepath.Join("keys", "server.pem")) {
		t.Errorf("unexpected progress output:\n%s", out)
	}
	if fields["name"] != "Demo" || fields["slug"] != "demo-x" || fields["channel"] != "beta" {
		t.Errorf("overrides not sent: %v", fields)
	}
	if _, ok := fields["type"]; ok {
		t.Error("type must only be sent when --type is given")
	}
	got := tarNames(t, fields["bundle"])
	want := []string{".env", ".env.example", "SKILL.md", "nested/notes.txt", "scripts/run.sh"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bundle entries = %v, want %v", got, want)
	}
}

func TestPublishCmd_FailsClosedWithoutGateway(t *testing.T) {
	withNoGateway(t)
	file := writeSkillFile(t, t.TempDir(), "SKILL.md")
	_, err := runCommand(t, PublishCmd(), file)
	if err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Fatalf("expected offline guidance, got %v", err)
	}
}

func TestHandlePublishResponse_RemainingStatuses(t *testing.T) {
	long := strings.Repeat("x", 300)
	cases := []struct {
		status int
		body   string
		hdr    http.Header
		want   string
	}{
		{http.StatusConflict, `{}`, nil, "retry (a version with this slug"},
		{http.StatusConflict, `{"error":{"message":"semver 1.0.0 taken"}}`, nil, "(semver 1.0.0 taken)"},
		{http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, http.Header{}, "rate limited — slow down;"},
		{http.StatusTooManyRequests, `not json`, http.Header{}, "rate limited; wait"},
		{http.StatusRequestEntityTooLarge, ``, nil, "bundle too large"},
		{http.StatusUnprocessableEntity, `plain text`, nil, "validation error (422): plain text"},
		{http.StatusUnprocessableEntity, `{"error":{"message":"bad manifest"}}`, nil, "validation error: bad manifest"},
		{http.StatusTeapot, long, nil, "publish failed (418): " + strings.Repeat("x", 200) + "..."},
		{http.StatusCreated, `{`, nil, "decode publish response"},
	}
	for _, tc := range cases {
		hdr := tc.hdr
		if hdr == nil {
			hdr = http.Header{}
		}
		err := handlePublishResponse(tc.status, hdr, []byte(tc.body), "https://g.example.com", io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: got %v, want %q", tc.status, err, tc.want)
		}
	}
}

func TestHandlePublishResponse_DefaultsChannelToStable(t *testing.T) {
	var out bytes.Buffer
	if err := handlePublishResponse(http.StatusOK, http.Header{}, []byte(`{"resourceId":"r","state":"published"}`), "https://g.example.com/", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Channel:     stable") || !strings.Contains(out.String(), "https://g.example.com/catalog/r") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}

func TestCatalogURL_WithoutAnyGatewayReturnsGuidance(t *testing.T) {
	withNoGateway(t)
	if got := catalogURL("res", ""); got != offlineGuidance {
		t.Errorf("catalogURL = %q", got)
	}
}

func TestPublishStatusCmd(t *testing.T) {
	score := 91
	cases := []struct {
		name    string
		args    []string
		states  []publishStatusResult
		wantOut []string
		wantErr string
	}{
		{
			name:    "published",
			args:    []string{"ver_1"},
			states:  []publishStatusResult{{VersionID: "ver_1", State: "published", AiScan: publishStatusAiScan{Status: "passed", TrustScore: &score}, Gates: []publishStatusGate{{Gate: "license", Status: "passed", RequiresAdminApproval: true}}}},
			wantOut: []string{"State:    published", "trust score: 91/100", "license", "(requires admin approval)"},
		},
		{
			name:    "changes requested",
			args:    []string{"ver_1"},
			states:  []publishStatusResult{{VersionID: "ver_1", State: "changes_requested"}},
			wantOut: []string{"trust score: n/a"},
			wantErr: "needs changes",
		},
		{
			name:    "watch",
			args:    []string{"ver_1", "--watch"},
			states:  []publishStatusResult{{VersionID: "ver_1", State: "scanning"}, {VersionID: "ver_1", State: "published"}},
			wantOut: []string{"State:    scanning", "State:    published"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFastPolling(t, 10)
			srv := sequenceStatusServer(t, tc.states)
			withAPIKeyGateway(t, srv)
			out, err := runCommand(t, publishStatusCmd(), tc.args...)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("status: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("got %v, want %q", err, tc.wantErr)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestPublishStatusCmd_Errors(t *testing.T) {
	withNoGateway(t)
	if _, err := runCommand(t, publishStatusCmd(), "ver_1"); err == nil || !strings.Contains(err.Error(), offlineGuidance) {
		t.Errorf("expected offline guidance, got %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }))
	t.Cleanup(srv.Close)
	withAPIKeyGateway(t, srv)
	if _, err := runCommand(t, publishStatusCmd(), "ver_1"); err == nil || !strings.Contains(err.Error(), "decode publish status response") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestFetchPublishStatus_FallsBackToRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("  no such version \n"))
	}))
	t.Cleanup(srv.Close)
	_, err := fetchPublishStatus(&publishStatusTestDoer{srv: srv}, "ver_x")
	if err == nil || !strings.Contains(err.Error(), "publish status failed (404): no such version") {
		t.Fatalf("got %v", err)
	}
}

func TestSecretExclusionReason(t *testing.T) {
	cases := map[string]bool{
		".env": true, "conf/.ENV": true, "id_ed25519": true, ".npmrc": true, "credentials.json": true,
		".env.local": true, "tls/server.KEY": true, "bundle.p12": true,
		".env.example": false, ".env.sample": false, ".env.template": false, "SKILL.md": false, "key.txt": false,
	}
	for rel, excluded := range cases {
		if got := secretExclusionReason(filepath.FromSlash(rel)) != ""; got != excluded {
			t.Errorf("secretExclusionReason(%q) excluded = %v, want %v", rel, got, excluded)
		}
	}
}

func TestResolveKindHintAndIncludeSet(t *testing.T) {
	if got := resolveKindHint(publishOpts{resourceType: "skill", aliasKind: "agent"}); got != "skill" {
		t.Errorf("--type skill must win, got %q", got)
	}
	if got := resolveKindHint(publishOpts{resourceType: "agent"}); got != "agent" {
		t.Errorf("got %q", got)
	}
	if got := resolveKindHint(publishOpts{resourceType: "plugin", aliasKind: "agent"}); got != "agent" {
		t.Errorf("unknown --type must fall back to alias, got %q", got)
	}
	if toIncludeSet(nil) != nil {
		t.Error("empty include list must yield nil set")
	}
	set := toIncludeSet([]string{"./conf/../.env", filepath.Join("a", "b.pem")})
	if !set[".env"] || !set["a/b.pem"] || !set[filepath.Join("a", "b.pem")] {
		t.Errorf("include set = %v", set)
	}
}

func TestBuildBundleFor_ManifestSelection(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		sel     bundleSelector
		want    string
		wantErr string
	}{
		{"explicit", map[string]string{"docs/main.md": "explicit", "SKILL.md": "s"}, bundleSelector{explicitManifestRel: "docs/main.md"}, "explicit", ""},
		{"explicit missing", map[string]string{"SKILL.md": "s"}, bundleSelector{explicitManifestRel: "nope.md"}, "", `--manifest "nope.md" not found`},
		{"kind specific", map[string]string{"skill.md": "lower", "agent.md": "a"}, bundleSelector{kind: "skill"}, "lower", ""},
		{"kind missing", map[string]string{"agent.md": "a"}, bundleSelector{kind: "skill"}, "", "no SKILL.md found at the root"},
		{"ambiguous canonical", map[string]string{"SKILL.md": "s", "agent.md": "a"}, bundleSelector{}, "", "ambiguous manifest: found both"},
		{"single root md", map[string]string{"guide.md": "g", "sub/x.md": "x"}, bundleSelector{}, "g", ""},
		{"multiple root md", map[string]string{"a.md": "a", "b.md": "b"}, bundleSelector{}, "", "multiple root .md files"},
		{"single nested md", map[string]string{"sub/x.md": "x", "run.sh": "r"}, bundleSelector{}, "x", ""},
		{"multiple nested md", map[string]string{"a/x.md": "x", "b/y.md": "y"}, bundleSelector{}, "", "multiple nested .md files"},
		{"no md", map[string]string{"run.sh": "r"}, bundleSelector{}, "", "no manifest file found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tc.files)
			manifest, _, _, _, err := buildBundleFor(dir, tc.sel)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(manifest) != tc.want {
				t.Errorf("manifest = %q, want %q", manifest, tc.want)
			}
		})
	}
}

func TestBuildBundleFor_EntryCap(t *testing.T) {
	old := artifactMaxEntries
	artifactMaxEntries = 2
	t.Cleanup(func() { artifactMaxEntries = old })
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"SKILL.md": "s", "a": "a", "b": "b"})
	if _, _, _, _, err := buildBundleFor(dir, bundleSelector{}); err == nil || !strings.Contains(err.Error(), "entry-count cap") {
		t.Fatalf("got %v", err)
	}
}

func TestCheckSymlink_RefusesBrokenAndNonRegularTargets(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "broken")
	toDir := filepath.Join(root, "todir")
	if err := os.Symlink("missing-target", broken); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("dir", toDir); err != nil {
		t.Fatal(err)
	}
	if err := checkSymlink(broken, root); err == nil || !strings.Contains(err.Error(), "cannot be resolved") {
		t.Errorf("broken symlink: got %v", err)
	}
	if err := checkSymlink(toDir, root); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Errorf("directory symlink: got %v", err)
	}
	if err := checkSymlink(filepath.Join(root, "absent"), root); err == nil || !strings.Contains(err.Error(), "lstat") {
		t.Errorf("missing path: got %v", err)
	}
}

func TestBuildMcpManifestContent_Defaults(t *testing.T) {
	raw, err := buildMcpManifestContent(mcpDescriptor{Name: "n", Transport: "sse", Endpoint: "https://e.example.com", AuthRef: "ref", Source: "url", AuthType: "api_key", AuthScope: "user"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != "1.0.0" || m["description"] != "Org-owned MCP server." || m["authRef"] != "ref" || m["authScope"] != "user" {
		t.Errorf("unexpected manifest %v", m)
	}
	if _, ok := m["slug"]; ok {
		t.Error("empty slug must be omitted")
	}
}

func TestValidateMcpDescriptor_Limits(t *testing.T) {
	base := mcpDescriptor{Transport: "stdio-command", Command: "srv"}
	cases := map[string]func(*mcpDescriptor){
		"negative version":   func(d *mcpDescriptor) { d.V = -1 },
		"too many hosts":     func(d *mcpDescriptor) { d.RuntimeEgressHosts = make([]string, 21) },
		"host with port":     func(d *mcpDescriptor) { d.RuntimeEgressHosts = []string{"api.example.com:443"} },
		"empty host":         func(d *mcpDescriptor) { d.RuntimeEgressHosts = []string{""} },
		"stdio no command":   func(d *mcpDescriptor) { d.Command = "" },
		"stdio endpoint":     func(d *mcpDescriptor) { d.Endpoint = "https://e.example.com" },
		"network no target":  func(d *mcpDescriptor) { d.Transport = "sse"; d.Command = "" },
		"long command":       func(d *mcpDescriptor) { d.Command = strings.Repeat("c", 513) },
		"empty tool":         func(d *mcpDescriptor) { d.DeclaredTools = []string{""} },
		"too many tools":     func(d *mcpDescriptor) { d.DeclaredTools = make([]string, 257) },
		"none with auth ref": func(d *mcpDescriptor) { d.AuthType = "none"; d.AuthRef = "ref" },
		"malformed auth ref": func(d *mcpDescriptor) { d.AuthRef = "9starts-with-digit" },
	}
	for name, mutate := range cases {
		d := base
		mutate(&d)
		if err := validateMcpDescriptor(d); err == nil {
			t.Errorf("%s: invalid descriptor accepted", name)
		}
	}
	ok := base
	ok.RuntimeEgressHosts = []string{"api.example.com"}
	ok.DeclaredTools = []string{"t"}
	if err := validateMcpDescriptor(ok); err != nil {
		t.Errorf("valid descriptor rejected: %v", err)
	}
}
