package agentsync

import (
	"os"
	"path/filepath"
	"testing"
)

func withOS(t *testing.T, goos string) {
	t.Helper()
	orig := hostOS
	hostOS = goos
	t.Cleanup(func() { hostOS = orig })
}

func envWith(vars map[string]string) Env {
	return Env{Home: filepath.FromSlash("/home/u"), Getenv: func(k string) string { return vars[k] }}
}

func TestWindsurfConfigLivesUnderAppDataOnWindows(t *testing.T) {
	withOS(t, "windows")
	appData := filepath.FromSlash("/home/u/AppData/Roaming")
	got := windsurfMCPPaths(envWith(map[string]string{"APPDATA": appData}))[0]
	if want := filepath.Join(appData, "devin", "mcp_config.json"); got != want {
		t.Errorf("windsurf path = %s, want %s", got, want)
	}
	if got := windsurfMCPPaths(envWith(nil))[0]; got != filepath.Join(filepath.FromSlash("/home/u/AppData/Roaming"), "devin", "mcp_config.json") {
		t.Errorf("without APPDATA = %s", got)
	}
}

func TestClineSettingsPathFollowsTheOS(t *testing.T) {
	for goos, want := range map[string][]string{
		"linux":   {"/home/u", ".config", "Code", "User", "globalStorage"},
		"darwin":  {"/home/u", "Library", "Application Support", "Code", "User", "globalStorage"},
		"windows": {"/home/u", "AppData", "Roaming", "Code", "User", "globalStorage"},
	} {
		withOS(t, goos)
		if got, w := vscodeGlobalStorage(envWith(nil)), filepath.Join(filepath.FromSlash(want[0]), filepath.Join(want[1:]...)); got != w {
			t.Errorf("%s: %s, want %s", goos, got, w)
		}
	}
	withOS(t, "linux")
	xdg := filepath.FromSlash("/custom/xdg")
	if got := vscodeGlobalStorage(envWith(map[string]string{"XDG_CONFIG_HOME": xdg})); got != filepath.Join(xdg, "Code", "User", "globalStorage") {
		t.Errorf("XDG_CONFIG_HOME ignored: %s", got)
	}
}

func TestOpenCodeHonoursXDGConfigHome(t *testing.T) {
	env := newEnv(t)
	xdg := filepath.Join(t.TempDir(), "xdg")
	env.Getenv = func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return xdg
		}
		return ""
	}
	if err := os.MkdirAll(filepath.Join(xdg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Detected(env); len(got) != 1 || got[0] != OpenCode {
		t.Fatalf("detected = %v, want opencode found under XDG_CONFIG_HOME", got)
	}
	syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{OpenCode}, Scopes: []Scope{ScopeUser}})
	for _, rel := range []string{"opencode.json", "AGENTS.md", "commands/review.md", "plugins/relay-hooks.js"} {
		if !fileExists(filepath.Join(xdg, "opencode", filepath.FromSlash(rel))) {
			t.Errorf("%s not written under XDG_CONFIG_HOME", rel)
		}
	}
	if dirExists(filepath.Join(env.Home, ".config", "opencode")) {
		t.Error("files were written under ~/.config although XDG_CONFIG_HOME is set")
	}
}
