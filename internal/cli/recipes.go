package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/recipes"
	"github.com/patrikmichi/zuano/internal/signing"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// recipeKeyTTL is how long the published recipe keys are reused.
const recipeKeyTTL = 24 * time.Hour

// RecipesCmd returns `zuano recipes`: the playbooks the gateway serves with
// tool search, which tools to call in what order and what goes wrong.
func RecipesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recipes",
		Short: "Read the recipes agents are served and make one into a skill",
		Long: `A recipe is a playbook for using a few tools well: the steps in order, which are
optional, the pitfalls and how to avoid them, and how well it has worked. The
gateway serves the best matching ones next to tool search results. Recipes come
from the platform, from your organisation or are mined from its usage.

'show' checks the recipe's signature against the published keys and exits 4 when
it does not verify. Everything printed from a recipe has control and invisible
characters removed.`,
	}
	cmd.AddCommand(recipesListCmd(), recipesShowCmd(), recipesVersionsCmd(), recipesPromoteCmd(), recipesSettingsCmd())
	return cmd
}

func recipePath(id string, rest ...string) string {
	return builderAPI + "/recipes/" + url.PathEscape(id) + strings.Join(rest, "")
}

func recipeRef(arg string) (string, error) {
	if arg == "" || strings.ContainsAny(arg, "/?#%: \t\n") {
		return "", fmt.Errorf("%q is not a recipe id or slug", textsafe.Clean(arg))
	}
	return arg, nil
}

func pctOrDash(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *v*100)
}

func recipesListCmd() *cobra.Command {
	var f approvalFlags
	var service, source, status, query, cursor string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recipes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := map[string]string{"service": service, "source": source, "status": status, "q": query, "cursor": cursor, "forCaller": "true"}
			if limit > 0 {
				q["limit"] = strconv.Itoa(limit)
			}
			var doc struct {
				Items      []recipes.Recipe `json:"items"`
				NextCursor string           `json:"nextCursor"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(builderAPI+"/recipes", q), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(doc.Items) == 0 {
				fmt.Fprintln(out, "No recipes match.")
				return nil
			}
			var rows [][]string
			for _, r := range doc.Items {
				rows = append(rows, []string{r.ID, r.Title, strings.Join(r.Services, ","), strconv.Itoa(len(r.Steps)), strconv.Itoa(len(r.Pitfalls)), pctOrDash(r.Evidence.SuccessRate), r.Source, "v" + strconv.Itoa(r.Version)})
			}
			printRows(out, []string{"ID", "TITLE", "SERVICES", "STEPS", "PITFALLS", "SUCCESS", "SOURCE", "VERSION"}, rows)
			if doc.NextCursor != "" {
				fmt.Fprintf(out, "\nMore: --cursor %s\n", textsafe.Clean(doc.NextCursor))
			}
			return nil
		},
	}
	f.bind(cmd, "Print the list as JSON")
	cmd.Flags().StringVar(&service, "service", "", "Only recipes that use this service")
	cmd.Flags().StringVar(&source, "source", "", "platform, mined or org")
	cmd.Flags().StringVar(&status, "status", "", "draft, active, suspended or retired")
	cmd.Flags().StringVar(&query, "query", "", "Words in the title or use case")
	cmd.Flags().IntVar(&limit, "limit", 0, "Page size, at most 100")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue from a previous page")
	return cmd
}

// recipeKeys returns the published keys for a recipe signed with kid.
func recipeKeys(ctx context.Context, gatewayURL, kid string) ([]signing.Key, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	cache := recipes.KeyCache{
		Path: filepath.Join(dir, "recipes", "keys.json"), TTL: recipeKeyTTL,
		Fetch: func(ctx context.Context, origin string) ([]signing.Key, error) {
			return signing.FetchKeysAt(ctx, origin, recipes.KeysPath)
		},
	}
	return cache.Keys(ctx, gatewayURL, kid)
}

func recipesShowCmd() *cobra.Command {
	var f approvalFlags
	var version int
	cmd := &cobra.Command{
		Use:   "show <id|slug>",
		Short: "Show a recipe and check its signature",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := recipeRef(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			q := map[string]string{"forCaller": "true"}
			if version > 0 {
				q["version"] = strconv.Itoa(version)
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, withQuery(recipePath(ref), q), nil, nil)
			if err != nil {
				return err
			}
			r, err := recipes.Decode(raw)
			if err != nil {
				return fmt.Errorf("decode the recipe: %w", err)
			}
			v := recipes.Verification{Reason: "the recipe is not signed"}
			if keys, kerr := recipeKeys(cmd.Context(), c.GatewayURL, r.Signature.KeyID); kerr != nil {
				v.Reason = "the published keys could not be read: " + kerr.Error()
			} else {
				v = recipes.Verify(r, keys)
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
			} else {
				renderRecipe(out, r, v)
			}
			if !v.Verified {
				return &signing.ContentError{Reason: v.Reason}
			}
			return nil
		},
	}
	f.bind(cmd, "Print the recipe as the gateway sent it")
	cmd.Flags().IntVar(&version, "version", 0, "A version other than the current one")
	return cmd
}

func renderRecipe(w io.Writer, r recipes.Recipe, v recipes.Verification) {
	fmt.Fprintf(w, "%s (%s)\n", textsafe.Strict(r.Title), textsafe.Clean(r.ID))
	fmt.Fprintf(w, "%s · %d steps · %d pitfalls · %s · v%d · %s\n", textsafe.Strict(strings.Join(r.Services, ", ")), len(r.Steps), len(r.Pitfalls), textsafe.Clean(r.Source), r.Version, textsafe.Clean(r.Status))
	fmt.Fprintf(w, "Difficulty: %s (%s)\n", textsafe.Clean(r.Difficulty.Level), textsafe.Strict(r.Difficulty.Reason))
	fmt.Fprintf(w, "Use for: %s\n", textsafe.Strict(r.UseCase))
	if len(r.PrimaryTools) > 0 {
		fmt.Fprintf(w, "Tools: %s\n", textsafe.Strict(strings.Join(r.PrimaryTools, ", ")))
	}
	if len(r.RelatedTools) > 0 {
		fmt.Fprintf(w, "Related: %s\n", textsafe.Strict(strings.Join(r.RelatedTools, ", ")))
	}
	idem := map[string]string{}
	for _, t := range r.Tools {
		idem[t.Tool] = t.Idempotency
	}
	fmt.Fprintln(w, "\nSteps")
	for _, s := range r.Steps {
		kind := ""
		switch s.Kind {
		case "optional":
			kind = " (optional: " + textsafe.Strict(s.Condition) + ")"
		case "fallback":
			kind = fmt.Sprintf(" (fallback for step %d: %s)", s.FallbackFor, textsafe.Strict(s.Condition))
		}
		fmt.Fprintf(w, "%3d. %s%s\n", s.Order, textsafe.Strict(s.Tool), kind)
		fmt.Fprintf(w, "     %s\n", textsafe.Strict(s.Instruction))
		var notes []string
		if s.Risk != "" {
			notes = append(notes, "risk "+textsafe.Clean(s.Risk))
		}
		switch s.Approval {
		case "required":
			notes = append(notes, "needs your approval")
		case "covered_by_grant":
			notes = append(notes, "covered by your grant")
		}
		if i := idem[s.Tool]; i != "" {
			notes = append(notes, textsafe.Clean(strings.ReplaceAll(i, "_", " ")))
		}
		if len(notes) > 0 {
			fmt.Fprintf(w, "     [%s]\n", strings.Join(notes, "; "))
		}
	}
	if len(r.Pitfalls) > 0 {
		fmt.Fprintln(w, "\nPitfalls")
		last := ""
		for _, p := range r.Pitfalls {
			if p.Tool != last {
				fmt.Fprintf(w, "  %s\n", textsafe.Strict(p.Tool))
				last = p.Tool
			}
			codes := ""
			if len(p.ErrorCodes) > 0 {
				codes = " [" + textsafe.Strict(strings.Join(p.ErrorCodes, ", ")) + "]"
			}
			fmt.Fprintf(w, "    - %s%s\n      Instead: %s\n", textsafe.Strict(p.Text), codes, textsafe.Strict(p.Fix))
		}
	}
	e := r.Evidence
	if e.SuccessRate != nil {
		low := ""
		if e.SuccessRateLow != nil {
			low = fmt.Sprintf(", at least %s with 95%% confidence", pctOrDash(e.SuccessRateLow))
		}
		n := ""
		if e.SampleSize != nil {
			n = fmt.Sprintf(" over %d+ runs", *e.SampleSize)
		}
		fmt.Fprintf(w, "\nEvidence: %s succeeded%s%s in the last %d days\n", pctOrDash(e.SuccessRate), n, low, e.WindowDays)
	}
	if e.LastEval != nil {
		fmt.Fprintf(w, "Last eval: score %.2f, %s, %s\n", e.LastEval.Score, map[bool]string{true: "passed", false: "failed"}[e.LastEval.Passed], textsafe.Clean(e.LastEval.At))
	}
	if v.Verified {
		fmt.Fprintf(w, "\nsignature: verified (%s, key %s)\n", textsafe.Clean(v.Signer), textsafe.Clean(v.KeyID))
	} else {
		fmt.Fprintf(w, "\nsignature: NOT VERIFIED: %s\n", textsafe.Strict(v.Reason))
	}
}

func recipesVersionsCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "versions <id>",
		Short: "List the versions of a recipe",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := recipeRef(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Items []struct {
					Version int    `json:"version"`
					Status  string `json:"status"`
					At      string `json:"at"`
					By      string `json:"by"`
				} `json:"items"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodGet, recipePath(ref, "/versions"), nil, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			var rows [][]string
			for _, v := range doc.Items {
				rows = append(rows, []string{"v" + strconv.Itoa(v.Version), v.Status, v.At, orDash(v.By)})
			}
			printRows(out, []string{"VERSION", "STATUS", "WHEN", "BY"}, rows)
			return nil
		},
	}
	f.bind(cmd, "Print the versions as JSON")
	return cmd
}

func recipesPromoteCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "promote <id>",
		Short: "Make a recipe into a draft skill your agents can install",
		Long: `Builds a skill from the recipe (its steps, pitfalls and notes), keeps it private
to you as a draft and copies the recipe's eval cases to it. The draft goes
through the usual publish checks and review before others get it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := recipeRef(args[0])
			if err != nil {
				return err
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var doc struct {
				Skill struct {
					ID   string `json:"id"`
					Slug string `json:"slug"`
					URL  string `json:"url"`
				} `json:"skill"`
				VersionID string `json:"versionId"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, http.MethodPost, recipePath(ref, ":promote"), map[string]any{}, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "Created draft skill %s (%s)\n", textsafe.Clean(doc.Skill.Slug), textsafe.Clean(doc.Skill.ID))
			if doc.Skill.URL != "" {
				if link, err := gatewayLink(c.GatewayURL, doc.Skill.URL); err == nil {
					fmt.Fprintf(out, "  %s\n", link)
				}
			}
			fmt.Fprintf(out, "Next: score it with `%s`, then submit it with `%s`.\n", brand.Cmd("eval run --target skill:"+textsafe.Clean(doc.Skill.Slug)), brand.Cmd("skill promote "+textsafe.Clean(doc.Skill.ID)))
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	return cmd
}

func recipesSettingsCmd() *cobra.Command {
	var f approvalFlags
	var contribute, serve, mine string
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show or change how your organisation takes part in recipes",
		Long: `Without flags, prints the settings. Changing them is an administrator's, is
audited, and is refused in an agent session (exit 10).

  --contribute on|off   share anonymised tool-usage patterns to improve recipes for everyone
  --serve all|org_only|off   which recipes are served with tool search
  --mine on|off         mine your own usage for private recipes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			changing := cmd.Flags().Changed("contribute") || cmd.Flags().Changed("serve") || cmd.Flags().Changed("mine")
			body := map[string]any{}
			if changing {
				if err := refuseAgentChange(brand.Cmd("recipes settings")); err != nil {
					return err
				}
				onOff := func(name, v string) (bool, error) {
					switch v {
					case "on":
						return true, nil
					case "off":
						return false, nil
					}
					return false, fmt.Errorf("--%s must be on or off", name)
				}
				if cmd.Flags().Changed("contribute") {
					v, err := onOff("contribute", contribute)
					if err != nil {
						return err
					}
					body["contributeCrossOrg"] = v
				}
				if cmd.Flags().Changed("mine") {
					v, err := onOff("mine", mine)
					if err != nil {
						return err
					}
					body["mineOrgPrivate"] = v
				}
				if cmd.Flags().Changed("serve") {
					if serve != "all" && serve != "org_only" && serve != "off" {
						return errors.New("--serve must be all, org_only or off")
					}
					body["serveRecipes"] = serve
				}
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			method := http.MethodGet
			var payload any
			if changing {
				method, payload = http.MethodPut, body
			}
			var doc struct {
				Settings struct {
					ContributeCrossOrg bool   `json:"contributeCrossOrg"`
					ServeRecipes       string `json:"serveRecipes"`
					MineOrgPrivate     bool   `json:"mineOrgPrivate"`
				} `json:"settings"`
			}
			raw, err := builderRequest(cmd, c, f.jsonOut, method, builderAPI+"/org/recipe-settings", payload, &doc)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			s := doc.Settings
			onoff := func(b bool) string { return map[bool]string{true: "on", false: "off"}[b] }
			fmt.Fprintf(out, "contribute: %s\nserve: %s\nmine: %s\n", onoff(s.ContributeCrossOrg), orDash(s.ServeRecipes), onoff(s.MineOrgPrivate))
			return nil
		},
	}
	f.bind(cmd, "Print the settings as JSON")
	cmd.Flags().StringVar(&contribute, "contribute", "", "on or off")
	cmd.Flags().StringVar(&serve, "serve", "", "all, org_only or off")
	cmd.Flags().StringVar(&mine, "mine", "", "on or off")
	return cmd
}
