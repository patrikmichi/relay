package agentsync

import (
	"path/filepath"
	"strings"
	"testing"
)

func serverNode(url string) *jnode {
	return toNode(obj{{"type", "http"}, {"url", url}})
}

func TestEditJSONCSetKeepsEveryComment(t *testing.T) {
	src := `// editor settings
{
  /* servers */
  "servers": {
    // mine
    "files": { "command": "npx" }, // trailing note
  },
  "other": [1, 2, 3], // keep
}
`
	out, err := editJSONC([]byte(src), []string{"servers"}, []jmut{{key: "docs", val: serverNode("https://docs.example.com/mcp")}}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"// editor settings", "/* servers */", "// mine", `"files": { "command": "npx" }, // trailing note`, `"other": [1, 2, 3], // keep`, `"docs": {`, "https://docs.example.com/mcp"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost or missing %q:\n%s", want, got)
		}
	}
	if _, err := scanJSONC(out); err != nil {
		t.Fatalf("result is not valid JSONC: %v\n%s", err, got)
	}
	// The new member sits below the commented one, not on its comment's line.
	if strings.Index(got, `"docs"`) < strings.Index(got, "// trailing note") {
		t.Errorf("new member landed before the trailing comment:\n%s", got)
	}
}

func TestEditJSONCReplaceDeleteAndCreateBranch(t *testing.T) {
	src := "{\n  // top\n  \"servers\": {\n    \"a\": {\"url\": \"https://one.example.com\"}, // first\n    \"b\": {\"url\": \"https://two.example.com\"}\n  }\n}\n"

	out, err := editJSONC([]byte(src), []string{"servers"}, []jmut{{key: "a", val: serverNode("https://changed.example.com")}}, "  ")
	if err != nil || !strings.Contains(string(out), "changed.example.com") || !strings.Contains(string(out), "// first") || strings.Contains(string(out), "one.example.com") {
		t.Fatalf("replace: %v\n%s", err, out)
	}

	out, err = editJSONC([]byte(src), []string{"servers"}, []jmut{{key: "b", del: true}}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"b"`) || !strings.Contains(string(out), "// top") {
		t.Fatalf("delete last: %s", out)
	}
	if _, err := scanJSONC(out); err != nil {
		t.Fatalf("deleting the last member left invalid JSON: %v\n%s", err, out)
	}

	out, err = editJSONC([]byte(src), []string{"servers"}, []jmut{{key: "a", del: true}, {key: "b", del: true}}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanJSONC(out); err != nil || !strings.Contains(string(out), "// top") {
		t.Fatalf("emptying the object: %v\n%s", err, out)
	}

	out, err = editJSONC([]byte("{\n  // only a comment\n}\n"), []string{"mcp"}, []jmut{{key: "x", val: serverNode("https://x.example.com")}}, "  ")
	if err != nil || !strings.Contains(string(out), "// only a comment") || !strings.Contains(string(out), `"mcp": {`) {
		t.Fatalf("create branch: %v\n%s", err, out)
	}
	if _, err := scanJSONC(out); err != nil {
		t.Fatalf("created branch is invalid: %v\n%s", err, out)
	}
}

func TestEditJSONCKeepsBOMAndCRLF(t *testing.T) {
	src := "\xef\xbb\xbf{\r\n  // note\r\n  \"servers\": {}\r\n}\r\n"
	out, err := editJSONC([]byte(src), []string{"servers"}, []jmut{{key: "docs", val: serverNode("https://docs.example.com/mcp")}}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "\xef\xbb\xbf") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") || !strings.Contains(got, "// note") {
		t.Fatalf("BOM, CRLF or comment lost:\n%q", got)
	}
}

func TestVSCodeMCPJSONWithCommentsIsEditedAndCleaned(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Project, ".vscode", "mcp.json")
	src := "{\n  // my servers\n  \"servers\": {\n    \"mine\": { \"command\": \"x\" } /* hand written */,\n  },\n}\n"
	writeFile(t, path, src)
	opts := Options{Agents: []AgentID{Copilot}, Scopes: []Scope{ScopeProject}, Kinds: []Kind{KindMCP}}
	syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, opts)

	got := readFile(t, path)
	for _, want := range []string{"// my servers", "/* hand written */", `"mine"`, `"zuano"`, `"docs"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}

	plan := syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, opts)
	for _, it := range plan.Items {
		if it.Kind == KindMCP && it.Status != StatusUnchanged && it.Status != StatusSkipped {
			t.Errorf("second sync changed %s: %s", it.Name, it.Status)
		}
	}

	syncAll(t, env, map[Scope]Bundle{ScopeProject: {}}, opts)
	got = readFile(t, path)
	if !strings.Contains(got, "// my servers") || !strings.Contains(got, `"mine"`) || strings.Contains(got, `"zuano"`) || strings.Contains(got, `"docs"`) {
		t.Errorf("removing zuano's servers damaged the file:\n%s", got)
	}
	if _, err := scanJSONC([]byte(got)); err != nil {
		t.Errorf("result is not valid JSONC: %v\n%s", err, got)
	}
}

func TestHooksInAJSONCFileAreStillSkippedWithAReason(t *testing.T) {
	env := newEnv(t)
	settings := filepath.Join(env.Home, ".claude", "settings.json")
	src := "{\n  // mine\n  \"theme\": \"dark\",\n}\n"
	writeFile(t, settings, src)
	plan := syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{Claude}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindHooks}})
	var detail string
	for _, it := range plan.Items {
		if it.Kind == KindHooks && it.Status == StatusSkipped {
			detail = it.Detail
		}
	}
	if !strings.Contains(detail, "JSONC") || readFile(t, settings) != src {
		t.Errorf("detail = %q; the file must be untouched", detail)
	}
}
