package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/relay/internal/agentport/txn"
	"github.com/patrikmichi/relay/internal/client"
)

// maxResponseBytes is the per-response read cap (10 MiB).
// Prevents a malicious/compromised gateway from OOM-ing the client.
const maxResponseBytes = 10 << 20

// ---- API response types ----

// manifestPlugin mirrors one entry in the CC marketplace.json plugins array.
type manifestPlugin struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// marketplaceManifest mirrors the CC marketplace.json top-level object.
type marketplaceManifest struct {
	Name    string           `json:"name"`
	Owner   string           `json:"owner,omitempty"`
	Plugins []manifestPlugin `json:"plugins"`
}

// pluginBundle is the materialisable shape returned by
// GET /api/marketplace/plugin/<id>/<rev>. File contents are UTF-8 strings.
// Paths are validated before being written to the local marketplace.
type pluginBundle struct {
	Files map[string]string `json:"files"`
}

// ---- syncDoer interface — allows tests to swap in an httptest.Server ----

// syncDoer is the minimal HTTP interface SyncCmd uses.
// The gateway client satisfies it; httptest servers satisfy it via adapters in tests.
type syncDoer interface {
	Get(path string) (*http.Response, error)
}

// ---- SyncCmd ----

// SyncCmd returns the `relay sync` cobra command.
func SyncCmd() *cobra.Command {
	return syncCmdWithDoer(nil) // nil → resolved at runtime from env/keychain
}

// syncCmdWithDoer is the internal constructor; doer==nil means resolve from env.
// The doer only serves the marketplace pull.
func syncCmdWithDoer(doer syncDoer) *cobra.Command {
	var (
		o           agentSyncOpts
		customDir   string
		gatewayURL  string
		marketplace bool
		matrix      bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync MCP servers, hooks, rules, commands, skills and subagents across coding agents",
		Args:  cobra.NoArgs,
		Long: `Write what relay manages into the config of every coding agent: Claude Code,
Codex, Cursor, OpenCode, Gemini CLI, Windsurf, Cline and GitHub Copilot.

Sources
  MCP servers   the gateway (when configured) plus servers added with
                'relay mcp install'
  hooks, rules, commands
                ~/.config/relay/sync/{hooks.json,rules/*.md,commands/*.md}
                for the user scope and <project>/.relay/ for the project scope
  skills, subagents
                everything installed for --from (default claude), copied to
                the other agents through the same engine as 'relay skill
                migrate' and 'relay agent migrate'

Entries relay writes are marked (a hash ledger for JSON, comment markers for
TOML, Markdown and owned files) so a later sync can update or remove them.
Entries you wrote are never replaced unless --force is given. Every write
is one transaction: a failure restores the files already changed.

Without --to, every agent detected on this machine is synced. Kinds an agent
has no format for are reported as unsupported, never skipped silently; see
'relay sync --matrix'.

--marketplace (or --dir) also pulls the gateway's Claude Code plugin
marketplace into ~/.config/relay/marketplace/<name>/.

Examples:
  relay sync --dry-run
  relay sync --to claude,codex --scope all
  relay sync --kind mcp,rules --to all`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if matrix {
				return printSyncMatrix(out)
			}
			wantMarketplace := marketplace || customDir != ""
			o.optionalAgents = wantMarketplace
			run, err := planAgentSync(o)
			if err != nil {
				return err
			}
			if len(run.agents) > 0 {
				if err := printSyncReport(out, run, o.dryRun, o.jsonOut); err != nil {
					return err
				}
				if !o.dryRun {
					if err := run.apply(cmd.Context()); err != nil {
						return err
					}
				}
			}
			if !wantMarketplace {
				return nil
			}
			d := doer
			if d == nil {
				d, err = resolveSyncDoer(gatewayURL)
				if err != nil {
					return err
				}
			}
			return runSync(commandDoer{d, cmd.Context()}, customDir, o.dryRun, out)
		},
	}

	f := cmd.Flags()
	f.StringSliceVar(&o.to, "to", nil, "Agents to sync: claude, codex, cursor, opencode, gemini-cli, windsurf, cline, copilot, or all (default: detected agents)")
	f.StringSliceVar(&o.scopes, "scope", nil, "user, project or all (default: user; project uses the current directory)")
	f.StringSliceVar(&o.kinds, "kind", nil, "Limit to mcp, hooks, rules, commands, skills, subagents (default: all)")
	f.StringVar(&o.from, "from", "", "Provider whose skills and subagents are copied to the other agents (default: claude)")
	f.StringVar(&o.mcpAuth, "mcp-auth", "auto", "Gateway MCP credentials: auto (API key when $GATEWAY_API_KEY is set, else OAuth), oauth or api-key")
	f.BoolVar(&o.dryRun, "dry-run", false, "Show what would change as a diff and write nothing")
	f.BoolVar(&o.force, "force", false, "Replace same-named entries relay does not manage")
	f.BoolVar(&o.acceptLoss, "accept-loss", false, "Copy skills and subagents even when a target drops non-security fields")
	f.BoolVar(&o.jsonOut, "json", false, "Print the report as JSON")
	f.BoolVar(&matrix, "matrix", false, "Print the agent by artifact support matrix and exit")
	f.BoolVar(&marketplace, "marketplace", false, "Also pull the gateway's Claude Code plugin marketplace")
	f.StringVar(&gatewayURL, "gateway-url", "", "Gateway URL for the marketplace pull (default: $GATEWAY_URL, then the config file)")
	f.StringVar(&customDir, "dir", "", "Marketplace directory (implies --marketplace; default: ~/.config/relay/marketplace/<name>/)")
	return cmd
}

// resolveSyncDoer reads the gateway URL and resolves an authenticated
// client for it. Auth resolution (GATEWAY_API_KEY / RELAY_EMAIL / persisted
// login email → keychain, with transparent OAuth refresh) is centralized in
// client.Resolve — *client.Client already satisfies syncDoer (it has a
// matching Get(path string) (*http.Response, error) method), so no adapter
// type is needed here anymore.
func resolveSyncDoer(overrideURL string) (syncDoer, error) {
	gURL, err := resolveGatewayURLOrFailClosed(overrideURL)
	if err != nil {
		return nil, err
	}

	c, err := client.Resolve(gURL)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ---- Core sync logic ----

// runSync implements the sync workflow.
func runSync(d syncDoer, customDir string, dryRun bool, writers ...io.Writer) error {
	out := outputWriter(writers)

	manifest, rawManifest, err := fetchManifest(d)
	if err != nil {
		return err
	}

	localDir, err := resolveLocalDir(customDir, manifest.Name)
	if err != nil {
		return fmt.Errorf("resolve local dir: %w", err)
	}

	if dryRun {
		fmt.Fprintf(out, "[dry-run] marketplace: %s\n", manifest.Name)
		fmt.Fprintf(out, "[dry-run] local dir:   %s\n", localDir)
		fmt.Fprintf(out, "[dry-run] plugins (%d):\n", len(manifest.Plugins))
		for _, p := range manifest.Plugins {
			// Validate before using, even in dry-run.
			id, err := safePluginID(p.Source)
			if err != nil {
				return fmt.Errorf("manifest plugin rejected: %w", err)
			}
			fmt.Fprintf(out, "  would write: plugins/%s/ (version %s)\n", id, p.Version)
		}
		// Propagate the error instead of silently discarding it.
		stale, err := stalePluginDirs(localDir, manifest)
		if err != nil {
			return fmt.Errorf("list stale plugins: %w", err)
		}
		for _, s := range stale {
			fmt.Fprintf(out, "[dry-run] would remove stale plugin: plugins/%s/\n", s)
		}
		fmt.Fprintf(out, "[dry-run] would write: marketplace.json\n")
		fmt.Fprintf(out, "[dry-run] would write: managed-settings.fragment.json\n")
		return nil
	}

	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return fmt.Errorf("create marketplace dir %s: %w", localDir, err)
	}

	for _, p := range manifest.Plugins {
		// Validate id before using it in filesystem paths.
		id, err := safePluginID(p.Source)
		if err != nil {
			return fmt.Errorf("manifest plugin rejected: %w", err)
		}
		rev := p.Version

		// Confirm resolved path is inside plugins/.
		pluginsBase := filepath.Join(localDir, "plugins")
		pluginDir := filepath.Join(pluginsBase, id)
		if !strings.HasPrefix(pluginDir+string(os.PathSeparator), pluginsBase+string(os.PathSeparator)) {
			return fmt.Errorf("plugin id %q would escape the plugins directory", id)
		}

		rawPlugin, err := fetchPlugin(d, id, rev)
		if err != nil {
			// Append retry guidance.
			return fmt.Errorf("fetch plugin %s: %w — re-run `relay sync` to retry", id, err)
		}

		if err := writePluginBundle(localDir, id, pluginDir, rawPlugin); err != nil {
			return fmt.Errorf("write plugin files for %s: %w", id, err)
		}
	}

	// Step 5: fetch and write managed-settings.
	rawSettings, err := fetchManagedSettings(d, localDir)
	if err != nil {
		return fmt.Errorf("fetch managed-settings: %w", err)
	}
	if err := writeFile(filepath.Join(localDir, "managed-settings.fragment.json"), rawSettings); err != nil {
		return fmt.Errorf("write managed-settings.fragment.json: %w", err)
	}

	// Step 6: write marketplace.json — only once every plugin and
	// managed-settings write it references has actually landed.
	if err := writeFile(filepath.Join(localDir, "marketplace.json"), rawManifest); err != nil {
		return fmt.Errorf("write marketplace.json: %w", err)
	}

	// Step 7: revocation — remove stale plugin dirs. Deliberately LAST,
	// after marketplace.json is confirmed written: revoking first would let
	// a crash/failure between revocation and the manifest write leave
	// marketplace.json still pointing at a plugin directory that no longer
	// exists. Revoking last means
	// the only failure-direction leftover is a harmless orphaned directory
	// marketplace.json already doesn't reference.
	stale, err := stalePluginDirs(localDir, manifest)
	if err != nil {
		return fmt.Errorf("list stale plugins: %w", err)
	}
	for _, s := range stale {
		dir := filepath.Join(localDir, "plugins", s)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove stale plugin %s: %w", s, err)
		}
		fmt.Fprintf(out, "Removed revoked plugin: plugins/%s/\n", s)
	}

	fmt.Fprintf(out, "Synced marketplace: %s\n", manifest.Name)
	fmt.Fprintf(out, "Local directory:    %s\n", localDir)
	fmt.Fprintf(out, "Plugins synced:     %d\n", len(manifest.Plugins))
	if len(stale) > 0 {
		fmt.Fprintf(out, "Stale plugins removed: %d\n", len(stale))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Managed-settings fragment written to:")
	fmt.Fprintf(out, "  %s\n", filepath.Join(localDir, "managed-settings.fragment.json"))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Register the marketplace in Claude Code:")
	fmt.Fprintf(out, "  /plugin marketplace add %s\n", localDir)
	fmt.Fprintln(out, "(Merge managed-settings.fragment.json into your Claude Code managed settings")
	fmt.Fprintln(out, " to auto-register and enable granted plugins.)")
	return nil
}

// ---- HTTP helpers ----

// fetchManifest calls GET /api/marketplace/manifest and returns the decoded manifest
// plus the raw bytes (for writing to disk verbatim).
func fetchManifest(d syncDoer) (marketplaceManifest, []byte, error) {
	resp, err := d.Get("/api/marketplace/manifest")
	if err != nil {
		return marketplaceManifest{}, nil, fmt.Errorf("GET /api/marketplace/manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return marketplaceManifest{}, nil, fmt.Errorf("not authenticated — run `relay login` first")
	}
	if resp.StatusCode != http.StatusOK {
		// Cap error-body read too.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return marketplaceManifest{}, nil, fmt.Errorf("GET /api/marketplace/manifest returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Cap response body to prevent OOM from a hostile gateway.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return marketplaceManifest{}, nil, fmt.Errorf("read manifest response: %w", err)
	}

	var m marketplaceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return marketplaceManifest{}, nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Name == "" {
		return marketplaceManifest{}, nil, fmt.Errorf("manifest has no name field")
	}
	// Reject names that could escape ~/.config/relay/marketplace/ when used as a
	// path component — even if the caller supplied --dir, the name is printed and
	// logged, so we reject unsafe names unconditionally.
	if strings.ContainsAny(m.Name, "/\\") || strings.Contains(m.Name, "..") {
		return marketplaceManifest{}, nil, fmt.Errorf("marketplace name %q contains path-unsafe characters", m.Name)
	}
	return m, raw, nil
}

// fetchPlugin calls GET /api/marketplace/plugin/<id>/<rev> and returns the raw bytes.
// id and rev are URL-path-escaped to prevent request hijacking.
func fetchPlugin(d syncDoer, id, rev string) ([]byte, error) {
	// Escape both id and rev so slashes / dots in rev cannot redirect the request.
	path := "/api/marketplace/plugin/" + url.PathEscape(id) + "/" + url.PathEscape(rev)
	resp, err := d.Get(path)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("not authenticated — run `relay login` first")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return nil, fmt.Errorf("GET %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Cap response body.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read plugin response: %w", err)
	}
	return raw, nil
}

// syncTxnTimeout bounds how long a plugin bundle write waits for another
// `relay sync` process's lock on the same plugin — local disk contention,
// never a network wait.
const syncTxnTimeout = 30 * time.Second

func writePluginBundle(localDir, pluginID, pluginDir string, raw []byte) error {
	var bundle pluginBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return fmt.Errorf("decode plugin bundle: %w", err)
	}

	newFiles := map[string][]byte{}
	if len(bundle.Files) == 0 {
		newFiles[filepath.ToSlash(filepath.Join(".claude-plugin", "plugin.json"))] = raw
	} else {
		for rel, contents := range bundle.Files {
			clean := filepath.Clean(rel)
			if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("plugin bundle contains unsafe path %q", rel)
			}
			target := filepath.Join(pluginDir, clean)
			if !strings.HasPrefix(target+string(os.PathSeparator), pluginDir+string(os.PathSeparator)) {
				return fmt.Errorf("plugin bundle path %q escapes plugin directory", rel)
			}
			newFiles[filepath.ToSlash(clean)] = []byte(contents)
		}
	}

	ownedBefore, err := loadOwnedFiles(localDir, pluginID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), syncTxnTimeout)
	defer cancel()
	tx, err := txn.Begin(ctx, fmt.Sprintf("sync plugin %s", pluginID), []string{"sync-plugin:" + localDir + ":" + pluginID})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		return tx.Discard(fmt.Errorf("create plugin dir %s: %w", pluginDir, err))
	}
	for rel, contents := range newFiles {
		if err := tx.Stage(pluginDir, filepath.FromSlash(rel), contents, 0o644); err != nil {
			return tx.Discard(err)
		}
	}
	for _, rel := range ownedBefore {
		if _, keep := newFiles[rel]; keep {
			continue
		}
		if err := tx.StageRemoval(pluginDir, filepath.FromSlash(rel)); err != nil {
			return tx.Discard(err)
		}
	}

	if err := tx.Persist(); err != nil {
		return tx.Discard(err)
	}
	if err := tx.Apply(txn.VerifyNoSymlinks); err != nil {
		return tx.Discard(err)
	}

	newOwned := make([]string, 0, len(newFiles))
	for rel := range newFiles {
		newOwned = append(newOwned, rel)
	}
	return tx.Commit(func() error { return saveOwnedFiles(localDir, pluginID, newOwned) })
}

// fetchManagedSettings calls GET /api/marketplace/managed-settings?localDir=<abs> and returns raw bytes.
func fetchManagedSettings(d syncDoer, localDir string) ([]byte, error) {
	q := url.QueryEscape(localDir)
	path := "/api/marketplace/managed-settings?localDir=" + q
	resp, err := d.Get(path)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("not authenticated — run `relay login` first")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return nil, fmt.Errorf("GET managed-settings returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Cap response body.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read managed-settings response: %w", err)
	}
	return raw, nil
}

// ---- Validation helpers ----

// safePluginID extracts the plugin ID from a source path and validates it.
// Rejects IDs that are empty, contain path separators, or contain "..".
// Returns the safe ID string on success.
func safePluginID(source string) (string, error) {
	id := pluginIDFromSource(source)
	if id == "" {
		return "", fmt.Errorf("plugin source %q yields an empty id", source)
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return "", fmt.Errorf("plugin source %q has an unsafe id %q", source, id)
	}
	return id, nil
}

// ---- File system helpers ----

// resolveLocalDir returns the absolute path for the marketplace dir.
// If customDir is non-empty it is used; otherwise defaults to
// ~/.config/relay/marketplace/<marketplaceName>/.
// Rejects marketplace names containing path-unsafe characters.
func resolveLocalDir(customDir, marketplaceName string) (string, error) {
	if customDir != "" {
		abs, err := filepath.Abs(customDir)
		if err != nil {
			return "", fmt.Errorf("resolve dir %s: %w", customDir, err)
		}
		return abs, nil
	}
	// Validate the name before embedding it in a path.
	if strings.ContainsAny(marketplaceName, "/\\") || strings.Contains(marketplaceName, "..") {
		return "", fmt.Errorf("marketplace name %q contains path-unsafe characters", marketplaceName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "relay", "marketplace", marketplaceName), nil
}

// pluginIDFromSource extracts the plugin ID from a source path like "./plugins/<id>".
func pluginIDFromSource(source string) string {
	source = strings.TrimPrefix(source, "./plugins/")
	source = strings.TrimPrefix(source, "plugins/")
	return source
}

// stalePluginDirs returns the names of subdirectories under <localDir>/plugins/
// that are NOT referenced by any plugin in the manifest.
// Returns an empty slice (not an error) if the plugins dir does not yet exist.
// Only plugins with safe (validated) IDs are added to the known set; a manifest
// entry with an unsafe source is simply not considered "known", so it would be
// treated as stale — but runSync rejects unsafe IDs before reaching this function.
func stalePluginDirs(localDir string, manifest marketplaceManifest) ([]string, error) {
	pluginsDir := filepath.Join(localDir, "plugins")
	entries, err := os.ReadDir(pluginsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read plugins dir: %w", err)
	}

	// Build a set of IDs present in the fresh manifest.
	// Only safe IDs are added; unsafe ones are intentionally excluded.
	known := make(map[string]struct{}, len(manifest.Plugins))
	for _, p := range manifest.Plugins {
		if id, err := safePluginID(p.Source); err == nil {
			known[id] = struct{}{}
		}
	}

	var stale []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := known[e.Name()]; !ok {
			stale = append(stale, e.Name())
		}
	}
	return stale, nil
}

// writeFile writes data to path atomically (write + rename).
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}
