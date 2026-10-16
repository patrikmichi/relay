package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/patrikmichi/zuano/internal/brand"
)

// FileRef is a file an install wrote and the hash it had when written.
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Install records one plugin installed for one agent. zuano removes and
// updates only what it lists.
type Install struct {
	Version     string    `json:"version"`
	Platform    string    `json:"platform"`
	Method      string    `json:"method"` // native or expanded
	Channel     string    `json:"channel"`
	Marketplace string    `json:"marketplace,omitempty"`
	InstalledAt string    `json:"installedAt"`
	Files       []FileRef `json:"files,omitempty"`
	// Skills and MCP name what an expansion put into the agent's own
	// configuration, so a removal takes the same things out.
	Skills []string `json:"skills,omitempty"`
	MCP    []string `json:"mcp,omitempty"`
	// Package is the npm package name an OpenCode install added.
	Package string `json:"package,omitempty"`
}

// Ledger maps plugin name to agent to install.
type Ledger struct {
	Version  int                           `json:"version"`
	Installs map[string]map[string]Install `json:"installs"`
}

// LedgerPath is where the install ledger lives for a home directory.
func LedgerPath(home string) string {
	return filepath.Join(home, ".config", brand.Name, "plugins", "ledger.json")
}

// LoadLedger reads the ledger; a missing file is an empty ledger.
func LoadLedger(path string) (*Ledger, error) {
	l := &Ledger{Version: 1, Installs: map[string]map[string]Install{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if l.Installs == nil {
		l.Installs = map[string]map[string]Install{}
	}
	return l, nil
}

// Save writes the ledger with mode 0600 through a temporary file in the same
// directory and a rename, so an interrupted write leaves the old ledger.
func (l *Ledger) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ledger-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Get returns the install of a plugin for an agent.
func (l *Ledger) Get(plugin, agent string) (Install, bool) {
	in, ok := l.Installs[plugin][agent]
	return in, ok
}

// Set records an install.
func (l *Ledger) Set(plugin, agent string, in Install) {
	if l.Installs[plugin] == nil {
		l.Installs[plugin] = map[string]Install{}
	}
	l.Installs[plugin][agent] = in
}

// Delete forgets an install.
func (l *Ledger) Delete(plugin, agent string) {
	delete(l.Installs[plugin], agent)
	if len(l.Installs[plugin]) == 0 {
		delete(l.Installs, plugin)
	}
}

// Plugins returns the installed plugin names, sorted.
func (l *Ledger) Plugins() []string {
	names := make([]string, 0, len(l.Installs))
	for n := range l.Installs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
