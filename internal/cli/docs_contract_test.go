package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// assertDocumented fails when a visible command or flag under root is missing
// from the reference page, so the page cannot fall behind the commands.
func assertDocumented(t *testing.T, root *cobra.Command, rootPath, docFile string) {
	t.Helper()
	raw, err := os.ReadFile(docFile)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Hidden {
			return
		}
		if path != rootPath && !strings.Contains(doc, path) {
			t.Errorf("command %q is not in %s", path, docFile)
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" || f.Name == "gateway-url" || f.Name == "json" {
				return
			}
			if !strings.Contains(doc, "--"+f.Name) {
				t.Errorf("flag --%s of %q is not in %s", f.Name, path, docFile)
			}
		})
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(root, rootPath)
}

func TestEvalReferenceDocumentsEveryCommandAndFlag(t *testing.T) {
	assertDocumented(t, EvalCmd(), "zuano eval", "../../docs/reference/evals.md")
}

func TestTriggersReferenceDocumentsEveryCommandAndFlag(t *testing.T) {
	assertDocumented(t, TriggersCmd(), "zuano triggers", "../../docs/reference/triggers.md")
	assertDocumented(t, DevCmd(), "zuano dev", "../../docs/reference/triggers.md")
	assertDocumented(t, GenerateCmd(), "zuano generate", "../../docs/reference/triggers.md")
}

func TestBuilderReferenceDocumentsEveryCommandAndFlag(t *testing.T) {
	for _, c := range []struct {
		cmd  *cobra.Command
		path string
	}{{AuthConfigsCmd(), "zuano auth-configs"}, {AccountsCmd(), "zuano accounts"}, {LinksCmd(), "zuano links"}, {BuilderCmd(), "zuano builder"}} {
		assertDocumented(t, c.cmd, c.path, "../../docs/reference/builder.md")
	}
}

func TestRecipesReferenceDocumentsEveryCommandAndFlag(t *testing.T) {
	assertDocumented(t, RecipesCmd(), "zuano recipes", "../../docs/reference/recipes.md")
}
