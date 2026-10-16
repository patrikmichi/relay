package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const petstore = `{"openapi":"3.0.3","info":{"title":"Pets","version":"1"},"paths":{"/pets":{"get":{"operationId":"listPets"}}}}`

func writeSpec(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const importReply = `{"draftId":"drf_1","revision":4,"format":"openapi3","operations":[{"id":"listPets","method":"get","path":"/pets","summary":"List pets","risk":"read"},{"id":"deletePet","method":"delete","path":"/pets/{id}","summary":"Delete a pet","risk":"destructive"}],"warnings":["2 operations have no summary"]}`

func importGateway(t *testing.T) *packServer {
	return a2aGateway(t, routeFns{
		"POST /api/connectors/import":               jsonReply(201, importReply),
		"PATCH /api/connectors/drafts/drf_1":        jsonReply(200, `{}`),
		"POST /api/connectors/drafts/drf_1/preview": jsonReply(200, `{"tools":[{"name":"list_pets","risk":"read","method":"get","path":"/pets"}],"quality":{"passed":true,"issues":[]},"version":2}`),
		"POST /api/connectors/drafts/drf_1/publish": jsonReply(200, `{"slug":"pets","version":2}`),
	})
}

func TestConnectorsImportList(t *testing.T) {
	gw := importGateway(t)
	out, err := runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "petstore.json", petstore), "--list")
	if err != nil {
		t.Fatalf("import --list: %v\n%s", err, out)
	}
	for _, want := range []string{"Draft drf_1 from openapi3: 2 operation(s)", "GET /pets", "DELETE /pets/{id}", "destructive", "warning: 2 operations have no summary"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	b := gw.body("POST /api/connectors/import", 0)
	if b["format"] != "openapi3" || !strings.Contains(b["text"].(string), "listPets") || b["url"] != nil {
		t.Errorf("body = %v", b)
	}
	if gw.count("PATCH /api/connectors/drafts/drf_1") != 0 {
		t.Error("--list changed the draft")
	}
	if out, err := runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "p.json", petstore), "--list", "--json"); err != nil || !strings.Contains(out, `"draftId"`) {
		t.Errorf("--json: %v", err)
	}
}

func TestConnectorsImportSelectsPreviewsAndPublishes(t *testing.T) {
	gw := importGateway(t)
	out, err := runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "p.yaml", "openapi: 3.0.3\npaths: {}\n"), "--ops", "listPets", "--slug", "pets", "--risk", "listPets=write:it logs the caller", "--auth-config", "ac_1")
	if err != nil || !strings.Contains(out, "Draft drf_1: 1 tool(s)") || !strings.Contains(out, "Nothing is published yet") {
		t.Fatalf("import: %v\n%s", err, out)
	}
	p := gw.body("PATCH /api/connectors/drafts/drf_1", 0)
	sel := p["selection"].(map[string]any)
	ro := p["riskOverrides"].(map[string]any)["listPets"].(map[string]any)
	if p["expectedRevision"] != 4.0 || sel["operations"].([]any)[0] != "listPets" || p["slug"] != "pets" || p["authConfigId"] != "ac_1" || ro["class"] != "write" || ro["reason"] != "it logs the caller" {
		t.Errorf("patch = %v", p)
	}
	if gw.count("POST /api/connectors/drafts/drf_1/publish") != 0 {
		t.Error("published without --publish")
	}
	out, err = runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "p.json", petstore), "--suggest", "--publish")
	if err != nil || !strings.Contains(out, "Published pets version 2") {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	if sel := gw.body("PATCH /api/connectors/drafts/drf_1", 1)["selection"].(map[string]any); sel["suggest"] != true {
		t.Errorf("selection = %v", sel)
	}
	if pub := gw.body("POST /api/connectors/drafts/drf_1/publish", 0); pub["expectedVersion"] != 2.0 {
		t.Errorf("publish body = %v", pub)
	}
}

func TestConnectorsImportFromAURLIsFetchedByTheGateway(t *testing.T) {
	gw := importGateway(t)
	if _, err := runCommand(t, ConnectorsCmd(), "import", "https://api.example.com/openapi.json", "--list", "--format", "openapi3"); err != nil {
		t.Fatal(err)
	}
	b := gw.body("POST /api/connectors/import", 0)
	if b["url"] != "https://api.example.com/openapi.json" || b["text"] != nil || b["format"] != "openapi3" {
		t.Errorf("body = %v", b)
	}
	if _, err := runCommand(t, ConnectorsCmd(), "import", "https://user:pw@api.example.com/x", "--list"); err == nil {
		t.Error("a URL with credentials")
	}
}

func TestConnectorsImportExitCodes(t *testing.T) {
	gw := importGateway(t)
	cases := []struct {
		name string
		file string
		args []string
		want int
	}{
		{"invalid file", writeSpec(t, "a.txt", "hello"), []string{"--list"}, exitSpecInvalid},
		{"unsupported file", writeSpec(t, "a.xml", `<?xml version="1.0"?><definitions/>`), []string{"--list"}, exitSpecUnsupported},
		{"format mismatch", writeSpec(t, "a.json", petstore), []string{"--list", "--format", "postman"}, exitSpecInvalid},
		{"nothing chosen", writeSpec(t, "a.json", petstore), nil, 1},
		{"bad format", writeSpec(t, "a.json", petstore), []string{"--list", "--format", "raml"}, 1},
		{"bad risk", writeSpec(t, "a.json", petstore), []string{"--ops", "a", "--risk", "a=bad:why"}, 1},
		{"risk without a reason", writeSpec(t, "a.json", petstore), []string{"--ops", "a", "--risk", "a=read:"}, 1},
		{"publish with list", writeSpec(t, "a.json", petstore), []string{"--list", "--publish"}, 1},
		{"bad auth config", writeSpec(t, "a.json", petstore), []string{"--ops", "a", "--auth-config", "nope"}, 1},
		{"missing file", filepath.Join(t.TempDir(), "none.json"), []string{"--list"}, 1},
	}
	for _, tc := range cases {
		_, err := runCommand(t, ConnectorsCmd(), append([]string{"import", tc.file}, tc.args...)...)
		if exitCodeOf(err) != tc.want {
			t.Errorf("%s: exit %d (%v), want %d", tc.name, exitCodeOf(err), err, tc.want)
		}
	}
	if gw.count("POST /api/connectors/import") != 0 {
		t.Error("a bad file reached the gateway")
	}
	big := writeSpec(t, "big.json", `{"openapi":"3.0.0","x":"`+strings.Repeat("a", 8<<20)+`"}`)
	if _, err := runCommand(t, ConnectorsCmd(), "import", big, "--list"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a large file: %v", err)
	}

	for code, want := range map[string]int{"spec_invalid": exitSpecInvalid, "spec_unsupported": exitSpecUnsupported, "connector_slug_taken": exitConnectorClash, "connector_version_conflict": exitConnectorClash, "quality_gate_failed": exitQualityGate} {
		code := code
		gw.replace(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"error":"` + code + `","message":"refused"}`))
		})
		if _, err := runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "a.json", petstore), "--list"); exitCodeOf(err) != want {
			t.Errorf("%s: exit %d, want %d", code, exitCodeOf(err), want)
		}
	}
}

func TestConnectorsImportQualityGateAndAgents(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"POST /api/connectors/import":               jsonReply(201, importReply),
		"PATCH /api/connectors/drafts/drf_1":        jsonReply(200, `{}`),
		"POST /api/connectors/drafts/drf_1/preview": jsonReply(200, `{"tools":[],"quality":{"passed":false,"issues":["no tool has a description"]},"version":1}`),
		"POST /api/connectors/drafts/drf_1/publish": jsonReply(200, `{}`),
	})
	out, err := runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "a.json", petstore), "--suggest", "--publish")
	if exitCodeOf(err) != exitQualityGate || !strings.Contains(out, "quality: no tool has a description") || gw.count("POST /api/connectors/drafts/drf_1/publish") != 0 {
		t.Errorf("quality gate: exit %d\n%s", exitCodeOf(err), out)
	}
	t.Setenv("ZUANO_AGENT", "1")
	if _, err := runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "a.json", petstore), "--suggest", "--publish"); exitCodeOf(err) != exitDenied || !strings.Contains(err.Error(), "agent_not_allowed") {
		t.Errorf("an agent publishing: exit %d (%v)", exitCodeOf(err), err)
	}
	if _, err := runCommand(t, ConnectorsCmd(), "publish", "pets"); exitCodeOf(err) != exitDenied {
		t.Errorf("an agent publishing by slug: exit %d", exitCodeOf(err))
	}
	// An agent may still make a draft; it just cannot publish it.
	if _, err := runCommand(t, ConnectorsCmd(), "import", writeSpec(t, "a.json", petstore), "--list"); err != nil {
		t.Errorf("an agent listing operations: %v", err)
	}
}

func TestCustomConnectorCommands(t *testing.T) {
	gw := a2aGateway(t, routeFns{
		"GET /api/connectors/custom":                jsonReply(200, `{"items":[{"slug":"pets","name":"Pets","version":2,"status":"published","tools":5,"updatedAt":"2026-10-15"}]}`),
		"GET /api/connectors/custom/pets":           jsonReply(200, `{"connector":{"slug":"pets","name":"Pets","version":2,"status":"published","auth":"api_key","tools":[{"name":"list_pets","risk":"read"}]}}`),
		"GET /api/connectors/custom/pets/versions":  jsonReply(200, `{"items":[{"version":2,"status":"published","at":"2026-10-15","by":"patrik"},{"version":1,"status":"superseded","at":"2026-10-01"}]}`),
		"POST /api/connectors/custom/pets/disable":  jsonReply(200, `{}`),
		"GET /api/connectors/custom/pets/export":    jsonReply(200, `{"slug":"pets","tools":[]}`),
		"GET /api/connectors/drafts":                jsonReply(200, `{"items":[{"id":"drf_7","nextVersion":3}]}`),
		"POST /api/connectors/drafts/drf_7/publish": jsonReply(200, `{"slug":"pets","version":3}`),
	})
	if out, err := runCommand(t, ConnectorsCmd(), "list"); err != nil || !strings.Contains(out, "pets  Pets  v2") {
		t.Errorf("list: %v\n%s", err, out)
	}
	if out, err := runCommand(t, ConnectorsCmd(), "show", "pets"); err != nil || !strings.Contains(out, "pets (Pets) v2, published, auth api_key") || !strings.Contains(out, "list_pets") {
		t.Errorf("show: %v\n%s", err, out)
	}
	if out, err := runCommand(t, ConnectorsCmd(), "versions", "pets"); err != nil || !strings.Contains(out, "v1") || !strings.Contains(out, "superseded") {
		t.Errorf("versions: %v\n%s", err, out)
	}
	if out, err := runCommand(t, ConnectorsCmd(), "publish", "pets"); err != nil || !strings.Contains(out, "Published pets version 3") {
		t.Errorf("publish: %v\n%s", err, out)
	}
	if b := gw.body("POST /api/connectors/drafts/drf_7/publish", 0); b["expectedVersion"] != 3.0 {
		t.Errorf("publish body = %v", b)
	}
	if _, err := runCommand(t, ConnectorsCmd(), "publish", "pets", "--version", "5"); err != nil || gw.body("POST /api/connectors/drafts/drf_7/publish", 1)["expectedVersion"] != 5.0 {
		t.Errorf("publish --version: %v", err)
	}
	if _, err := runCommand(t, ConnectorsCmd(), "disable", "pets"); err == nil {
		t.Error("disable without --yes")
	}
	if out, err := runCommand(t, ConnectorsCmd(), "disable", "pets", "--yes"); err != nil || !strings.Contains(out, "Disabled pets") {
		t.Errorf("disable: %v", err)
	}
	if out, err := runCommand(t, ConnectorsCmd(), "export", "pets"); err != nil || !strings.Contains(out, `"slug":"pets"`) {
		t.Errorf("export: %v %q", err, out)
	}
	file := filepath.Join(t.TempDir(), "pets.json")
	if out, err := runCommand(t, ConnectorsCmd(), "export", "pets", "--out", file); err != nil || !strings.Contains(out, "Wrote") {
		t.Errorf("export --out: %v", err)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"slug"`) {
		t.Errorf("exported file = %q", b)
	}
	for _, args := range [][]string{{"show", "a/b"}, {"versions", "a?b"}, {"disable", "a:b", "-y"}, {"export", "../x"}, {"publish", "a b"}} {
		if _, err := runCommand(t, ConnectorsCmd(), args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestPublishNeedsExactlyOneDraft(t *testing.T) {
	drafts := `{"items":[]}`
	a2aGateway(t, routeFns{"GET /api/connectors/drafts": func(_ *packServer, w http.ResponseWriter, r *http.Request) { jsonReply(200, drafts)(nil, w, r) }})
	if _, err := runCommand(t, ConnectorsCmd(), "publish", "pets"); err == nil || !strings.Contains(err.Error(), "0 open drafts") {
		t.Errorf("no draft: %v", err)
	}
	drafts = `{"items":[{"id":"a"},{"id":"b"}]}`
	if _, err := runCommand(t, ConnectorsCmd(), "publish", "pets"); err == nil || !strings.Contains(err.Error(), "2 open drafts") {
		t.Errorf("two drafts: %v", err)
	}
}
