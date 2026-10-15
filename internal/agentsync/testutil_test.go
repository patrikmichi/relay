package agentsync

import (
	"context"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// newEnv returns an Env rooted in temporary directories and points HOME at
// the temporary home so the transaction store never touches the real one.
func newEnv(t *testing.T) Env {
	t.Helper()
	root := t.TempDir()
	home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
	for _, d := range []string{home, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	return Env{Home: home, Project: project, Getenv: func(string) string { return "" }}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func fixtureBundle() Bundle {
	return Bundle{
		MCP: []MCPServer{
			{Name: "relay", URL: "https://gateway.example.com/api/mcp", BearerEnv: "GATEWAY_API_KEY"},
			{Name: "docs", Command: "npx", Args: []string{"-y", "@acme/docs-mcp"}, Env: map[string]string{"DOCS_REGION": "eu"}},
		},
		Hooks: []Hook{
			{Event: EventPreToolUse, Matcher: "Bash", Command: "/opt/relay/guard.sh", Timeout: 10},
			{Event: EventSessionStart, Command: "/opt/relay/context.sh"},
			{Event: EventStop, Command: "/opt/relay/notify.sh"},
			{Event: EventNotification, Command: "/opt/relay/ping.sh"},
		},
		Rules: []Rule{
			{Name: "style", Description: "Code style", Always: true, Body: "Prefer small functions."},
			{Name: "tests", Description: "Test rules", Globs: []string{"**/*_test.go"}, Body: "Use table tests."},
		},
		Commands: []Command{{Name: "review", Description: "Review the diff", ArgumentHint: "[path]", Body: "Review $ARGUMENTS for bugs."}},
	}
}

func syncAll(t *testing.T, env Env, bundles map[Scope]Bundle, opts Options) *Plan {
	t.Helper()
	plan, err := Build(env, bundles, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := plan.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return plan
}

func statusOf(items []Item, kind Kind, name string) string {
	for _, it := range items {
		if it.Kind == kind && it.Name == name {
			return it.Status
		}
	}
	return ""
}

// snapshot returns every regular file under root keyed by slash path,
// skipping relay's own state.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".config/relay" {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertGolden(t *testing.T, name string, got map[string]string) {
	t.Helper()
	dir := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		for rel, content := range got {
			writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), content)
		}
		return
	}
	want := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		rel, _ := filepath.Rel(dir, path)
		want[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	var names []string
	for k := range got {
		names = append(names, k)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		g, gok := got[k]
		w, wok := want[k]
		switch {
		case !wok:
			t.Errorf("%s: unexpected file %s:\n%s", name, k, g)
		case !gok:
			t.Errorf("%s: missing file %s", name, k)
		case g != w:
			t.Errorf("%s: %s differs\n--- want\n%s\n--- got\n%s", name, k, w, g)
		}
	}
}

func contains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %q in:\n%s", needle, haystack)
	}
}
