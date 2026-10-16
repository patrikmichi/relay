package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Every evaluation command and flag appears in the reference page, so the page
// cannot fall behind the commands.
func TestEvalReferenceDocumentsEveryCommandAndFlag(t *testing.T) {
	raw, err := os.ReadFile("../../docs/reference/evals.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Hidden {
			return
		}
		if path != "zuano eval" && !strings.Contains(doc, path) {
			t.Errorf("command %q is not in docs/reference/evals.md", path)
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" || f.Name == "gateway-url" || f.Name == "json" {
				return
			}
			if !strings.Contains(doc, "--"+f.Name) {
				t.Errorf("flag --%s of %q is not in docs/reference/evals.md", f.Name, path)
			}
		})
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(EvalCmd(), "zuano eval")
}
