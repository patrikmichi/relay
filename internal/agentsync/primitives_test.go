package agentsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrderedJSONRoundTripKeepsOrderAndNumbers(t *testing.T) {
	in := "{\n  \"z\": 1,\n  \"a\": {\n    \"big\": 12345678901234567890,\n    \"s\": \"<&>\"\n  },\n  \"list\": [\n    1,\n    \"two\"\n  ]\n}\n"
	n, err := parseJSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(n.marshal(detectIndent([]byte(in)))); got != in {
		t.Fatalf("round trip changed the document:\n%s", got)
	}
}

func TestOrderedJSONRejectsCommentsAndTrailingData(t *testing.T) {
	for _, in := range []string{"{\n// c\n}", "{} {}", "[1,]", ""} {
		if _, err := parseJSON([]byte(in)); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}

func TestDetectIndent(t *testing.T) {
	for in, want := range map[string]string{"{\n\t\"a\": 1\n}": "\t", "{\n    \"a\": 1\n}": "    ", "{}": "  "} {
		if got := detectIndent([]byte(in)); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestNodeHashIgnoresKeyOrder(t *testing.T) {
	a := toNode(obj{{"a", 1}, {"b", obj{{"x", true}, {"y", "z"}}}})
	b := toNode(obj{{"b", obj{{"y", "z"}, {"x", true}}}, {"a", 1}})
	if a.hash() != b.hash() {
		t.Fatal("equal documents hash differently")
	}
	if a.hash() == toNode(obj{{"a", 2}}).hash() {
		t.Fatal("different documents hash equally")
	}
}

func TestUnifiedDiffShowsLocalHunks(t *testing.T) {
	a := strings.Split("1\n2\n3\n4\n5\n6\n7\n8\n9\n10", "\n")
	b := strings.Split("1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n11", "\n")
	got := unifiedDiff("a/f", "b/f", a, b)
	want := "--- a/f\n+++ b/f\n@@ -3,5 +3,5 @@\n 3\n 4\n-5\n+five\n 6\n 7\n@@ -9,2 +9,3 @@\n 9\n 10\n+11\n"
	if got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}

func TestDiffRedactsSecretLookingValues(t *testing.T) {
	line := `  "apiKey": "sk-live-123",`
	if got := redactLine(line); strings.Contains(got, "sk-live") {
		t.Fatalf("not redacted: %s", got)
	}
	ref := `"Authorization": "Bearer ${GATEWAY_API_KEY}"`
	if got := redactLine(ref); got != ref {
		t.Fatalf("env reference was redacted: %s", got)
	}
}

func TestLoadBundleParsesSourcesAndWarns(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "rules", "style.md"), "---\ndescription: Style\n---\nBe terse.\n")
	writeFile(t, filepath.Join(dir, "rules", "go.md"), "---\nglobs: \"**/*.go, **/go.mod\"\n---\nUse gofmt.\n")
	writeFile(t, filepath.Join(dir, "rules", "manual.md"), "---\nalways: false\n---\nOnly on request.\n")
	writeFile(t, filepath.Join(dir, "rules", "bad name.md"), "x")
	writeFile(t, filepath.Join(dir, "commands", "review.md"), "---\ndescription: Review\nargument-hint: \"[path]\"\n---\nReview $ARGUMENTS\n")
	writeFile(t, filepath.Join(dir, "hooks.json"), `{"hooks":[{"event":"stop","command":"/a.sh"},{"event":"nope","command":"/b.sh"},{"event":"stop","command":""}]}`)

	b, warns, err := LoadBundle(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rules) != 3 || len(b.Commands) != 1 || len(b.Hooks) != 1 {
		t.Fatalf("bundle = %+v", b)
	}
	byName := map[string]Rule{}
	for _, r := range b.Rules {
		byName[r.Name] = r
	}
	if !byName["style"].Always || byName["go"].Always || len(byName["go"].Globs) != 2 || byName["manual"].Always {
		t.Errorf("rule scoping wrong: %+v", byName)
	}
	if b.Commands[0].ArgumentHint != "[path]" || b.Commands[0].Body != "Review $ARGUMENTS" {
		t.Errorf("command = %+v", b.Commands[0])
	}
	if len(warns) != 3 {
		t.Errorf("warnings = %v", warns)
	}
}

func TestLoadBundleMissingDirIsEmpty(t *testing.T) {
	b, warns, err := LoadBundle(filepath.Join(t.TempDir(), "absent"), nil)
	if err != nil || len(warns) != 0 || b.count(KindMCP)+b.count(KindRules)+b.count(KindCommands)+b.count(KindHooks) != 0 {
		t.Fatalf("%+v %v %v", b, warns, err)
	}
}

func TestMCPSourceRoundTripAndValidation(t *testing.T) {
	env := newEnv(t)
	src := MCPSource{Servers: []MCPServer{{Name: "b", URL: "https://b.example/mcp"}, {Name: "a", Command: "a-mcp"}}}
	if err := SaveMCPSource(env, ScopeUser, src); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMCPSource(env, ScopeUser)
	if err != nil || len(got.Servers) != 2 || got.Servers[0].Name != "a" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := os.WriteFile(MCPSourcePath(env, ScopeUser), []byte(`{"servers":[{"name":"../x","url":"u"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMCPSource(env, ScopeUser); err == nil {
		t.Fatal("unsafe name accepted")
	}
	if err := (MCPServer{Name: "both", URL: "u", Command: "c"}).Validate(); err == nil {
		t.Fatal("server with url and command accepted")
	}
}

func TestTOMLKeyAndStringEscaping(t *testing.T) {
	if tomlKey("docs") != "docs" || tomlKey("a.b") != `"a.b"` {
		t.Fatal("tomlKey")
	}
	if got := tomlStr("a\"b\\c\nd\x01"); got != `"a\"b\\c\nd\u0001"` {
		t.Fatalf("tomlStr = %s", got)
	}
}

func TestGeminiCommandTOMLEscapesPrompt(t *testing.T) {
	got := string(geminiCommandTOML(Command{Name: "x", Description: `say "hi"`, Body: "run $ARGUMENTS with \\ and \"\"\" fences"}))
	contains(t, got, `description = "say \"hi\""`)
	contains(t, got, `{{args}}`)
	contains(t, got, `\\`)
	contains(t, got, `""\"`)
}
