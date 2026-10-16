package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/brand"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

type personalSkill struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	SizeBytes int64  `json:"sizeBytes"`
	CreatedAt string `json:"createdAt"`
	Promotion *struct {
		State string `json:"state"`
	} `json:"promotion"`
}

// listPersonalSkills prints the skills only the caller can see.
func listPersonalSkills(cmd *cobra.Command, f approvalFlags) error {
	c, err := approvalsClient(f.gatewayURL)
	if err != nil {
		return err
	}
	var doc struct {
		Skills []personalSkill `json:"skills"`
	}
	raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/marketplace/skills?scope=personal", nil, &doc)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if f.jsonOut {
		printRaw(out, raw)
		return nil
	}
	if len(doc.Skills) == 0 {
		fmt.Fprintf(out, "No personal skills. Add one with `%s`.\n", brand.Cmd("skill publish <dir> --scope personal"))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tVERSION\tSIZE\tORGANISATION")
	for _, s := range doc.Skills {
		org := "-"
		if s.Promotion != nil && s.Promotion.State != "" {
			org = textsafe.Clean(s.Promotion.State)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", textsafe.Clean(s.ID), textsafe.Clean(s.Name), textsafe.Clean(s.Version), humanBytes(s.SizeBytes), org)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\n`%s` installs them on this machine.\n", brand.Cmd("sync"))
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// SkillPromoteCmd returns `zuano skill promote`.
func SkillPromoteCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "promote <skill-id>",
		Short: "Submit a personal skill to your organisation",
		Long: `Sends a copy of a personal skill through the organisation's publish pipeline: the
same checks, and an administrator's review where the organisation requires it.
The personal skill stays until the organisation's version is approved; then you
choose whether to keep it. A plan without private publishing refuses (exit 30).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if id == "" || strings.ContainsAny(id, "/?#% ") {
				return fmt.Errorf("%q is not a skill id", textsafe.Clean(id))
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var res struct {
				VersionID string `json:"versionId"`
				State     string `json:"state"`
				ReviewURL string `json:"reviewUrl"`
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/marketplace/resources/"+url.PathEscape(id)+"/promote", map[string]any{}, &res)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			fmt.Fprintf(out, "Submitted %s to your organisation (%s).\n", textsafe.Clean(id), orDash(res.State))
			if res.ReviewURL != "" {
				link, err := gatewayLink(c.GatewayURL, res.ReviewURL)
				if err != nil {
					link = textsafe.Clean(res.ReviewURL)
				}
				fmt.Fprintf(out, "An administrator reviews it here: %s\n", link)
			}
			return nil
		},
	}
	f.bind(cmd, "Print the gateway's answer as JSON")
	return cmd
}
