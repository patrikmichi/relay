package agentsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectOpts(agents ...AgentID) Options {
	return Options{Agents: agents, Scopes: []Scope{ScopeProject}}
}

func TestProjectSymlinkToUserConfigIsRefused(t *testing.T) {
	env := newEnv(t)
	userCfg := filepath.Join(env.Home, ".claude.json")
	writeFile(t, userCfg, "{}\n")
	if err := os.Symlink(userCfg, filepath.Join(env.Project, ".mcp.json")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	plan := syncAll(t, env,map[Scope]Bundle{ScopeProject: fixtureBundle()}, projectOpts(Claude))
	if got := readFile(t, userCfg); got != "{}\n" {
		t.Fatalf("project symlink redirected a write into the user config:\n%s", got)
	}
	var skipped bool
	for _, it := range plan.Items {
		skipped = skipped || (it.Status == StatusSkipped && strings.Contains(it.Detail, "symlink"))
	}
	if !skipped {
		t.Fatalf("refusal was not reported: %+v", plan.Items)
	}
}

func TestProjectSymlinkedDirectoryIsRefused(t *testing.T) {
	env := newEnv(t)
	outside := filepath.Join(env.Home, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(env.Project, ".claude")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, projectOpts(Claude))
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write escaped through a symlinked project directory: %v", entries)
	}
}

func TestUserSymlinkOutsideHomeIsRefused(t *testing.T) {
	env := newEnv(t)
	real := filepath.Join(t.TempDir(), "dotfiles", "claude.json")
	writeFile(t, real, "{}\n")
	if err := os.Symlink(real, filepath.Join(env.Home, ".claude.json")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	if got := readFile(t, real); got != "{}\n" {
		t.Fatalf("symlink target outside home was modified:\n%s", got)
	}
}
