package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodPluginYAML = `name: toolkit
version: 1.2.0
description: Team toolkit
components:
  skills:
    - ref: triage@1.4.0
  commands:
    - file: commands/review.md
  rules:
    - file: rules/style.md
      name: team-style
  hooks:
    - event: PreToolUse
      matcher: Bash
      command: ./hooks/check.sh
      timeoutSec: 10
  mcp:
    - ref: docs@2.0.1
    - http: https://mcp.example.com/mcp
`

func writePluginDir(t *testing.T, yamlText string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"plugin.yaml":        yamlText,
		"commands/review.md": "---\ndescription: Review a change\nargument-hint: <ref>\n---\nReview $ARGUMENTS.\n",
		"rules/style.md":     "---\ndescription: Style\nglobs: \"*.go, *.ts\"\nalways: false\n---\nBe terse.\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func publishFields(t *testing.T, req *capturedRequest) map[string]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(req.contentType)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	mr := multipart.NewReader(bytes.NewReader(req.body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		fields[p.FormName()] = string(data)
	}
	return fields
}

func TestPublishPlugin_SendsTheManifestAndTheValidatedComponents(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()
	dir := writePluginDir(t, goodPluginYAML)
	doer := &publishTestDoer{srv: srv}
	var out bytes.Buffer
	if err := runPublish(doer, dir, publishOpts{resourceType: "plugin", channel: "beta", out: &out}); err != nil {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 hook(s) run on user machines") {
		t.Errorf("output = %s", out.String())
	}
	fields := publishFields(t, doer.lastRequest())
	if fields["type"] != "plugin" || fields["channel"] != "beta" || fields["manifest"] != goodPluginYAML {
		t.Errorf("fields = %v", fields)
	}
	var doc struct {
		Version    string            `json:"version"`
		Components []pluginComponent `json:"components"`
	}
	if err := json.Unmarshal([]byte(fields["pluginComponents"]), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "1.2.0" || len(doc.Components) != 6 {
		t.Fatalf("components = %+v", doc)
	}
	byKey := map[string]pluginComponent{}
	for _, c := range doc.Components {
		byKey[c.Kind+"/"+c.Name] = c
	}
	if c := byKey["skill/triage"]; c.Ref == nil || c.Ref.Version != "1.4.0" {
		t.Errorf("skill = %+v", c)
	}
	if c := byKey["command/review"]; c.Content["description"] != "Review a change" || c.Content["argumentHint"] != "<ref>" || c.Content["body"] != "Review $ARGUMENTS.\n" {
		t.Errorf("command = %+v", c)
	}
	if c := byKey["rule/team-style"]; c.Content["alwaysApply"] != false || len(c.Content["globs"].([]any)) != 2 {
		t.Errorf("rule = %+v", c)
	}
	if c := byKey["hook/pretooluse-1"]; c.Content["command"] != "./hooks/check.sh" || c.Content["timeoutSec"] != float64(10) || c.Content["matcher"] != "Bash" {
		t.Errorf("hook = %+v", c)
	}
	if c := byKey["mcp_server/mcp"]; c.Content["url"] != "https://mcp.example.com/mcp" {
		t.Errorf("inline mcp = %+v (%v)", c, byKey)
	}
	if c := byKey["mcp_server/docs"]; c.Ref == nil || c.Ref.Name != "docs" {
		t.Errorf("mcp ref = %+v", c)
	}
}

func TestPublishPlugin_DryRunSendsNothingAndAFileTargetWorks(t *testing.T) {
	srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
	defer srv.Close()
	dir := writePluginDir(t, goodPluginYAML)
	doer := &publishTestDoer{srv: srv}
	var out bytes.Buffer
	if err := runPublish(doer, filepath.Join(dir, "plugin.yaml"), publishOpts{resourceType: "plugin", dryRun: true, out: &out}); err != nil {
		t.Fatal(err)
	}
	if doer.requestCount() != 0 {
		t.Error("a dry run sent a request")
	}
	for _, want := range []string{"[dry-run] version: 1.2.0", "[dry-run] skills: 1", "[dry-run] hooks: 1", "runs on user machines (needs admin review): hook PreToolUse: ./hooks/check.sh"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPublishPlugin_RejectsAnInvalidDefinition(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"bad version":        {"name: toolkit\nversion: latest\ncomponents:\n  rules: [{file: rules/style.md}]\n", "semantic version"},
		"bad name":           {"name: Tool Kit\nversion: 1.0.0\ncomponents:\n  rules: [{file: rules/style.md}]\n", "lower case"},
		"unpinned skill":     {"name: toolkit\nversion: 1.0.0\ncomponents:\n  skills: [{ref: triage}]\n", "name@1.2.3"},
		"range skill":        {"name: toolkit\nversion: 1.0.0\ncomponents:\n  skills: [{ref: \"triage@^1.0.0\"}]\n", "name@1.2.3"},
		"escaping file":      {"name: toolkit\nversion: 1.0.0\ncomponents:\n  commands: [{file: ../secrets.md}]\n", "inside the plugin directory"},
		"missing file":       {"name: toolkit\nversion: 1.0.0\ncomponents:\n  commands: [{file: commands/none.md}]\n", "no such file"},
		"http mcp":           {"name: toolkit\nversion: 1.0.0\ncomponents:\n  mcp: [{http: \"http://mcp.example.com\"}]\n", "https URL"},
		"both ref and http":  {"name: toolkit\nversion: 1.0.0\ncomponents:\n  mcp: [{ref: docs@1.0.0, http: \"https://x.example.com\"}]\n", "exactly one"},
		"multiline hook":     {"name: toolkit\nversion: 1.0.0\ncomponents:\n  hooks: [{event: Stop, command: \"a\\nb\"}]\n", "one-line command"},
		"no components":      {"name: toolkit\nversion: 1.0.0\n", "no components"},
		"duplicate":          {"name: toolkit\nversion: 1.0.0\ncomponents:\n  commands: [{file: commands/review.md}, {file: commands/review.md}]\n", "listed twice"},
		"unknown field":      {"name: toolkit\nversion: 1.0.0\nflavour: x\ncomponents:\n  rules: [{file: rules/style.md}]\n", "flavour"},
		"bad component name": {"name: toolkit\nversion: 1.0.0\ncomponents:\n  commands: [{file: commands/review.md, name: Review Me}]\n", "kebab-case"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := buildPublishServer(t, http.StatusCreated, successPublishResponse)
			defer srv.Close()
			doer := &publishTestDoer{srv: srv}
			err := runPublish(doer, writePluginDir(t, tc.yaml), publishOpts{resourceType: "plugin", out: io.Discard})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if doer.requestCount() != 0 {
				t.Error("an invalid bundle was sent")
			}
		})
	}
}

func TestPublishPlugin_ALinkedCommandFileIsRefused(t *testing.T) {
	dir := writePluginDir(t, "name: toolkit\nversion: 1.0.0\ncomponents:\n  commands: [{file: commands/linked.md}]\n")
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "commands", "linked.md")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := runPublish(&publishTestDoer{}, dir, publishOpts{resourceType: "plugin", out: io.Discard}); err == nil {
		t.Error("a symlinked command file was read")
	}
}
