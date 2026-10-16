package cli

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCatalogSearchAndShow(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/catalog/browse":         jsonReply(200, `{"items":[{"key":"gmail","name":"Gmail","sources":[{"source":"native","tools":24},{"source":"federated","tools":40}],"preferred":"native"},{"key":"gh","name":"GitHub","sources":[{"source":"registry"}]}],"nextCursor":"n"}`),
		"GET /api/catalog/services/gmail": jsonReply(200, `{"service":{"key":"gmail","name":"Gmail","summary":"Read and send mail","sources":[{"source":"native","tools":24,"name":"Gmail (built in)"},{"source":"federated","tools":40,"name":"Composio Gmail"}],"preferred":"native"}}`),
	})
	out, err := runCommand(t, CatalogCmd(), "search", "mail", "--source", "native", "--cursor", "c0")
	if err != nil || !strings.Contains(out, "native, federated") || !strings.Contains(out, "More: --cursor n") {
		t.Fatalf("search: %v\n%s", err, out)
	}
	if out, err := runCommand(t, CatalogCmd(), "search", "mail", "--json"); err != nil || !strings.Contains(out, `"items"`) {
		t.Errorf("--json: %v", err)
	}
	if _, err := runCommand(t, CatalogCmd(), "search", "x", "--source", "cloud"); err == nil {
		t.Error("an unknown source")
	}
	out, err = runCommand(t, CatalogCmd(), "show", "gmail")
	if err != nil || !strings.Contains(out, "Gmail (gmail)") || !strings.Contains(out, " * native") || !strings.Contains(out, "Composio Gmail") || !strings.Contains(out, "preferred source") {
		t.Errorf("show: %v\n%s", err, out)
	}
	if _, err := runCommand(t, CatalogCmd(), "show", "a/b"); err == nil {
		t.Error("a path as the service")
	}
	if q := gw.count("GET /api/catalog/browse"); q != 2 {
		t.Errorf("browse requests = %d", q)
	}
}

func TestCatalogFederated(t *testing.T) {
	secret := fakeSecret("vendor")
	gw := a2aGateway(t, routeFns{
		"GET /api/catalog/federated":             jsonReply(200, `{"items":[{"id":"fed_1","vendor":"composio","credentialMode":"per_user","apps":3,"health":"ok","reviewQueue":12}]}`),
		"GET /api/catalog/federated/fed_1":       jsonReply(200, `{"federated":{"id":"fed_1","vendor":"composio","credentialMode":"per_user","health":"ok","apps":[{"vendorApp":"gmail","status":"enabled","tools":40}]}}`),
		"POST /api/catalog/federated":            jsonReply(201, `{"federated":{"id":"fed_2"}}`),
		"POST /api/catalog/federated/fed_1/apps": jsonReply(202, `{}`),
	})
	if out, err := runCommand(t, CatalogCmd(), "federated", "list"); err != nil || !strings.Contains(out, "fed_1  composio  per_user     3     ok      12") {
		t.Errorf("list: %v\n%s", err, out)
	}
	if out, err := runCommand(t, CatalogCmd(), "federated", "show", "fed_1"); err != nil || !strings.Contains(out, "gmail") || !strings.Contains(out, "per_user") {
		t.Errorf("show: %v\n%s", err, out)
	}
	out, err := runWithStdin(t, CatalogCmd(), `{"api_key":"`+secret+`"}`, "federated", "add", "composio", "--credential-stdin", "--per-user")
	if err != nil || strings.Contains(out, secret) || !strings.Contains(out, "Added composio as fed_2") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	b := gw.body("POST /api/catalog/federated", 0)
	if b["vendor"] != "composio" || b["credentialMode"] != "per_user" || b["credentials"].(map[string]any)["api_key"] != secret {
		t.Errorf("add body = %v", b)
	}
	if _, err := runWithStdin(t, CatalogCmd(), `{"k":"v"}`, "federated", "add", "composio"); err == nil {
		t.Error("add without a way to give the credential")
	}
	if _, err := runWithStdin(t, CatalogCmd(), `{"k":"v"}`, "federated", "add", "Bad Vendor", "--credential-stdin"); err == nil {
		t.Error("a bad vendor name")
	}
	if out, err := runCommand(t, CatalogCmd(), "federated", "enable", "fed_1", "gmail", "slack"); err != nil || !strings.Contains(out, "Enabling 2 app(s)") {
		t.Errorf("enable: %v\n%s", err, out)
	}
	if apps := gw.body("POST /api/catalog/federated/fed_1/apps", 0)["apps"].([]any); len(apps) != 2 || apps[0].(map[string]any)["vendorApp"] != "gmail" {
		t.Errorf("enable body = %v", gw.body("POST /api/catalog/federated/fed_1/apps", 0))
	}
	stubBrowserWith(t, nil)
	if out, err := runCommand(t, CatalogCmd(), "federated", "review", "fed_1", "--no-browser"); err != nil || !strings.Contains(out, "/catalog/federated/fed_1") {
		t.Errorf("review: %v\n%s", err, out)
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runWithStdin(t, CatalogCmd(), `{"k":"v"}`, "federated", "add", "composio", "--credential-stdin"); exitCodeOf(err) != exitDenied {
		t.Errorf("an agent adding a catalog: exit %d", exitCodeOf(err))
	}
	if _, err := runCommand(t, CatalogCmd(), "federated", "review", "fed_1"); err == nil {
		t.Error("an agent reviewing")
	}
	if gw.count("POST /api/catalog/federated") != 1 {
		t.Error("an agent's add reached the gateway")
	}
}

const registryVersions = `{"versions":[{"version":"1.2.0","packages":[{"registryType":"npm","identifier":"@acme/files-mcp"}],"remotes":[{"url":"https://mcp.acme.example/sse"}]},{"version":"1.1.0","packages":[{"registryType":"pypi","identifier":"acme-files"}]}]}`

func TestCatalogRegistry(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/catalog/registry": jsonReply(200, `{"items":[{"name":"io.acme/files","description":"Files","version":"1.2.0","status":"active"}],"nextCursor":"n"}`),
		"GET /api/catalog/registry/io.acme/files/versions": func(_ *packServer, w http.ResponseWriter, r *http.Request) {
			jsonReply(200, registryVersions)(nil, w, r)
		},
		"POST /api/catalog/registry/install": jsonReply(201, `{"upstreamId":"up_1","status":"pending_pin"}`),
	})
	if out, err := runCommand(t, CatalogCmd(), "registry", "search", "files", "--source", "official", "--cursor", "c"); err != nil || !strings.Contains(out, "io.acme/files") || !strings.Contains(out, "More: --cursor n") {
		t.Errorf("search: %v\n%s", err, out)
	}
	out, err := runCommand(t, CatalogCmd(), "registry", "show", "io.acme/files")
	if err != nil || !strings.Contains(out, "package  npm @acme/files-mcp") || !strings.Contains(out, "remote   https://mcp.acme.example/sse") {
		t.Errorf("show: %v\n%s", err, out)
	}
	out, err = runCommand(t, CatalogCmd(), "registry", "install", "io.acme/files@1.2.0", "--remote")
	if err != nil || !strings.Contains(out, "Installed io.acme/files as up_1 (pending_pin)") {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if b := gw.body("POST /api/catalog/registry/install", 0); b["name"] != "io.acme/files" || b["version"] != "1.2.0" || b["remote"] != true || b["source"] != "registry" {
		t.Errorf("install body = %v", b)
	}
	if _, err := runCommand(t, CatalogCmd(), "registry", "install", "io.acme/files", "--remote", "--package"); err == nil {
		t.Error("--remote with --package")
	}

	out, err = runCommand(t, CatalogCmd(), "registry", "install", "io.acme/files", "--wrap")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zuano mcp wrap --upstream files -- npx -y @acme/files-mcp@1.2.0", "zuano mcp install files --command npx --arg -y --arg @acme/files-mcp@1.2.0 --firewall"} {
		if !strings.Contains(out, want) {
			t.Errorf("wrap output lacks %q:\n%s", want, out)
		}
	}
	if gw.count("POST /api/catalog/registry/install") != 1 {
		t.Error("--wrap installed on the gateway")
	}
	out, err = runCommand(t, CatalogCmd(), "registry", "install", "io.acme/files@1.1.0", "--wrap")
	if err != nil || !strings.Contains(out, "uvx acme-files==1.1.0") {
		t.Errorf("pypi wrap: %v\n%s", err, out)
	}
	if _, err := runCommand(t, CatalogCmd(), "registry", "install", "io.acme/files@9.9.9", "--wrap"); err == nil {
		t.Error("a version the registry lacks")
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, CatalogCmd(), "registry", "install", "io.acme/files"); exitCodeOf(err) != exitDenied {
		t.Errorf("an agent installing: exit %d", exitCodeOf(err))
	}
}

func TestWrapCommandRefusesWhatCannotBeQuotedSafely(t *testing.T) {
	for _, tc := range []struct {
		typ, id, ver string
		want         string
	}{
		{"npm", "@a/b", "1.0.0", "npx -y @a/b@1.0.0"},
		{"npm", "left-pad", "", "npx -y left-pad"},
		{"pypi", "acme-files", "2.0", "uvx acme-files==2.0"},
		{"oci", "ghcr.io/acme/files", "1.0", "docker run -i --rm ghcr.io/acme/files:1.0"},
	} {
		argv, err := wrapCommand(tc.typ, tc.id, tc.ver)
		if err != nil || strings.Join(argv, " ") != tc.want {
			t.Errorf("%v: %v %v", tc, argv, err)
		}
	}
	for _, bad := range [][3]string{{"npm", "a; rm -rf ~", "1"}, {"npm", "a", "1 && x"}, {"npm", "$(x)", ""}, {"nuget", "a", "1"}, {"npm", "a`b`", ""}} {
		if _, err := wrapCommand(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestRegistryWrapOfARemoteOnlyServer(t *testing.T) {
	a2aGateway(t, routeFns{"GET /api/catalog/registry/acme/remote/versions": jsonReply(200, `{"versions":[{"version":"1.0.0","remotes":[{"url":"https://mcp.acme.example/mcp"}]}]}`),
		"GET /api/catalog/registry/acme/bad/versions": jsonReply(200, `{"versions":[{"version":"1.0.0","remotes":[{"url":"http://mcp.acme.example/mcp"}],"packages":[{"registryType":"npm","identifier":"x y"}]}]}`)})
	out, err := runCommand(t, CatalogCmd(), "registry", "install", "acme/remote", "--wrap")
	if err != nil || !strings.Contains(out, "zuano mcp install remote --url https://mcp.acme.example/mcp --firewall") {
		t.Errorf("remote: %v\n%s", err, out)
	}
	if _, err := runCommand(t, CatalogCmd(), "registry", "install", "acme/bad", "--wrap"); err == nil {
		t.Error("a server with nothing safe to print")
	}
}

func TestCatalogReferenceDocumentsEveryCommandAndFlag(t *testing.T) {
	doc := filepath.Join("..", "..", "docs", "reference", "catalog.md")
	assertDocumented(t, CatalogCmd(), "zuano catalog", doc)
	custom := &cobra.Command{Use: "connectors"}
	custom.AddCommand(connectorsImportCmd(), connectorsListCmd(), connectorsShowCmd(), connectorsVersionsCmd(), connectorsPublishCmd(), connectorsDisableCmd(), connectorsExportCmd())
	assertDocumented(t, custom, "zuano connectors", doc)
}
