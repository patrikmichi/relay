package agentsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
}

func TestProjectSourceSymlinksAreNeverRead(t *testing.T) {
	env := newEnv(t)
	secret := filepath.Join(env.Home, ".aws", "credentials")
	writeFile(t, secret, "aws_secret_access_key = TOPSECRET\n")
	src := SourceDir(env, ScopeProject)
	writeFile(t, filepath.Join(src, "rules", "good.md"), "Be terse.\n")
	link(t, secret, filepath.Join(src, "rules", "x.md"))
	link(t, secret, filepath.Join(src, "hooks.json"))

	b, warns, err := LoadBundle(src, SourceCheck(env, ScopeProject))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rules) != 1 || b.Rules[0].Name != "good" || len(b.Hooks) != 0 {
		t.Fatalf("bundle = %+v", b)
	}
	if got := strings.Join(warns, "\n"); !strings.Contains(got, "x.md") || !strings.Contains(got, "hooks.json") || strings.Contains(got, "TOPSECRET") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestProjectSourceDirectorySymlinkIsRefused(t *testing.T) {
	env := newEnv(t)
	outside := filepath.Join(env.Home, "notes")
	writeFile(t, filepath.Join(outside, "rules", "stolen.md"), "TOPSECRET\n")
	link(t, outside, SourceDir(env, ScopeProject))

	b, warns, err := LoadBundle(SourceDir(env, ScopeProject), SourceCheck(env, ScopeProject))
	if err != nil || len(b.Rules) != 0 || len(warns) == 0 {
		t.Fatalf("bundle=%+v warns=%v err=%v", b, warns, err)
	}
	if _, err := LoadMCPSource(env, ScopeProject); err == nil {
		t.Fatal("a symlinked mcp.json source was read")
	}
}

func TestProjectMCPSourceSymlinkIsRefused(t *testing.T) {
	env := newEnv(t)
	secret := filepath.Join(env.Home, "mcp-from-elsewhere.json")
	writeFile(t, secret, `{"servers":[{"name":"x","command":"sh"}]}`)
	link(t, secret, MCPSourcePath(env, ScopeProject))
	if src, err := LoadMCPSource(env, ScopeProject); err == nil || len(src.Servers) != 0 {
		t.Fatalf("src=%+v err=%v", src, err)
	}
}

func TestUserSourceSymlinkMustStayInHome(t *testing.T) {
	env := newEnv(t)
	src := SourceDir(env, ScopeUser)
	outside := filepath.Join(t.TempDir(), "shared.md")
	writeFile(t, outside, "outside home\n")
	inside := filepath.Join(env.Home, "dotfiles", "style.md")
	writeFile(t, inside, "inside home\n")
	link(t, outside, filepath.Join(src, "rules", "out.md"))
	link(t, inside, filepath.Join(src, "rules", "in.md"))

	b, warns, err := LoadBundle(src, SourceCheck(env, ScopeUser))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rules) != 1 || b.Rules[0].Name != "in" {
		t.Fatalf("rules = %+v", b.Rules)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "out.md") {
		t.Fatalf("warnings = %v", warns)
	}
}
