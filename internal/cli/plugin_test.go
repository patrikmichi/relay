package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/plugins"
	"github.com/patrikmichi/zuano/internal/signing"
)

const testSkill = "---\nname: triage\ndescription: Triage issues\n---\n# Triage\n"

func bundleFiles(withHooks bool) map[string][]byte {
	files := map[string][]byte{
		"plugin.json":            []byte(`{"name":"toolkit","version":"1.2.0"}`),
		"skills/triage/SKILL.md": []byte(testSkill),
		"commands/review.md":     []byte("---\ndescription: Review a change\n---\nReview it.\n"),
		"rules/style.md":         []byte("---\ndescription: Style\nalways: true\n---\nBe terse.\n"),
		"mcp.json":               []byte(`{"mcpServers":{"docs":{"type":"http","url":"https://docs.example.com/mcp"}}}`),
		"agents/reviewer.md":     []byte("---\nname: reviewer\ndescription: Reviews changes\n---\nYou review changes.\n"),
	}
	if withHooks {
		files["hooks/hooks.json"] = []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"./hooks/check.sh"}]}]}}`)
		files["hooks/check.sh"] = []byte("#!/bin/sh\nexit 0\n")
	}
	return files
}

type pluginGW struct {
	t         *testing.T
	srv       *httptest.Server
	priv      ed25519.PrivateKey
	keys      []signing.Key
	mu        sync.Mutex
	version   string
	listed    bool
	hooks     bool
	tamper    string
	minted    int
	auths     []string
	reports   []map[string]any
	removed   []string
	gitMirror bool
	cache     map[string][]byte
}

func newPluginGW(t *testing.T) *pluginGW {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	g := &pluginGW{t: t, priv: priv, version: "1.2.0", listed: true, hooks: true, gitMirror: true,
		keys: []signing.Key{{KID: "2026-10", Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(pub)}}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	withAPIKeyGateway(t, g.srv)
	isolateAgentEnv(t, os.Getenv("HOME"))
	t.Setenv("ZUANO_AGENT", "")
	origin, err := config.NormalizeGatewayURL(g.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.PinSigningKeys(origin, g.keys); err != nil {
		t.Fatal(err)
	}
	return g
}

// archive returns the same bytes for the same inputs, as a real feed does.
func (g *pluginGW) archive(platform string) ([]byte, string) {
	key := fmt.Sprint(platform, g.version, g.hooks, g.tamper)
	if g.cache == nil {
		g.cache = map[string][]byte{}
	}
	ext := ".zip"
	if platform == platformOpencode {
		ext = ".tgz"
	}
	if data, ok := g.cache[key]; ok {
		return data, ext
	}
	data, ext := g.buildArchive(platform)
	g.cache[key] = data
	return data, ext
}

func (g *pluginGW) buildArchive(platform string) ([]byte, string) {
	files := bundleFiles(g.hooks)
	if g.tamper != "" {
		files["skills/triage/SKILL.md"] = []byte(g.tamper)
	}
	listed := map[string]any{}
	signed := bundleFiles(g.hooks)
	for n, b := range signed {
		listed[n] = sha256Hex(b)
	}
	doc := map[string]any{"schema": "plugin-signature/1", "plugin": "toolkit", "version": g.version, "platform": platform, "files": listed, "kid": "2026-10", "alg": "ed25519"}
	msg, err := signing.CanonicalJSON(doc)
	if err != nil {
		g.t.Fatal(err)
	}
	doc["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(g.priv, msg))
	sig, _ := json.Marshal(doc)
	files[plugins.SignatureFile] = sig

	var b bytes.Buffer
	if platform == platformOpencode {
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		for n, data := range files {
			_ = tw.WriteHeader(&tar.Header{Name: "package/" + n, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
			_, _ = tw.Write(data)
		}
		_ = tw.Close()
		_ = gz.Close()
		return b.Bytes(), ".tgz"
	}
	zw := zip.NewWriter(&b)
	for n, data := range files {
		w, _ := zw.Create(n)
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	return b.Bytes(), ".zip"
}

func (g *pluginGW) pkgURL(platform string) string {
	_, ext := g.archive(platform)
	return fmt.Sprintf("%s/m/acme/pkg/toolkit/%s/%s%s", g.srv.URL, g.version, platform, ext)
}

func (g *pluginGW) marketplace(platform string) string {
	archive, _ := g.archive(platform)
	if !g.listed {
		return `{"name":"zuano-acme","owner":{"name":"Acme"},"plugins":[]}`
	}
	return fmt.Sprintf(`{"name":"zuano-acme","owner":{"name":"Acme"},"plugins":[{"name":"toolkit","version":%q,"description":"Team toolkit","source":{"source":"url","url":%q,"sha256":%q}}]}`,
		g.version, g.pkgURL(platform), sha256Hex(archive))
}

func (g *pluginGW) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/m/") {
		g.auths = append(g.auths, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer fdt_x" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	switch {
	case r.URL.Path == "/api/org/plugin-feeds":
		fmt.Fprintf(w, `{"feed":{"slug":"acme","base_url":"%s/m/acme","npm_scope":"acme","git_mirror":%v,"channels":["stable","beta"]}}`, g.srv.URL, g.gitMirror)
	case r.URL.Path == "/api/cli/feed-token":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["kind"] == "api_key" {
			g.minted++
			fmt.Fprintf(w, `{"token":"fdk_device%d"}`, g.minted)
			return
		}
		_, _ = io.WriteString(w, `{"token":"fdt_x","expires_in":3600}`)
	case r.URL.Path == "/m/acme/claude/stable/marketplace.json":
		_, _ = io.WriteString(w, g.marketplace(platformClaude))
	case r.URL.Path == "/m/acme/agent-plugins/stable/marketplace.json":
		_, _ = io.WriteString(w, g.marketplace(platformAgentPlugins))
	case strings.HasPrefix(r.URL.Path, "/m/acme/pkg/toolkit/"):
		name := filepath.Base(r.URL.Path)
		platform := strings.TrimSuffix(strings.TrimSuffix(name, ".zip"), ".tgz")
		archive, _ := g.archive(platform)
		_, _ = w.Write(archive)
	case r.URL.Path == "/api/plugins/toolkit/requirements":
		_, _ = io.WriteString(w, `{"missing":["slack"]}`)
	case r.URL.Path == "/api/plugins/toolkit/packages":
		var items []string
		for _, p := range []string{platformGemini, platformOpencode, platformCline} {
			archive, _ := g.archive(p)
			items = append(items, fmt.Sprintf(`{"platform":%q,"state":"ready","sha256":%q,"url":%q}`, p, sha256Hex(archive), g.pkgURL(p)))
		}
		fmt.Fprintf(w, `{"packages":[%s]}`, strings.Join(items, ","))
	case r.URL.Path == "/api/plugins/installs" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.reports = append(g.reports, body)
		_, _ = io.WriteString(w, `{"install":{"id":"pli_1"}}`)
	case strings.HasPrefix(r.URL.Path, "/api/plugins/installs/") && r.Method == http.MethodDelete:
		g.removed = append(g.removed, strings.TrimPrefix(r.URL.Path, "/api/plugins/installs/"))
		w.WriteHeader(http.StatusNoContent)
	default:
		g.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

type ranCommand struct {
	name string
	args []string
	env  []string
}

// fakeAgentBinaries stands in for the agents' own CLIs and records what was run.
func fakeAgentBinaries(t *testing.T, present ...string) *[]ranCommand {
	t.Helper()
	var ran []ranCommand
	oldLook, oldRun := lookAgentBinary, runAgentBinary
	lookAgentBinary = func(name string) (string, error) {
		for _, p := range present {
			if p == name {
				return "/fake/" + name, nil
			}
		}
		return "", os.ErrNotExist
	}
	runAgentBinary = func(_ context.Context, env []string, _ io.Writer, name string, args ...string) error {
		ran = append(ran, ranCommand{name, args, env})
		return nil
	}
	t.Cleanup(func() { lookAgentBinary, runAgentBinary = oldLook, oldRun })
	return &ran
}

func readHomeFile(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{os.Getenv("HOME")}, parts...)...))
	if err != nil {
		t.Fatalf("read %v: %v", parts, err)
	}
	return string(data)
}

func loadTestLedger(t *testing.T) *plugins.Ledger {
	t.Helper()
	l, err := plugins.LoadLedger(plugins.LedgerPath(os.Getenv("HOME")))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPluginInstall_CursorUnpacksAVerifiedPackageAndRecordsIt(t *testing.T) {
	g := newPluginGW(t)
	out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "cursor")
	if err == nil || !strings.Contains(out, "repeat with --allow-hooks") || !strings.Contains(out, "hook: ./hooks/check.sh") {
		t.Fatalf("without --allow-hooks: err %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".cursor")); statErr == nil {
		t.Fatal("something was written before the hooks were allowed")
	}

	out, err = runCommand(t, PluginCmd(), "install", "toolkit", "--to", "cursor", "--allow-hooks")
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verified toolkit 1.2.0") || !strings.Contains(out, "Installed toolkit 1.2.0 for cursor (native)") {
		t.Errorf("output = %s", out)
	}
	if got := readHomeFile(t, ".cursor", "plugins", "local", "toolkit", "skills", "triage", "SKILL.md"); got != testSkill {
		t.Errorf("skill = %q", got)
	}
	if info, _ := os.Stat(filepath.Join(os.Getenv("HOME"), ".cursor", "plugins", "local", "toolkit", "hooks", "check.sh")); info == nil || info.Mode().Perm() != 0o755 {
		t.Errorf("hook script mode wrong: %v", info)
	}
	in, ok := loadTestLedger(t).Get("toolkit", "cursor")
	if !ok || in.Version != "1.2.0" || in.Method != "native" || len(in.Files) != len(bundleFiles(true)) || in.CommandsSHA256 == "" || in.InstallID != "pli_1" {
		t.Errorf("ledger = %+v", in)
	}
	for _, a := range g.auths {
		if a != "Bearer fdt_x" {
			t.Errorf("feed request carried %q", a)
		}
	}
	if len(g.reports) != 1 || g.reports[0]["agent"] != "cursor" || g.reports[0]["method"] != "native" {
		t.Errorf("reports = %v", g.reports)
	}
}

func TestPluginInstall_ATamperedOrForeignSignedPackageExits4AndInstallsNothing(t *testing.T) {
	g := newPluginGW(t)
	g.tamper = "# tampered"
	_, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "cursor", "--allow-hooks")
	if got := exitCodeOf(err); got != 4 || !strings.Contains(err.Error(), "does not match its signed hash") {
		t.Fatalf("tampered: exit %d (%v)", got, err)
	}
	g.tamper = ""
	other, _, _ := ed25519.GenerateKey(nil)
	g.keys = []signing.Key{{KID: "2026-10", Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(other)}}
	origin, _ := config.NormalizeGatewayURL(g.srv.URL)
	if err := config.PinSigningKeys(origin, g.keys); err != nil {
		t.Fatal(err)
	}
	_, err = runCommand(t, PluginCmd(), "install", "toolkit", "--to", "cursor", "--allow-hooks")
	if got := exitCodeOf(err); got != 4 {
		t.Fatalf("wrong key: exit %d (%v)", got, err)
	}
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".cursor")); statErr == nil {
		t.Error("files were written from a package that failed verification")
	}
	if len(loadTestLedger(t).Plugins()) != 0 {
		t.Error("a failed install was recorded")
	}
}

func TestPluginInstall_ClaudeRegistersTheFeedAndRunsTheAgentsOwnInstaller(t *testing.T) {
	newPluginGW(t)
	fakeAgentBinaries(t)
	if out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "claude", "--allow-hooks"); err == nil || !strings.Contains(out, "claude was not found") {
		t.Fatalf("missing binary: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")); statErr == nil {
		t.Fatal("settings were edited although claude is not installed")
	}

	ran := fakeAgentBinaries(t, "claude")
	settings := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{\n  // keep me\n  \"theme\": \"dark\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "claude", "--allow-hooks")
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	got := readHomeFile(t, ".claude", "settings.json")
	for _, want := range []string{"// keep me", `"theme": "dark"`, `"extraKnownMarketplaces"`, `"zuano-acme"`, `"headersHelper": "zuano auth header --json"`, "/m/acme/claude/stable/marketplace.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("settings lack %q:\n%s", want, got)
		}
	}
	if len(*ran) != 1 || (*ran)[0].name != "claude" || strings.Join((*ran)[0].args, " ") != "plugin install toolkit@zuano-acme" {
		t.Errorf("ran %+v", *ran)
	}
	if in, _ := loadTestLedger(t).Get("toolkit", "claude"); in.Marketplace != "zuano-acme" {
		t.Errorf("ledger = %+v", in)
	}

	*ran = nil
	if out, err = runCommand(t, PluginCmd(), "remove", "toolkit", "--to", "claude"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if len(*ran) != 1 || strings.Join((*ran)[0].args, " ") != "plugin uninstall toolkit@zuano-acme" {
		t.Errorf("remove ran %+v", *ran)
	}
	if len(loadTestLedger(t).Plugins()) != 0 {
		t.Error("the ledger still lists the plugin")
	}
}

func TestPluginInstall_GeminiAndCodexUseTheGitMirrorThroughTheCredentialHelper(t *testing.T) {
	g := newPluginGW(t)
	ran := fakeAgentBinaries(t, "gemini", "codex")
	if out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "gemini,codex", "--allow-hooks"); err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	if len(*ran) != 3 {
		t.Fatalf("ran %+v", *ran)
	}
	gem := (*ran)[0]
	if gem.name != "gemini" || strings.Join(gem.args, " ") != "extensions install "+g.srv.URL+"/m/acme/git/toolkit.git --ref stable --consent" {
		t.Errorf("gemini ran %+v", gem)
	}
	env := strings.Join(gem.env, "\n")
	for _, want := range []string{"GIT_CONFIG_KEY_0=credential." + g.srv.URL + "/m/.helper", "auth git-credential", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, want) {
			t.Errorf("git env lacks %q:\n%s", want, env)
		}
	}
	if strings.Join((*ran)[1].args, " ") != "plugin marketplace add "+g.srv.URL+"/m/acme/git/stable.git" || strings.Join((*ran)[2].args, " ") != "plugin add toolkit@zuano-acme" {
		t.Errorf("codex ran %+v %+v", (*ran)[1], (*ran)[2])
	}

	g.gitMirror = false
	if out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "codex", "--allow-hooks"); err == nil || !strings.Contains(out, "git mirror is not enabled") {
		t.Errorf("no mirror: %v\n%s", err, out)
	}
}

func TestPluginInstall_CodexRemovesWithPluginRemoveAndClineInstallsFromADirectory(t *testing.T) {
	newPluginGW(t)
	ran := fakeAgentBinaries(t, "codex", "cline")
	var clineDir string
	old := runAgentBinary
	runAgentBinary = func(ctx context.Context, env []string, out io.Writer, name string, args ...string) error {
		if name == "cline" {
			clineDir = args[len(args)-1]
			if data, err := os.ReadFile(filepath.Join(clineDir, "skills", "triage", "SKILL.md")); err != nil || string(data) != testSkill {
				t.Errorf("cline was not given the unpacked package: %v %q", err, data)
			}
		}
		return old(ctx, env, out, name, args...)
	}
	t.Cleanup(func() { runAgentBinary = old })

	if out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "codex,cline", "--allow-hooks"); err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	var cline *ranCommand
	for i := range *ran {
		if (*ran)[i].name == "cline" {
			cline = &(*ran)[i]
		}
	}
	if cline == nil || strings.Join(cline.args[:2], " ") != "plugin install" || clineDir == "" {
		t.Fatalf("cline ran %+v", cline)
	}
	if _, err := os.Stat(clineDir); err == nil {
		t.Error("the unpacked package was left behind")
	}

	*ran = nil
	if out, err := runCommand(t, PluginCmd(), "remove", "toolkit", "--to", "codex,cline"); err != nil || !strings.Contains(out, "Cline has no command to remove toolkit") && !strings.Contains(out, "Removed toolkit from cline") {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if len(*ran) != 1 || (*ran)[0].name != "codex" || strings.Join((*ran)[0].args, " ") != "plugin remove toolkit@zuano-acme" {
		t.Errorf("remove ran %+v", *ran)
	}
}

func TestPluginInstall_AnAgentWithoutAPluginSystemGetsTheBundleExpanded(t *testing.T) {
	newPluginGW(t)
	home := os.Getenv("HOME")
	out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "windsurf", "--allow-hooks")
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	in, ok := loadTestLedger(t).Get("toolkit", "windsurf")
	if !ok || in.Method != "expanded" {
		t.Fatalf("ledger = %+v", in)
	}
	if len(in.Skills) != 1 || in.Skills[0] != "triage" {
		t.Errorf("skills = %v", in.Skills)
	}
	if len(in.Hooks) != 1 || in.Hooks[0].Event != "pre-tool-use" || in.Hooks[0].Matcher != "Bash" || !strings.HasSuffix(in.Hooks[0].Command, filepath.Join("toolkit", "hooks", "check.sh")+"'") {
		t.Errorf("hooks = %+v", in.Hooks)
	}
	hooksFile := readHomeFile(t, ".config", "zuano", "sync", "hooks.json")
	if !strings.Contains(hooksFile, `"agents": [`) || !strings.Contains(hooksFile, `"windsurf"`) || !strings.Contains(hooksFile, `"event": "pre-tool-use"`) {
		t.Errorf("hooks.json = %s", hooksFile)
	}
	script := filepath.Join(home, ".config", "zuano", "plugins", "files", "toolkit", "hooks", "check.sh")
	if info, err := os.Stat(script); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("hook script = %v, %v", info, err)
	}
	if !strings.Contains(out, "no subagent format zuano can write") {
		t.Errorf("no note about subagents on windsurf:\n%s", out)
	}
	source := filepath.Join(home, ".config", "zuano", "sync")
	if got := readHomeFile(t, ".config", "zuano", "sync", "commands", "toolkit--review.md"); !strings.Contains(got, "Review it.") {
		t.Errorf("command = %q", got)
	}
	if got := readHomeFile(t, ".config", "zuano", "sync", "rules", "toolkit--style.md"); !strings.Contains(got, "Be terse.") {
		t.Errorf("rule = %q", got)
	}
	if got := readHomeFile(t, ".config", "zuano", "sync", "mcp.json"); !strings.Contains(got, "toolkit-docs") || !strings.Contains(got, "https://docs.example.com/mcp") {
		t.Errorf("mcp source = %q", got)
	}
	if !strings.Contains(out, "agents and hooks are not expanded") && !strings.Contains(out, "Installed toolkit") {
		t.Errorf("output = %s", out)
	}
	var skillFile string
	for _, f := range in.Files {
		if strings.HasSuffix(f.Path, filepath.Join("triage", "SKILL.md")) {
			skillFile = f.Path
		}
	}
	if skillFile == "" {
		t.Fatalf("no skill file in the ledger: %+v", in.Files)
	}
	if _, err := os.Stat(skillFile); err != nil {
		t.Errorf("skill not written: %v", err)
	}

	if out, err = runCommand(t, PluginCmd(), "verify", "toolkit"); err != nil || !strings.Contains(out, "No drift") {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(source, "commands", "toolkit--review.md"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err = runCommand(t, PluginCmd(), "verify", "toolkit"); err == nil || !strings.Contains(out, "toolkit--review.md was changed") {
		t.Fatalf("drift: %v\n%s", err, out)
	}

	if out, err = runCommand(t, PluginCmd(), "remove", "toolkit"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if _, err := os.Stat(skillFile); err == nil {
		t.Error("the skill survived removal")
	}
	if _, err := os.Stat(filepath.Join(source, "commands", "toolkit--review.md")); err != nil {
		t.Log("the edited command was kept, as intended")
	} else if got := readHomeFile(t, ".config", "zuano", "sync", "commands", "toolkit--review.md"); got != "edited" {
		t.Errorf("a changed file was replaced: %q", got)
	}
	if got := readHomeFile(t, ".config", "zuano", "sync", "mcp.json"); strings.Contains(got, "toolkit-docs") {
		t.Errorf("the MCP server survived removal: %q", got)
	}
	if len(loadTestLedger(t).Plugins()) != 0 {
		t.Error("the ledger still lists the plugin")
	}
}

func TestPluginInstall_OpencodeWritesTheNPMScopeOnceAndListsThePackage(t *testing.T) {
	g := newPluginGW(t)
	if out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "opencode", "--allow-hooks"); err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	npmrc := readHomeFile(t, ".config", "opencode", ".npmrc")
	host := strings.TrimPrefix(g.srv.URL, "http://")
	for _, want := range []string{"@acme:registry=" + g.srv.URL + "/m/acme/npm/", "//" + host + "/m/acme/npm/:_authToken=fdk_device1"} {
		if !strings.Contains(npmrc, want) {
			t.Errorf(".npmrc lacks %q:\n%s", want, npmrc)
		}
	}
	if info, _ := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "opencode", ".npmrc")); info.Mode().Perm() != 0o600 {
		t.Errorf(".npmrc mode = %v", info.Mode().Perm())
	}
	if cfg := readHomeFile(t, ".config", "opencode", "opencode.json"); !strings.Contains(cfg, "@acme/toolkit") {
		t.Errorf("config = %s", cfg)
	}
	in, _ := loadTestLedger(t).Get("toolkit", "opencode")
	if len(in.Subagents) != 1 || in.Subagents[0] != "reviewer" || len(in.Hooks) != 1 {
		t.Fatalf("ledger = %+v", in)
	}
	var agentFile string
	for _, f := range in.Files {
		if strings.HasSuffix(f.Path, "reviewer.md") {
			agentFile = f.Path
		}
	}
	if data, err := os.ReadFile(agentFile); err != nil || !strings.Contains(string(data), "You review changes.") {
		t.Errorf("subagent file %q: %v", agentFile, err)
	}
	if _, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "opencode", "--allow-hooks"); err != nil {
		t.Fatal(err)
	}
	if hooks := readHomeFile(t, ".config", "zuano", "sync", "hooks.json"); strings.Count(hooks, "pre-tool-use") != 1 {
		t.Errorf("a second install duplicated the hook:\n%s", hooks)
	}
	if g.minted != 1 {
		t.Errorf("minted %d keys, want 1", g.minted)
	}
	if _, err := runCommand(t, PluginCmd(), "remove", "toolkit", "--to", "opencode"); err != nil {
		t.Fatal(err)
	}
	if cfg := readHomeFile(t, ".config", "opencode", "opencode.json"); strings.Contains(cfg, "@acme/toolkit") {
		t.Errorf("the package stayed in the config: %s", cfg)
	}
	if _, err := os.Stat(agentFile); err == nil {
		t.Error("the subagent survived removal")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "zuano", "sync", "hooks.json")); err == nil {
		t.Error("hooks.json is left although it held only the plugin's hook")
	}
}

func TestMergeHooks_ReplacesOnlyThePluginsOwnEntries(t *testing.T) {
	before := []byte(`{"note":"mine","hooks":[{"event":"stop","command":"notify"},{"event":"pre-tool-use","matcher":"Bash","command":"'/x/check.sh'","agents":["windsurf"]},{"event":"pre-tool-use","matcher":"Bash","command":"'/x/check.sh'","agents":["cursor"]}]}`)
	drop := []plugins.HookRef{{Event: "pre-tool-use", Matcher: "Bash", Command: "'/x/check.sh'"}}
	out, err := mergeHooks(before, drop, nil, "windsurf")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"note": "mine"`) || !strings.Contains(got, `"notify"`) || !strings.Contains(got, `"cursor"`) || strings.Contains(got, `"windsurf"`) {
		t.Errorf("merged = %s", got)
	}
	if _, err := mergeHooks([]byte(`{"hooks": 3}`), nil, nil, "windsurf"); err == nil {
		t.Error("a hooks member that is not a list was accepted")
	}
	if out, _ := mergeHooks([]byte(`{"hooks":[{"event":"stop","command":"n","agents":["windsurf"]}]}`), []plugins.HookRef{{Event: "stop", Command: "n"}}, nil, "windsurf"); out != nil {
		t.Errorf("an emptied file should go away, got %s", out)
	}
}

func TestPluginInstall_DryRunVerifiesButWritesNothing(t *testing.T) {
	g := newPluginGW(t)
	fakeAgentBinaries(t, "claude")
	out, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "claude,cursor", "--dry-run")
	if err != nil || !strings.Contains(out, "would install for cursor (native)") || !strings.Contains(out, "Verified toolkit 1.2.0") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if len(g.reports) != 0 {
		t.Error("a dry run reported an install")
	}
	if entries, _ := os.ReadDir(os.Getenv("HOME")); len(entries) != 0 {
		for _, e := range entries {
			if e.Name() != ".config" {
				t.Errorf("a dry run wrote %s", e.Name())
			}
		}
	}
	if len(loadTestLedger(t).Plugins()) != 0 {
		t.Error("a dry run wrote the ledger")
	}
}

func TestPluginUpdate_InstallsNewerVersionsAndRemovesRevokedOnes(t *testing.T) {
	g := newPluginGW(t)
	if _, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "cursor", "--allow-hooks"); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, PluginCmd(), "update")
	if err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("current: %v\n%s", err, out)
	}
	g.version = "1.3.0"
	if out, err = runCommand(t, PluginCmd(), "list"); err != nil || !strings.Contains(out, "update available (1.3.0)") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if out, err = runCommand(t, PluginCmd(), "update"); err != nil || !strings.Contains(out, "Installed toolkit 1.3.0 for cursor") {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if in, _ := loadTestLedger(t).Get("toolkit", "cursor"); in.Version != "1.3.0" {
		t.Errorf("ledger = %+v", in)
	}
	g.hooks = false
	g.version = "1.4.0"
	if out, err = runCommand(t, PluginCmd(), "update"); err != nil {
		t.Fatalf("update without hooks: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".cursor", "plugins", "local", "toolkit", "hooks", "check.sh")); statErr == nil {
		t.Error("a file the new version dropped was left behind")
	}

	g.listed = false
	out, err = runCommand(t, PluginCmd(), "update")
	if err != nil || !strings.Contains(out, "") {
		t.Fatalf("revoked: %v\n%s", err, out)
	}
	if len(loadTestLedger(t).Plugins()) != 0 {
		t.Error("a revoked plugin stayed installed")
	}
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".cursor", "plugins", "local", "toolkit", "plugin.json")); statErr == nil {
		t.Error("a revoked plugin's files stayed")
	}
	if len(g.removed) != 1 || g.removed[0] != "pli_1" {
		t.Errorf("gateway was told %v", g.removed)
	}
}

func TestPluginUpdate_NewCommandsNeedApprovalAgain(t *testing.T) {
	g := newPluginGW(t)
	g.hooks = false
	if _, err := runCommand(t, PluginCmd(), "install", "toolkit", "--to", "cursor"); err != nil {
		t.Fatalf("a bundle with no commands needs no flag: %v", err)
	}
	g.hooks = true
	g.version = "1.3.0"
	if out, err := runCommand(t, PluginCmd(), "update"); err == nil || !strings.Contains(out, "repeat with --allow-hooks") {
		t.Fatalf("update that adds a hook: %v\n%s", err, out)
	}
	if in, _ := loadTestLedger(t).Get("toolkit", "cursor"); in.Version != "1.2.0" {
		t.Errorf("the refused update changed the ledger: %+v", in)
	}
}

func TestPluginFeedsSetup_RegistersClaudeAndWritesTheNPMScope(t *testing.T) {
	newPluginGW(t)
	fakeAgentBinaries(t)
	out, err := runCommand(t, PluginCmd(), "feeds", "setup", "--to", "claude,opencode,cursor")
	if err != nil {
		t.Fatalf("err %v\n%s", err, out)
	}
	if got := readHomeFile(t, ".claude", "settings.json"); !strings.Contains(got, `"zuano-acme"`) {
		t.Errorf("claude settings = %s", got)
	}
	if got := readHomeFile(t, ".config", "opencode", ".npmrc"); !strings.Contains(got, ":_authToken=fdk_device1") {
		t.Errorf(".npmrc = %s", got)
	}
	if !strings.Contains(out, "cursor: nothing to register") {
		t.Errorf("output = %s", out)
	}
}

func TestPluginList_ReadsTheLedgerWithoutAGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZUANO_URL", "")
	t.Setenv("ZUANO_API_KEY", "")
	l, _ := plugins.LoadLedger(plugins.LedgerPath(home))
	l.Set("toolkit", "cursor", plugins.Install{Version: "1.2.0", Method: "native", Channel: "stable"})
	if err := l.Save(plugins.LedgerPath(home)); err != nil {
		t.Fatal(err)
	}
	out, err := runCommand(t, PluginCmd(), "list")
	if err != nil || !strings.Contains(out, "toolkit") || !strings.Contains(out, "unknown (offline)") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if _, err := runCommand(t, PluginCmd(), "list", "--available"); err == nil {
		t.Error("--available worked with no gateway")
	}
	if _, err := runCommand(t, PluginCmd(), "remove", "missing"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("remove err = %v", err)
	}
	if _, err := runCommand(t, PluginCmd(), "install", "Bad Name"); err == nil {
		t.Error("a bad name was accepted")
	}
}

func TestPackageCommands(t *testing.T) {
	pkg := &plugins.Package{Files: map[string][]byte{
		"hooks/hooks.json":  []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify --done"}]}]}}`),
		"hooks/run.sh":      []byte("x"),
		".mcp.json":         []byte(`{"mcpServers":{"local":{"command":"npx","args":["-y","pkg"]},"remote":{"type":"http","url":"https://x"}}}`),
		"skills/a/SKILL.md": []byte("x"),
	}}
	got := packageCommands(pkg, platformClaude)
	want := []string{"hook script hooks/run.sh", "hook: notify --done", "mcp server: npx -y pkg"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commands = %q, want %q", got, want)
	}
	if got := packageCommands(&plugins.Package{Files: map[string][]byte{}}, platformOpencode); len(got) != 1 {
		t.Errorf("opencode commands = %q", got)
	}
	if got := packageCommands(&plugins.Package{Files: map[string][]byte{"skills/a/SKILL.md": []byte("x")}}, platformClaude); len(got) != 0 {
		t.Errorf("a plain bundle lists %q", got)
	}
}
