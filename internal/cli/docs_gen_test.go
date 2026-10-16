package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDocsGen_WritesAPagePerVisibleCommand(t *testing.T) {
	root := &cobra.Command{Use: "zuano", Short: "root"}
	visible := &cobra.Command{Use: "audit", Short: "Read the audit log", Run: func(*cobra.Command, []string) {}}
	visible.Flags().String("from", "", "First day")
	visible.AddCommand(&cobra.Command{Use: "verify", Short: "Verify the chain", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(visible, DocsCmd(), &cobra.Command{Use: "secret", Short: "internal", Hidden: true, Run: func(*cobra.Command, []string) {}})

	dir := filepath.Join(t.TempDir(), "out")
	out, err := runCommand(t, root, "docs", "gen", "--out", dir)
	if err != nil || !strings.Contains(out, "Wrote ") {
		t.Fatalf("gen = %q, %v", out, err)
	}
	for _, name := range []string{"zuano.md", "zuano_audit.md", "zuano_audit_verify.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "zuano_secret.md")); err == nil {
		t.Error("a hidden command was documented")
	}
	page, _ := os.ReadFile(filepath.Join(dir, "zuano_audit.md"))
	for _, want := range []string{"# zuano audit", "--from string", "[zuano audit verify](zuano_audit_verify.md)"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
}

func TestDocsGen_RequiresAnOutputDirectory(t *testing.T) {
	root := &cobra.Command{Use: "zuano"}
	root.AddCommand(DocsCmd())
	if _, err := runCommand(t, root, "docs", "gen"); err == nil {
		t.Error("gen without --out succeeded")
	}
}
