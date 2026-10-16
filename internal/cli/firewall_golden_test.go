package cli

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/patrikmichi/zuano/internal/config"
)

var updateFirewallGolden = flag.Bool("update-firewall-golden", false, "rewrite the firewall sync golden files")

// agentTree reads every file below home except zuano's own state.
func agentTree(t *testing.T, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(home, p)
		if d.IsDir() {
			if rel == filepath.Join(".config", "zuano") || rel == filepath.Join(".local", "state") || rel == filepath.Join(".local", "share") {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// firewallGolden is the text of every config file the firewall sync changed.
func firewallGolden(before, after map[string]string, home, installID string) string {
	var names []string
	for name, text := range after {
		if before[name] != text {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		text := strings.ReplaceAll(after[name], home, "$HOME")
		text = strings.ReplaceAll(text, strings.ReplaceAll(home, `\`, `\\`), "$HOME")
		text = strings.ReplaceAll(text, installID, "$INSTALL_ID")
		b.WriteString("=== " + name + "\n" + text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestSyncFirewallGoldenPerAgent pins what each agent's config looks like once
// its servers are routed through the firewall, and that turning the firewall
// off gives back the original bytes.
func TestSyncFirewallGoldenPerAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("agent config locations differ per platform; the golden files are the Linux ones")
	}
	for _, agent := range []string{"claude", "codex", "cursor", "opencode", "gemini", "windsurf", "cline"} {
		t.Run(agent, func(t *testing.T) {
			home := syncHome(t)
			keyring.MockInit()
			if err := config.SetGatewayURL("https://gw.example.com"); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{".cursor", ".config/opencode", ".codeium/windsurf", ".config/Code/User/globalStorage/saoudrizwan.claude-dev/settings"} {
				if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			install := func(args ...string) {
				t.Helper()
				if out, err := runCommand(t, McpInstallCmd(), append(args, "--to", agent, "--yes")...); err != nil {
					t.Fatalf("install: %v\n%s", err, out)
				}
			}
			// Codex's TOML is read back in name order, so the servers go in that order.
			install("docs", "--url", "https://docs.example.com/mcp")
			install("files", "--command", "npx", "--arg", "-y", "--arg", "@acme/files", "--env", "ROOT=/srv")
			original := agentTree(t, home)

			if out, err := runCommand(t, SyncCmd(), "--to", agent, "--kind", "mcp", "--firewall", "--yes", "--no-packs"); err != nil {
				t.Fatalf("sync --firewall: %v\n%s", err, out)
			}
			installID, _ := config.InstallID()
			got := firewallGolden(original, agentTree(t, home), home, installID)
			if !strings.Contains(got, "mcp") || !strings.Contains(got, "/api/fw/docs/mcp") {
				t.Fatalf("the servers were not routed:\n%s", got)
			}
			golden := filepath.Join("testdata", "firewall", agent+".golden")
			if *updateFirewallGolden {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("config differs from %s; rerun with -update-firewall-golden if intended\n%s", golden, got)
			}

			if out, err := runCommand(t, SyncCmd(), "--to", agent, "--kind", "mcp", "--firewall=off", "--yes", "--no-packs"); err != nil {
				t.Fatalf("sync --firewall=off: %v\n%s", err, out)
			}
			restored := agentTree(t, home)
			for name, text := range original {
				if restored[name] != text {
					t.Errorf("%s is not byte-identical after --firewall=off\nwant:\n%s\ngot:\n%s", name, text, restored[name])
				}
			}
			if len(restored) != len(original) {
				t.Errorf("files after the round trip: %d, before: %d", len(restored), len(original))
			}
		})
	}
}
