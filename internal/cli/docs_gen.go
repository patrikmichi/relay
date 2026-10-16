package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// DocsCmd returns the hidden `zuano docs` group. The gateway's docs site
// calls `docs gen` to build its command reference from the live command tree.
func DocsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "docs", Short: "Generate documentation", Hidden: true}
	var out string
	gen := &cobra.Command{
		Use:   "gen",
		Short: "Write a Markdown page per command",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			n, err := writeCommandDocs(c.Root(), out)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "Wrote %d pages to %s\n", n, out)
			return nil
		},
	}
	gen.Flags().StringVar(&out, "out", "", "Directory to write the pages to")
	_ = gen.MarkFlagRequired("out")
	cmd.AddCommand(gen)
	return cmd
}

// writeCommandDocs writes one page for cmd and for each visible command
// beneath it, named by the command path with underscores.
func writeCommandDocs(cmd *cobra.Command, dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	page := filepath.Join(dir, strings.ReplaceAll(cmd.CommandPath(), " ", "_")+".md")
	if err := os.WriteFile(page, []byte(commandPage(cmd)), 0o644); err != nil {
		return 0, err
	}
	n := 1
	for _, sub := range cmd.Commands() {
		if sub.Hidden || sub.Name() == "help" {
			continue
		}
		m, err := writeCommandDocs(sub, dir)
		if err != nil {
			return 0, err
		}
		n += m
	}
	return n, nil
}

func commandPage(cmd *cobra.Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", cmd.CommandPath(), cmd.Short)
	if cmd.Long != "" {
		b.WriteString(cmd.Long + "\n\n")
	}
	if cmd.Runnable() {
		fmt.Fprintf(&b, "```\n%s\n```\n\n", cmd.UseLine())
	}
	if cmd.Example != "" {
		fmt.Fprintf(&b, "## Examples\n\n```\n%s\n```\n\n", cmd.Example)
	}
	if f := cmd.NonInheritedFlags().FlagUsages(); f != "" {
		fmt.Fprintf(&b, "## Options\n\n```\n%s```\n\n", f)
	}
	if f := cmd.InheritedFlags().FlagUsages(); f != "" {
		fmt.Fprintf(&b, "## Options inherited from parent commands\n\n```\n%s```\n\n", f)
	}
	var subs []string
	for _, sub := range cmd.Commands() {
		if !sub.Hidden && sub.Name() != "help" {
			subs = append(subs, fmt.Sprintf("- [%s](%s.md): %s", sub.CommandPath(), strings.ReplaceAll(sub.CommandPath(), " ", "_"), sub.Short))
		}
	}
	if len(subs) > 0 {
		b.WriteString("## Subcommands\n\n" + strings.Join(subs, "\n") + "\n")
	}
	return b.String()
}
