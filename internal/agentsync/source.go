package agentsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SourceDir is where relay reads what to sync: ~/.config/relay/sync for the
// user scope and <project>/.relay for the project scope.
func SourceDir(env Env, scope Scope) string {
	if scope == ScopeProject {
		return filepath.Join(env.Project, ".relay")
	}
	return filepath.Join(env.Home, ".config", "relay", "sync")
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// MCPSource is the persisted list of MCP servers installed with
// `relay mcp install`, plus the switch for the gateway server.
type MCPSource struct {
	GatewayDisabled bool        `json:"gatewayDisabled,omitempty"`
	Servers         []MCPServer `json:"servers,omitempty"`
}

func MCPSourcePath(env Env, scope Scope) string {
	return filepath.Join(SourceDir(env, scope), "mcp.json")
}

func LoadMCPSource(env Env, scope Scope) (MCPSource, error) {
	var src MCPSource
	raw, err := os.ReadFile(MCPSourcePath(env, scope))
	if errors.Is(err, fs.ErrNotExist) {
		return src, nil
	}
	if err != nil {
		return src, err
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return src, fmt.Errorf("parse %s: %w", MCPSourcePath(env, scope), err)
	}
	for _, s := range src.Servers {
		if err := s.Validate(); err != nil {
			return src, fmt.Errorf("%s: %w", MCPSourcePath(env, scope), err)
		}
	}
	return src, nil
}

func SaveMCPSource(env Env, scope Scope, src MCPSource) error {
	path := MCPSourcePath(env, scope)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sort.Slice(src.Servers, func(i, j int) bool { return src.Servers[i].Name < src.Servers[j].Name })
	raw, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func (s MCPServer) Validate() error {
	if !safeName.MatchString(s.Name) {
		return fmt.Errorf("invalid MCP server name %q", s.Name)
	}
	if (s.URL == "") == (s.Command == "") {
		return fmt.Errorf("MCP server %q needs exactly one of url or command", s.Name)
	}
	if s.URL != "" {
		if err := checkRemoteURL(s.URL); err != nil {
			return fmt.Errorf("MCP server %q: %w", s.Name, err)
		}
	}
	return nil
}

// checkRemoteURL accepts https, or http for a loopback host, so a synced
// server never sends its credentials in clear text over a network.
func checkRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid url %q", raw)
	}
	if u.User != nil {
		return fmt.Errorf("url %q must not contain credentials", raw)
	}
	host := u.Hostname()
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback()):
		return nil
	}
	return fmt.Errorf("url %q must use https (http is allowed only for localhost)", raw)
}

// LoadBundle reads rules, commands and hooks from dir. Problems with one
// file are returned as warnings so the rest still syncs.
func LoadBundle(dir string) (Bundle, []string, error) {
	var b Bundle
	var warns []string

	rules, w, err := loadMarkdown(filepath.Join(dir, "rules"))
	if err != nil {
		return b, nil, err
	}
	warns = append(warns, w...)
	for _, f := range rules {
		r := Rule{Name: f.name, Description: f.fm.Description, Globs: f.fm.globs(), Body: f.body}
		r.Always = len(r.Globs) == 0 && (f.fm.Always == nil || *f.fm.Always)
		b.Rules = append(b.Rules, r)
	}

	cmds, w, err := loadMarkdown(filepath.Join(dir, "commands"))
	if err != nil {
		return b, nil, err
	}
	warns = append(warns, w...)
	for _, f := range cmds {
		b.Commands = append(b.Commands, Command{Name: f.name, Description: f.fm.Description, ArgumentHint: f.fm.ArgumentHint, Body: f.body})
	}

	hooks, w, err := loadHooks(filepath.Join(dir, "hooks.json"))
	if err != nil {
		return b, nil, err
	}
	warns = append(warns, w...)
	b.Hooks = hooks
	return b, warns, nil
}

type frontmatter struct {
	Description  string `yaml:"description"`
	ArgumentHint string `yaml:"argument-hint"`
	Globs        any    `yaml:"globs"`
	Always       *bool  `yaml:"always"`
}

func (f frontmatter) globs() []string {
	switch t := f.Globs.(type) {
	case string:
		var out []string
		for _, g := range strings.Split(t, ",") {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
			}
		}
		return out
	case []any:
		var out []string
		for _, g := range t {
			if s, ok := g.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

type mdFile struct {
	name string
	fm   frontmatter
	body string
}

func loadMarkdown(dir string) ([]mdFile, []string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var out []mdFile
	var warns []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if !safeName.MatchString(name) {
			warns = append(warns, fmt.Sprintf("%s: skipped, name must match %s", filepath.Join(dir, e.Name()), safeName))
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		fm, body, err := splitFrontmatter(string(raw))
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: skipped, %v", filepath.Join(dir, e.Name()), err))
			continue
		}
		out = append(out, mdFile{name: name, fm: fm, body: strings.TrimSpace(body)})
	}
	return out, warns, nil
}

func splitFrontmatter(s string) (frontmatter, string, error) {
	var fm frontmatter
	s = strings.TrimPrefix(s, "\xef\xbb\xbf")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return fm, s, nil
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, "", errors.New("unterminated frontmatter")
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return fm, "", fmt.Errorf("invalid frontmatter: %w", err)
	}
	body := rest[end+len("\n---"):]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	return fm, body, nil
}

func loadHooks(path string) ([]Hook, []string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		Hooks []Hook `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []Hook
	var warns []string
	for i, h := range doc.Hooks {
		switch {
		case !hookEvents[h.Event]:
			warns = append(warns, fmt.Sprintf("%s: hook %d skipped, unknown event %q", path, i+1, h.Event))
		case strings.TrimSpace(h.Command) == "":
			warns = append(warns, fmt.Sprintf("%s: hook %d skipped, empty command", path, i+1))
		default:
			out = append(out, h)
		}
	}
	return out, warns, nil
}
