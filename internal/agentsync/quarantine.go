package agentsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Statuses of a quarantine item beyond the sync ones.
const (
	StatusQuarantined = "quarantined"
	StatusRestored    = "restored"
	StatusNotFound    = "not_found"
	StatusManaged     = "managed"
	StatusChanged     = "changed"
)

// Format of a config file that holds MCP servers.
const (
	FormatJSON = "json"
	FormatTOML = "toml"
)

// QuarantineTarget names an MCP server somebody else configured that should be switched off.
type QuarantineTarget struct {
	Agent AgentID `json:"agent"`
	Scope Scope   `json:"scope"`
	Name  string  `json:"name"`
	// Hash, when set, must match the entry on this machine: the entry was
	// judged as it was, and a changed one has to be looked at again.
	Hash string `json:"hash,omitempty"`
}

// Quarantined is one server taken out of an agent's config, with what is
// needed to put it back.
type Quarantined struct {
	Target QuarantineTarget `json:"target"`
	Path   string           `json:"path"`
	Format string           `json:"format"`
	Key    string           `json:"key"`
	// Entry is the server as it was: JSON for a JSON config, the TOML tables otherwise.
	Entry  string `json:"entry"`
	Hash   string `json:"hash"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// mcpLocation says where an agent keeps its MCP servers: the file, the format
// and the key (JSON) or table (TOML) that holds them.
func mcpLocation(home Env, agent AgentID, scope Scope) (path, format, key string, ok bool) {
	root := home.root(scope)
	switch agent {
	case Claude:
		if scope == ScopeProject {
			return filepath.Join(home.Project, ".mcp.json"), FormatJSON, "mcpServers", true
		}
		return filepath.Join(home.Home, ".claude.json"), FormatJSON, "mcpServers", true
	case Cursor:
		return filepath.Join(root, ".cursor", "mcp.json"), FormatJSON, "mcpServers", true
	case Gemini:
		return filepath.Join(root, ".gemini", "settings.json"), FormatJSON, "mcpServers", true
	case OpenCode:
		return opencodeConfigPath(home, scope), FormatJSON, "mcp", true
	case Windsurf:
		if scope == ScopeProject {
			return "", "", "", false
		}
		for _, p := range windsurfMCPPaths(home) {
			if fileExists(p) {
				return p, FormatJSON, "mcpServers", true
			}
		}
	case Cline:
		if scope == ScopeProject {
			return "", "", "", false
		}
		for _, p := range clineSettingsPaths(home) {
			if fileExists(p) {
				return p, FormatJSON, "mcpServers", true
			}
		}
	case Copilot:
		if scope == ScopeProject {
			return filepath.Join(home.Project, ".vscode", "mcp.json"), FormatJSON, "servers", true
		}
	case Codex:
		return filepath.Join(root, ".codex", "config.toml"), FormatTOML, "mcp_servers", true
	}
	return "", "", "", false
}

// EntryHash identifies a server entry in a quarantine report.
func EntryHash(entry string) string {
	sum := sha256.Sum256([]byte(entry))
	return hex.EncodeToString(sum[:8])
}

// PlanQuarantine plans taking each target out of its agent's config. Only a
// server the person wrote is touched: one zuano manages, one that is not
// there and one that no longer matches the hash it was judged by are reported
// and left alone. Nothing is written until Apply; the Quarantined entries hold
// what is needed to restore each server.
func PlanQuarantine(where Env, targets []QuarantineTarget) (*Plan, []Quarantined, error) {
	led, err := LoadLedger(where.Home)
	if err != nil {
		return nil, nil, err
	}
	c := &Ctx{Env: where, Ledger: led}
	var out []Quarantined
	for _, t := range targets {
		q := Quarantined{Target: t}
		path, format, key, ok := mcpLocation(where, t.Agent, t.Scope)
		if !ok {
			q.Status, q.Detail = StatusNotFound, "this agent keeps no MCP servers in that scope"
			out = append(out, q)
			continue
		}
		q.Path, q.Format, q.Key = path, format, key
		data, exists, real, rerr := c.read(t.Scope, path)
		var unsafe *unsafePathError
		switch {
		case errors.As(rerr, &unsafe):
			q.Status, q.Detail = StatusSkipped, unsafe.reason
			out = append(out, q)
			continue
		case rerr != nil:
			return nil, nil, rerr
		case !exists:
			q.Status, q.Detail = StatusNotFound, "the config file is gone"
			out = append(out, q)
			continue
		}
		q.Path = real
		if _, mine := led.Files[real]["mcp:"+t.Name]; mine {
			q.Status, q.Detail = StatusManaged, "zuano manages this server; it is not shadow"
			out = append(out, q)
			continue
		}
		var entry string
		var after []byte
		if format == FormatJSON {
			entry, after, err = removeJSONServer(data, key, t.Name)
		} else {
			entry, after, err = removeTOMLServer(data, key, t.Name)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", real, err)
		}
		if entry == "" {
			q.Status, q.Detail = StatusNotFound, "no server of that name in the config"
			out = append(out, q)
			continue
		}
		q.Entry, q.Hash = entry, EntryHash(entry)
		if t.Hash != "" && t.Hash != q.Hash {
			q.Status, q.Detail = StatusChanged, "the server changed since it was reported; it has to be reported and judged again"
			out = append(out, q)
			continue
		}
		if err := c.write(real, fileMode(real), after); err != nil {
			return nil, nil, err
		}
		q.Status = StatusQuarantined
		out = append(out, q)
	}
	return c.finish(nil), out, nil
}

// PlanRestore plans putting quarantined servers back where they were.
func PlanRestore(where Env, saved []Quarantined) (*Plan, []Quarantined, error) {
	led, err := LoadLedger(where.Home)
	if err != nil {
		return nil, nil, err
	}
	c := &Ctx{Env: where, Ledger: led}
	var out []Quarantined
	for _, q := range saved {
		data, _, real, rerr := c.read(q.Target.Scope, q.Path)
		var unsafe *unsafePathError
		if errors.As(rerr, &unsafe) {
			q.Status, q.Detail = StatusSkipped, unsafe.reason
			out = append(out, q)
			continue
		}
		if rerr != nil {
			return nil, nil, rerr
		}
		var after []byte
		if q.Format == FormatJSON {
			var v any
			if err := json.Unmarshal([]byte(q.Entry), &v); err != nil {
				return nil, nil, fmt.Errorf("the saved entry of %s is not JSON: %w", q.Target.Name, err)
			}
			if _, found, _ := ReadJSONValue(data, []string{q.Key}, q.Target.Name); found {
				q.Status, q.Detail = StatusSkipped, "a server of that name is already in the config"
				out = append(out, q)
				continue
			}
			if after, err = SetJSONMember(data, []string{q.Key}, q.Target.Name, v); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", real, err)
			}
		} else {
			if entry, _, _ := removeTOMLServer(data, q.Key, q.Target.Name); entry != "" {
				q.Status, q.Detail = StatusSkipped, "a server of that name is already in the config"
				out = append(out, q)
				continue
			}
			text := string(data)
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			after = []byte(text + "\n" + q.Entry)
		}
		if err := c.write(real, fileMode(real), after); err != nil {
			return nil, nil, err
		}
		q.Status = StatusRestored
		out = append(out, q)
	}
	return c.finish(nil), out, nil
}

func removeJSONServer(data []byte, key, name string) (entry string, after []byte, err error) {
	v, found, err := ReadJSONValue(data, []string{key}, name)
	if err != nil || !found {
		return "", data, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", data, err
	}
	after, err = DeleteJSONMember(data, []string{key}, name)
	if err != nil {
		return "", data, err
	}
	return string(raw), after, nil
}

// removeTOMLServer cuts the [table.name] section and its sub-tables out of a
// TOML document and returns them.
func removeTOMLServer(data []byte, table, name string) (entry string, after []byte, err error) {
	quoted := regexp.QuoteMeta(name)
	own := regexp.MustCompile(`^\s*\[\s*` + regexp.QuoteMeta(table) + `\s*\.\s*(?:"` + quoted + `"|'` + quoted + `'|` + quoted + `)\s*(?:\.[^\]]*)?\]\s*(?:#.*)?$`)
	anyTable := regexp.MustCompile(`^\s*\[`)
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var kept, cut strings.Builder
	inside := false
	for _, l := range strings.SplitAfter(text, "\n") {
		switch {
		case own.MatchString(strings.TrimRight(l, "\n")):
			inside = true
		case anyTable.MatchString(l):
			inside = false
		}
		if inside {
			cut.WriteString(l)
		} else {
			kept.WriteString(l)
		}
	}
	if cut.Len() == 0 {
		return "", data, nil
	}
	return strings.TrimRight(cut.String(), "\n") + "\n", []byte(kept.String()), nil
}

func fileMode(path string) fs.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o600
}
