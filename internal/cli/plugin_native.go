package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/patrikmichi/zuano/internal/agentsync"
	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/plugins"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// pluginAgent describes how a plugin reaches one coding agent.
type pluginAgent struct {
	id string
	// platform is the package built for this agent.
	platform string
	// method is "native" when the agent has its own plugin system, "expanded"
	// when zuano writes the bundle's parts into the agent's configuration.
	method  string
	install func(s *pluginSession, a *pluginAgent, t *pluginTarget) (plugins.Install, error)
	remove  func(s *pluginSession, a *pluginAgent, name string, in plugins.Install) error
}

var pluginAgentTable = []*pluginAgent{
	{id: "claude", platform: platformClaude, method: "native", install: installClaude, remove: removeClaude},
	{id: "codex", platform: platformAgentPlugins, method: "native", install: installCodex, remove: removeCodex},
	{id: "copilot", platform: platformAgentPlugins, method: "native", install: installCopilot, remove: removeViaCLI("copilot", "plugin", "uninstall")},
	{id: "vscode", platform: platformAgentPlugins, method: "native", install: installVSCode, remove: removeVSCode},
	{id: "cursor", platform: platformAgentPlugins, method: "native", install: installCursor, remove: removeLedgerFiles},
	{id: "gemini-cli", platform: platformGemini, method: "native", install: installGemini, remove: removeViaCLI("gemini", "extensions", "uninstall")},
	{id: "opencode", platform: platformOpencode, method: "native", install: installOpencode, remove: removeOpencode},
	{id: "cline", platform: platformCline, method: "native", install: installCline, remove: removeCline},
	{id: "windsurf", platform: platformAgentPlugins, method: "expanded", install: installExpandedOnly, remove: removeExpanded},
}

func pluginAgentByID(id string) *pluginAgent {
	for _, a := range pluginAgentTable {
		if a.id == id {
			return a
		}
	}
	return nil
}

func pluginAgentIDs() []string {
	ids := make([]string, len(pluginAgentTable))
	for i, a := range pluginAgentTable {
		ids[i] = a.id
	}
	return ids
}

// Seams for tests: how an agent's own binary is found and run.
var (
	lookAgentBinary = exec.LookPath
	runAgentBinary  = func(ctx context.Context, env []string, out io.Writer, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout, cmd.Stderr = out, out
		return cmd.Run()
	}
)

// run executes an agent's binary with explicit arguments, never through a shell.
func (s *pluginSession) run(env []string, name string, args ...string) error {
	if _, err := lookAgentBinary(name); err != nil {
		return fmt.Errorf("%s was not found on PATH; install it first", name)
	}
	fmt.Fprintf(s.err, "  $ %s %s\n", name, textsafe.Clean(strings.Join(args, " ")))
	if err := runAgentBinary(s.ctx, env, s.err, name, args...); err != nil {
		return fmt.Errorf("%s %s failed: %w", name, args[0], err)
	}
	return nil
}

func (s *pluginSession) hasBinary(name string) bool {
	_, err := lookAgentBinary(name)
	return err == nil
}

// gitEnv makes git ask zuano for credentials for the gateway's /m/ paths and
// nowhere else, without touching the user's git configuration.
func (s *pluginSession) gitEnv() []string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = brand.Name
	}
	helper := "!'" + strings.ReplaceAll(exe, "'", `'\''`) + "' auth git-credential"
	scope := "credential." + s.origin + "/m/."
	return []string{
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=" + scope + "helper", "GIT_CONFIG_VALUE_0=" + helper,
		"GIT_CONFIG_KEY_1=" + scope + "useHttpPath", "GIT_CONFIG_VALUE_1=true",
	}
}

func (s *pluginSession) requireGitMirror() error {
	if !s.feed.GitMirror {
		return errors.New("the gateway's git mirror is not enabled for your organisation, and this agent installs from it")
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- Claude Code ----

func installClaude(s *pluginSession, _ *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	if !s.hasBinary("claude") {
		return plugins.Install{}, errors.New("claude was not found on PATH; install Claude Code first")
	}
	m, err := s.marketplace(platformClaude, t.channel)
	if err != nil {
		return plugins.Install{}, err
	}
	settings := filepath.Join(s.home, ".claude", "settings.json")
	if err := editSettings(settings, func(d []byte) ([]byte, error) {
		return agentsync.SetJSONMember(d, []string{"extraKnownMarketplaces"}, m.Name, map[string]any{
			"source":        map[string]any{"source": "url", "url": s.marketplaceURL(platformClaude, t.channel)},
			"headersHelper": brand.Cmd("auth header --json"),
		})
	}); err != nil {
		return plugins.Install{}, err
	}
	if err := s.run(nil, "claude", "plugin", "install", t.name+"@"+m.Name); err != nil {
		return plugins.Install{}, err
	}
	return plugins.Install{Method: "native", Marketplace: m.Name}, nil
}

func removeClaude(s *pluginSession, _ *pluginAgent, name string, in plugins.Install) error {
	return s.run(nil, "claude", "plugin", "uninstall", name+"@"+in.Marketplace)
}

// removeViaCLI removes a plugin by running the agent's own uninstall command.
func removeViaCLI(bin string, verb ...string) func(*pluginSession, *pluginAgent, string, plugins.Install) error {
	return func(s *pluginSession, _ *pluginAgent, name string, _ plugins.Install) error {
		return s.run(nil, bin, append(slices.Clone(verb), name)...)
	}
}

// ---- Codex, Copilot CLI, VS Code: the git mirror ----

func installCodex(s *pluginSession, _ *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	return installFromGitMarketplace(s, t, "codex", "add")
}

func installCopilot(s *pluginSession, _ *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	return installFromGitMarketplace(s, t, "copilot", "install")
}

// installFromGitMarketplace adds the feed's git mirror as a marketplace and
// installs the plugin from it; the two CLIs name the install verb differently.
func installFromGitMarketplace(s *pluginSession, t *pluginTarget, bin, installVerb string) (plugins.Install, error) {
	if err := s.requireGitMirror(); err != nil {
		return plugins.Install{}, err
	}
	m, err := s.marketplace(platformClaude, t.channel)
	if err != nil {
		return plugins.Install{}, err
	}
	env := s.gitEnv()
	if err := s.run(env, bin, "plugin", "marketplace", "add", s.gitURL("", t.channel)); err != nil {
		return plugins.Install{}, err
	}
	if err := s.run(env, bin, "plugin", installVerb, t.name+"@"+m.Name); err != nil {
		return plugins.Install{}, err
	}
	return plugins.Install{Method: "native", Marketplace: m.Name}, nil
}

func removeCodex(s *pluginSession, _ *pluginAgent, name string, in plugins.Install) error {
	return s.run(nil, "codex", "plugin", "remove", name+"@"+in.Marketplace)
}

func vscodeSettingsPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Code", "User", "settings.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "settings.json")
	}
	return filepath.Join(home, ".config", "Code", "User", "settings.json")
}

func installVSCode(s *pluginSession, _ *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	if err := s.requireGitMirror(); err != nil {
		return plugins.Install{}, err
	}
	const key = "chat.plugins.marketplaces"
	repo := s.gitURL("", t.channel)
	if err := editSettings(vscodeSettingsPath(s.home), func(d []byte) ([]byte, error) {
		cur, _, err := agentsync.ReadJSONValue(d, nil, key)
		if err != nil {
			return nil, err
		}
		list, _ := cur.([]any)
		if slices.Contains(list, any(repo)) {
			return d, nil
		}
		return agentsync.SetJSONMember(d, nil, key, append(slices.Clone(list), repo))
	}); err != nil {
		return plugins.Install{}, err
	}
	fmt.Fprintf(s.err, "VS Code installs plugins itself: open the plugins view and install %s from the %s marketplace. Run `%s` once so git can sign in to the gateway.\n", textsafe.Clean(t.name), textsafe.Clean(s.feed.Slug), brand.Cmd("plugin feeds setup --to vscode"))
	return plugins.Install{Method: "native"}, nil
}

func removeVSCode(s *pluginSession, _ *pluginAgent, name string, _ plugins.Install) error {
	fmt.Fprintf(s.err, "VS Code owns the install of %s: remove it from the plugins view. zuano forgets it.\n", textsafe.Clean(name))
	return nil
}

// ---- Gemini CLI ----

func installGemini(s *pluginSession, _ *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	if err := s.requireGitMirror(); err != nil {
		return plugins.Install{}, err
	}
	// The commands the bundle runs were listed and approved before this point,
	// so Gemini's own consent prompt, which has no terminal here, is skipped.
	if err := s.run(s.gitEnv(), "gemini", "extensions", "install", s.gitURL(t.name, t.channel), "--ref", t.channel, "--consent"); err != nil {
		return plugins.Install{}, err
	}
	return plugins.Install{Method: "native"}, nil
}

// ---- Cursor: the unpacked package ----

func isExecutablePath(name string) bool {
	return strings.HasPrefix(name, "hooks/") || strings.HasSuffix(name, ".sh")
}

func installCursor(s *pluginSession, _ *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	dir := filepath.Join(s.home, ".cursor", "plugins", "local", t.name)
	owned := map[string]string{}
	if t.prior != nil {
		for _, f := range t.prior.Files {
			owned[f.Path] = f.SHA256
		}
	} else if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return plugins.Install{}, fmt.Errorf("%s already exists and was not installed by zuano; move it away first", textsafe.Clean(dir))
	}
	var refs []plugins.FileRef
	written := map[string]bool{}
	for _, name := range t.vp.pkg.Paths() {
		target, err := safeJoin(dir, name)
		if err != nil {
			return plugins.Install{}, err
		}
		data := t.vp.pkg.Files[name]
		if cur, err := os.ReadFile(target); err == nil {
			if want, ok := owned[target]; !ok || sha256Hex(cur) != want {
				if sha256Hex(cur) != sha256Hex(data) {
					return plugins.Install{}, fmt.Errorf("%s was changed since zuano wrote it; move it away or run `%s` first", textsafe.Clean(target), brand.Cmd("plugin remove "+t.name+" --to cursor"))
				}
			}
		}
		mode := os.FileMode(0o644)
		if isExecutablePath(name) {
			mode = 0o755
		}
		if err := writeFileAtomic(target, data, mode); err != nil {
			return plugins.Install{}, err
		}
		written[target] = true
		refs = append(refs, plugins.FileRef{Path: target, SHA256: sha256Hex(data)})
	}
	for path, want := range owned {
		if written[path] {
			continue
		}
		if cur, err := os.ReadFile(path); err == nil && sha256Hex(cur) == want {
			_ = os.Remove(path)
		} else if err == nil {
			fmt.Fprintf(s.err, "warning: kept %s, which was changed since zuano wrote it\n", textsafe.Clean(path))
		}
	}
	return plugins.Install{Method: "native", Files: refs}, nil
}

// safeJoin joins a package path under dir and refuses anything that leaves it.
func safeJoin(dir, rel string) (string, error) {
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if r, err := filepath.Rel(dir, target); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the package path %q leaves the install directory", textsafe.Clean(rel))
	}
	return target, nil
}

// removeLedgerFiles deletes the files an install wrote, but only those that
// still hold the bytes zuano wrote.
func removeLedgerFiles(s *pluginSession, _ *pluginAgent, _ string, in plugins.Install) error {
	var kept []string
	for _, f := range in.Files {
		cur, err := os.ReadFile(f.Path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return err
		case sha256Hex(cur) != f.SHA256:
			kept = append(kept, f.Path)
		default:
			if err := os.Remove(f.Path); err != nil {
				return err
			}
			pruneEmptyDirs(filepath.Dir(f.Path))
		}
	}
	for _, p := range kept {
		fmt.Fprintf(s.err, "kept %s: it was changed since zuano wrote it\n", textsafe.Clean(p))
	}
	return nil
}

// pruneEmptyDirs removes dir and its parents while they are empty, stopping
// at the first that is not.
func pruneEmptyDirs(dir string) {
	for i := 0; i < 8; i++ {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// ---- OpenCode: an npm package from the feed's registry ----

func opencodeConfigDir(home string) string { return filepath.Join(home, ".config", "opencode") }

func installOpencode(s *pluginSession, a *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	if s.feed.NPMScope == "" {
		return plugins.Install{}, errors.New("the gateway's feed has no npm scope, which OpenCode plugins install through")
	}
	if err := s.ensureNPMRC(opencodeConfigDir(s.home)); err != nil {
		return plugins.Install{}, err
	}
	pkgName := "@" + strings.TrimPrefix(s.feed.NPMScope, "@") + "/" + t.name
	cfg := filepath.Join(opencodeConfigDir(s.home), "opencode.json")
	if _, err := os.Stat(filepath.Join(opencodeConfigDir(s.home), "opencode.jsonc")); err == nil {
		cfg = filepath.Join(opencodeConfigDir(s.home), "opencode.jsonc")
	}
	if err := editSettings(cfg, func(d []byte) ([]byte, error) {
		cur, _, err := agentsync.ReadJSONValue(d, nil, "plugin")
		if err != nil {
			return nil, err
		}
		list, _ := cur.([]any)
		if slices.Contains(list, any(pkgName)) {
			return d, nil
		}
		return agentsync.SetJSONMember(d, nil, "plugin", append(slices.Clone(list), pkgName))
	}); err != nil {
		return plugins.Install{}, err
	}
	in := plugins.Install{Method: "native", Package: pkgName}
	if err := s.expandInto(a.id, t, &in); err != nil {
		return in, err
	}
	return in, nil
}

func removeOpencode(s *pluginSession, a *pluginAgent, name string, in plugins.Install) error {
	if in.Package != "" {
		cfg := filepath.Join(opencodeConfigDir(s.home), "opencode.json")
		if _, err := os.Stat(filepath.Join(opencodeConfigDir(s.home), "opencode.jsonc")); err == nil {
			cfg = filepath.Join(opencodeConfigDir(s.home), "opencode.jsonc")
		}
		if err := editSettings(cfg, func(d []byte) ([]byte, error) {
			cur, found, err := agentsync.ReadJSONValue(d, nil, "plugin")
			if err != nil || !found {
				return d, err
			}
			list, _ := cur.([]any)
			return agentsync.SetJSONMember(d, nil, "plugin", slices.DeleteFunc(slices.Clone(list), func(e any) bool { return e == in.Package }))
		}); err != nil {
			return err
		}
	}
	return removeExpanded(s, a, name, in)
}

// ---- Cline ----

func installCline(s *pluginSession, a *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	in := plugins.Install{Method: "expanded"}
	if s.hasBinary("cline") {
		// Cline installs from a directory, not an archive: unpack the
		// verified package and point it there.
		dir, err := os.MkdirTemp("", "zuano-cline-*")
		if err != nil {
			return in, err
		}
		defer os.RemoveAll(dir)
		for _, name := range t.vp.pkg.Paths() {
			target, err := safeJoin(dir, name)
			if err != nil {
				return in, err
			}
			if err := writeFileAtomic(target, t.vp.pkg.Files[name], 0o600); err != nil {
				return in, err
			}
		}
		if err := s.run(nil, "cline", "plugin", "install", dir); err != nil {
			return in, err
		}
		in.Method = "native"
	} else {
		fmt.Fprintln(s.err, "cline was not found on PATH; expanding the bundle into Cline's configuration instead")
	}
	if err := s.expandInto(a.id, t, &in); err != nil {
		return in, err
	}
	return in, nil
}

func removeCline(s *pluginSession, a *pluginAgent, name string, in plugins.Install) error {
	if in.Method == "native" {
		// Cline documents no uninstall command for a plugin it copied into its
		// store, so the person removes it there.
		fmt.Fprintf(s.err, "Cline has no command to remove %s: delete it from Cline's plugin store (see `cline config`). zuano forgets it.\n", textsafe.Clean(name))
	}
	return removeExpanded(s, a, name, in)
}

// ---- Agents with no plugin system ----

func installExpandedOnly(s *pluginSession, a *pluginAgent, t *pluginTarget) (plugins.Install, error) {
	in := plugins.Install{Method: "expanded"}
	if err := s.expandInto(a.id, t, &in); err != nil {
		return in, err
	}
	return in, nil
}
