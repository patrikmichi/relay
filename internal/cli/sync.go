package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

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

// managedSettings is the shape returned by GET /api/marketplace/managed-settings.
// Written verbatim to managed-settings.fragment.json.
type managedSettings = json.RawMessage

// ---- syncDoer interface — allows tests to swap in an httptest.Server ----

// syncDoer is the minimal HTTP interface SyncCmd uses.
// The gateway client satisfies it; httptest servers satisfy it via adapters in tests.
type syncDoer interface {
	Get(path string) (*http.Response, error)
}

// ---- SyncCmd ----

// SyncCmd returns the `relay sync` cobra command.
// It pulls the caller's marketplace manifest from the gateway and materialises
// a local-path Claude Code marketplace directory.
func SyncCmd() *cobra.Command {
	return syncCmdWithDoer(nil) // nil → resolved at runtime from env/keychain
}

// syncCmdWithDoer is the internal constructor; doer==nil means resolve from env.
// Exposed for testing via a package-level wrapper below.
func syncCmdWithDoer(doer syncDoer) *cobra.Command {
	var (
		customDir  string
		dryRun     bool
		gatewayURL string
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync the marketplace from the gateway to a local Claude Code directory",
		Long: `Pull the caller's marketplace from the gateway and write a local-path
Claude Code marketplace under ~/.config/relay/marketplace/<name>/.

The directory is namespaced by the marketplace name so multiple orgs never collide.

Flags:
  --dir <path>   Override the default output directory.
  --dry-run      Fetch and report what WOULD be written/removed — write nothing.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Resolve doer (real gateway client) when not injected by tests.
			d := doer
			if d == nil {
				var err error
				d, err = resolveSyncDoer(gatewayURL)
				if err != nil {
					return err
				}
			}
			return runSync(d, customDir, dryRun)
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (default: $GATEWAY_URL, config, or built-in default)")
	cmd.Flags().StringVar(&customDir, "dir", "", "Local marketplace directory (default: ~/.config/relay/marketplace/<name>/)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Fetch and report changes without writing anything")
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
func runSync(d syncDoer, customDir string, dryRun bool) error {
	// Step 1: fetch manifest.
	manifest, rawManifest, err := fetchManifest(d)
	if err != nil {
		return err
	}

	// Step 2: determine local dir.
	localDir, err := resolveLocalDir(customDir, manifest.Name)
	if err != nil {
		return fmt.Errorf("resolve local dir: %w", err)
	}

	if dryRun {
		fmt.Printf("[dry-run] marketplace: %s\n", manifest.Name)
		fmt.Printf("[dry-run] local dir:   %s\n", localDir)
		fmt.Printf("[dry-run] plugins (%d):\n", len(manifest.Plugins))
		for _, p := range manifest.Plugins {
			// C1 (dry-run): validate before using.
			id, err := safePluginID(p.Source)
			if err != nil {
				return fmt.Errorf("manifest plugin rejected: %w", err)
			}
			fmt.Printf("  would write: plugins/%s/ (version %s)\n", id, p.Version)
		}
		// N7: propagate the error instead of silently discarding it.
		stale, err := stalePluginDirs(localDir, manifest)
		if err != nil {
			return fmt.Errorf("list stale plugins: %w", err)
		}
		for _, s := range stale {
			fmt.Printf("[dry-run] would remove stale plugin: plugins/%s/\n", s)
		}
		fmt.Printf("[dry-run] would write: marketplace.json\n")
		fmt.Printf("[dry-run] would write: managed-settings.fragment.json\n")
		return nil
	}

	// Step 3: create dir.
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return fmt.Errorf("create marketplace dir %s: %w", localDir, err)
	}

	// Step 4: write marketplace.json.
	if err := writeFile(filepath.Join(localDir, "marketplace.json"), rawManifest); err != nil {
		return fmt.Errorf("write marketplace.json: %w", err)
	}

	// Step 5: fetch and write each plugin.
	for _, p := range manifest.Plugins {
		// C1: validate id before using it in filesystem paths.
		id, err := safePluginID(p.Source)
		if err != nil {
			return fmt.Errorf("manifest plugin rejected: %w", err)
		}
		rev := p.Version

		// C1: confirm resolved path is inside plugins/.
		pluginsBase := filepath.Join(localDir, "plugins")
		pluginDir := filepath.Join(pluginsBase, id)
		if !strings.HasPrefix(pluginDir+string(os.PathSeparator), pluginsBase+string(os.PathSeparator)) {
			return fmt.Errorf("plugin id %q would escape the plugins directory", id)
		}

		rawPlugin, err := fetchPlugin(d, id, rev)
		if err != nil {
			// L6: append retry guidance.
			return fmt.Errorf("fetch plugin %s: %w — re-run `relay sync` to retry", id, err)
		}

		if err := os.MkdirAll(pluginDir, 0o700); err != nil {
			return fmt.Errorf("create plugin dir %s: %w", pluginDir, err)
		}
		if err := writePluginBundle(pluginDir, rawPlugin); err != nil {
			return fmt.Errorf("write plugin files for %s: %w", id, err)
		}
	}

	// Step 6: revocation — remove stale plugin dirs.
	stale, err := stalePluginDirs(localDir, manifest)
	if err != nil {
		return fmt.Errorf("list stale plugins: %w", err)
	}
	for _, s := range stale {
		dir := filepath.Join(localDir, "plugins", s)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove stale plugin %s: %w", s, err)
		}
		fmt.Printf("Removed revoked plugin: plugins/%s/\n", s)
	}

	// Step 7: fetch and write managed-settings.
	rawSettings, err := fetchManagedSettings(d, localDir)
	if err != nil {
		return fmt.Errorf("fetch managed-settings: %w", err)
	}
	if err := writeFile(filepath.Join(localDir, "managed-settings.fragment.json"), rawSettings); err != nil {
		return fmt.Errorf("write managed-settings.fragment.json: %w", err)
	}

	// Step 8: print summary.
	fmt.Printf("Synced marketplace: %s\n", manifest.Name)
	fmt.Printf("Local directory:    %s\n", localDir)
	fmt.Printf("Plugins synced:     %d\n", len(manifest.Plugins))
	if len(stale) > 0 {
		fmt.Printf("Stale plugins removed: %d\n", len(stale))
	}
	fmt.Println()
	fmt.Println("Managed-settings fragment written to:")
	fmt.Printf("  %s\n", filepath.Join(localDir, "managed-settings.fragment.json"))
	fmt.Println()
	fmt.Println("Next step — register the marketplace in Claude Code:")
	fmt.Printf("  /plugin marketplace add %s\n", localDir)
	fmt.Println("(Merge managed-settings.fragment.json into your Claude Code managed settings")
	fmt.Println(" to auto-register and enable granted plugins.)")
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
		// M4: cap error-body read too.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return marketplaceManifest{}, nil, fmt.Errorf("GET /api/marketplace/manifest returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// M4: cap response body to prevent OOM from a hostile gateway.
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
	// C2: reject names that could escape ~/.config/relay/marketplace/ when used as a
	// path component — even if the caller supplied --dir, the name is printed and
	// logged, so we reject unsafe names unconditionally.
	if strings.ContainsAny(m.Name, "/\\") || strings.Contains(m.Name, "..") {
		return marketplaceManifest{}, nil, fmt.Errorf("marketplace name %q contains path-unsafe characters", m.Name)
	}
	return m, raw, nil
}

// fetchPlugin calls GET /api/marketplace/plugin/<id>/<rev> and returns the raw bytes.
// id and rev are URL-path-escaped to prevent request hijacking (H3).
func fetchPlugin(d syncDoer, id, rev string) ([]byte, error) {
	// H3: escape both id and rev so slashes / dots in rev cannot redirect the request.
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

	// M4: cap response body.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read plugin response: %w", err)
	}
	return raw, nil
}

// writePluginBundle validates and writes every artifact returned by the
// gateway. Before the bundle contract existed, the endpoint returned a raw
// plugin manifest; accept that legacy shape and place it at Claude Code's
// required .claude-plugin/plugin.json path so older gateways remain usable.
func writePluginBundle(pluginDir string, raw []byte) error {
	var bundle pluginBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return fmt.Errorf("decode plugin bundle: %w", err)
	}
	if len(bundle.Files) == 0 {
		manifestDir := filepath.Join(pluginDir, ".claude-plugin")
		if err := os.MkdirAll(manifestDir, 0o700); err != nil {
			return fmt.Errorf("create legacy manifest directory: %w", err)
		}
		return writeFile(filepath.Join(manifestDir, "plugin.json"), raw)
	}

	for rel, contents := range bundle.Files {
		clean := filepath.Clean(rel)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("plugin bundle contains unsafe path %q", rel)
		}
		target := filepath.Join(pluginDir, clean)
		if !strings.HasPrefix(target+string(os.PathSeparator), pluginDir+string(os.PathSeparator)) {
			return fmt.Errorf("plugin bundle path %q escapes plugin directory", rel)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create directory for %q: %w", rel, err)
		}
		if err := writeFile(target, []byte(contents)); err != nil {
			return fmt.Errorf("write %q: %w", rel, err)
		}
	}
	return nil
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

	// M4: cap response body.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read managed-settings response: %w", err)
	}
	return raw, nil
}

// ---- Validation helpers ----

// safePluginID extracts the plugin ID from a source path and validates it.
// C1: rejects IDs that are empty, contain path separators, or contain "..".
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
// C2: rejects marketplace names containing path-unsafe characters.
func resolveLocalDir(customDir, marketplaceName string) (string, error) {
	if customDir != "" {
		abs, err := filepath.Abs(customDir)
		if err != nil {
			return "", fmt.Errorf("resolve dir %s: %w", customDir, err)
		}
		return abs, nil
	}
	// C2: validate the name before embedding it in a path.
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
