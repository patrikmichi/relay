package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
)

type grantDoc struct {
	PrincipalType  string  `json:"principal_type"`
	PrincipalID    string  `json:"principal_id,omitempty"`
	PrincipalLabel string  `json:"principal_label,omitempty"`
	PrincipalEmail string  `json:"principal_email,omitempty"`
	Role           string  `json:"role"`
	ExpiresAt      *string `json:"expires_at,omitempty"`
}

func (g grantDoc) label() string {
	switch g.PrincipalType {
	case "org":
		return "everyone in the organisation"
	case "group":
		return "group " + firstNonEmptyString(g.PrincipalLabel, g.PrincipalID)
	}
	return firstNonEmptyString(g.PrincipalLabel, g.PrincipalEmail, g.PrincipalID)
}

// shareDiff mirrors the answer to a sharing proposal.
type shareDiff struct {
	Diff struct {
		Gain   []grantDoc `json:"gain"`
		Lose   []grantDoc `json:"lose"`
		Public struct {
			Before bool `json:"before"`
			After  bool `json:"after"`
		} `json:"public"`
	} `json:"diff"`
	PolicyRevision int64 `json:"policy_revision"`
}

func (d shareDiff) empty() bool {
	return len(d.Diff.Gain) == 0 && len(d.Diff.Lose) == 0 && d.Diff.Public.Before == d.Diff.Public.After
}

var expiryPattern = regexp.MustCompile(`^(\d+)([mhdw])$`)

// parseExpiry reads 30m, 12h, 7d, 2w or a date or timestamp and returns the
// moment it denotes in RFC 3339.
func parseExpiry(s string, now time.Time) (string, error) {
	if m := expiryPattern.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		if n == 0 {
			return "", fmt.Errorf("invalid expiry %q: it must be positive", s)
		}
		return now.UTC().Add(time.Duration(n) * unit).Format(time.RFC3339), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			if !t.After(now) {
				return "", fmt.Errorf("invalid expiry %q: it is in the past", s)
			}
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("invalid expiry %q: use 7d, 12h, 30m, a date such as 2026-12-31 or a timestamp", s)
}

type shareFlags struct {
	add, groups, remove []string
	org, public         bool
	role, expires       string
	private             bool
	yes                 bool
}

// desiredGrants applies the flags to the site's current grants.
func desiredGrants(current []grantDoc, fl shareFlags) ([]grantDoc, error) {
	if fl.role != "viewer" && fl.role != "editor" {
		return nil, fmt.Errorf("invalid --role %q: use viewer or editor", textsafe.Clean(fl.role))
	}
	var out []grantDoc
	for _, g := range current {
		if fl.private && g.Role != "owner" {
			continue
		}
		out = append(out, g)
	}
	for _, r := range fl.remove {
		if r == "public" {
			continue
		}
		before := len(out)
		out = removeGrant(out, r)
		if len(out) == before {
			return nil, fmt.Errorf("--remove %q matches no one who has access", textsafe.Clean(r))
		}
	}
	add := func(g grantDoc) {
		for i := range out {
			if out[i].PrincipalType == g.PrincipalType && (g.PrincipalType == "org" || strings.EqualFold(out[i].label(), g.label())) {
				if out[i].Role != "owner" {
					out[i].Role = g.Role
				}
				return
			}
		}
		out = append(out, g)
	}
	for _, email := range fl.add {
		if !strings.Contains(email, "@") {
			return nil, fmt.Errorf("--add %q is not an email address; use --group for a group", textsafe.Clean(email))
		}
		add(grantDoc{PrincipalType: "user", PrincipalEmail: email, PrincipalLabel: email, Role: fl.role})
	}
	for _, name := range fl.groups {
		add(grantDoc{PrincipalType: "group", PrincipalID: name, PrincipalLabel: name, Role: fl.role})
	}
	if fl.org {
		add(grantDoc{PrincipalType: "org", Role: fl.role})
	}
	return out, nil
}

func removeGrant(grants []grantDoc, target string) []grantDoc {
	var kept []grantDoc
	for _, g := range grants {
		match := false
		switch {
		case target == "org":
			match = g.PrincipalType == "org"
		case strings.HasPrefix(target, "group:"):
			match = g.PrincipalType == "group" && (g.PrincipalLabel == target[6:] || g.PrincipalID == target[6:])
		default:
			match = g.PrincipalType == "user" && (strings.EqualFold(g.PrincipalLabel, target) || strings.EqualFold(g.PrincipalEmail, target))
		}
		if !match {
			kept = append(kept, g)
		}
	}
	return kept
}

func dropShareCmd() *cobra.Command {
	var f approvalFlags
	var fl shareFlags
	cmd := &cobra.Command{
		Use:   "share <site>",
		Short: "Show or change who can see a site",
		Long: `Without changes, prints who can see the site. With changes, first asks the
gateway what would change (who gains or loses access, and whether the site
becomes public), shows that, and applies it after you confirm (--yes skips the
question; without a terminal --yes is required). A change to the site's
sharing since you looked is refused.

Remove with --remove EMAIL, --remove group:NAME, --remove org or
--remove public. --private removes everyone but the owner and takes the site
off the public. Making a site public is held for approval in an agent
session (exit 12).`,
		Example: `  zuano drop share docs --add ana@example.com --group eng
  zuano drop share docs --public --expires 7d
  zuano drop share docs --private --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShare(cmd, f, fl, args[0])
		},
	}
	f.bind(cmd, "Print the proposal or the result as JSON")
	fl.role = "viewer"
	cmd.Flags().StringArrayVar(&fl.add, "add", nil, "Give this person (email) access (repeatable)")
	cmd.Flags().StringArrayVar(&fl.groups, "group", nil, "Give this group access (repeatable)")
	cmd.Flags().BoolVar(&fl.org, "org", false, "Give everyone in the organisation access")
	cmd.Flags().StringVar(&fl.role, "role", "viewer", "Role for what --add, --group and --org grant: viewer or editor")
	cmd.Flags().BoolVar(&fl.public, "public", false, "Let anyone with the address read the site")
	cmd.Flags().StringVar(&fl.expires, "expires", "", "End public access after this long (7d, 12h) or on this date")
	cmd.Flags().StringArrayVar(&fl.remove, "remove", nil, "Take access away: EMAIL, group:NAME, org or public (repeatable)")
	cmd.Flags().BoolVar(&fl.private, "private", false, "Remove everyone but the owner and end public access")
	cmd.Flags().BoolVar(&fl.yes, "yes", false, "Apply without asking")
	return cmd
}

func runShare(cmd *cobra.Command, f approvalFlags, fl shareFlags, ref string) error {
	if fl.expires != "" && !fl.public {
		return errors.New("--expires applies to --public")
	}
	c, err := approvalsClient(f.gatewayURL)
	if err != nil {
		return err
	}
	site, err := resolveSite(cmd, c, ref, "")
	if err != nil {
		return err
	}
	var detail struct {
		Site   siteDoc    `json:"site"`
		Grants []grantDoc `json:"grants"`
	}
	if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/artifacts/sites/"+url.PathEscape(site.ID), nil, &detail); err != nil {
		return err
	}
	site = detail.Site
	out := cmd.OutOrStdout()

	changing := len(fl.add)+len(fl.groups)+len(fl.remove) > 0 || fl.org || fl.public || fl.private
	if !changing {
		printSharing(out, site, detail.Grants)
		return nil
	}
	grants, err := desiredGrants(detail.Grants, fl)
	if err != nil {
		return err
	}
	public := map[string]any{"enabled": site.PublicRead}
	switch {
	case fl.private:
		public["enabled"] = false
	case fl.public:
		public["enabled"] = true
		if fl.expires != "" {
			if public["expires_at"], err = parseExpiry(fl.expires, time.Now()); err != nil {
				return err
			}
		}
	}
	for _, r := range fl.remove {
		if r == "public" {
			public["enabled"] = false
		}
	}
	body := map[string]any{"grants": grants, "public": public, "source_download": site.SourceDownload, "expected_policy_revision": site.PolicyRevision}
	base := "/api/artifacts/sites/" + url.PathEscape(site.ID) + "/sharing"

	var proposal shareDiff
	if _, err := gatewayJSON(cmd.Context(), c, http.MethodPut, base+"?mode=propose", body, &proposal); err != nil {
		return err
	}
	if proposal.empty() {
		fmt.Fprintln(out, "No change: the site is already shared that way.")
		return nil
	}
	if !f.jsonOut {
		printShareDiff(out, proposal)
	}
	if !fl.yes && f.jsonOut {
		return errors.New("--json with a change needs --yes: there is no prompt to answer")
	}
	if err := confirmOrYes(cmd, fl.yes, "Apply this?"); err != nil {
		return err
	}
	body["expected_policy_revision"] = proposal.PolicyRevision
	raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPut, base+"?mode=apply", body, nil)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printRaw(out, raw)
		return nil
	}
	fmt.Fprintf(out, "Sharing of %s updated.\n", textsafe.Clean(site.Slug))
	return nil
}

func printSharing(out io.Writer, s siteDoc, grants []grantDoc) {
	fmt.Fprintf(out, "%s  %s\n", textsafe.Clean(s.Slug), textsafe.Clean(s.URL))
	public := "off"
	if s.PublicRead {
		public = "on"
		if s.PublicExpiresAt != nil && *s.PublicExpiresAt != "" {
			public += ", until " + textsafe.Clean(*s.PublicExpiresAt)
		}
	}
	fmt.Fprintf(out, "Public: %s\n", public)
	for _, g := range grants {
		line := fmt.Sprintf("  %-8s %s", textsafe.Clean(g.Role), textsafe.Clean(g.label()))
		if g.ExpiresAt != nil && *g.ExpiresAt != "" {
			line += " (until " + textsafe.Clean(*g.ExpiresAt) + ")"
		}
		fmt.Fprintln(out, line)
	}
}

func printShareDiff(out io.Writer, d shareDiff) {
	for _, g := range d.Diff.Gain {
		fmt.Fprintf(out, "+ %s (%s)\n", textsafe.Clean(g.label()), textsafe.Clean(g.Role))
	}
	for _, g := range d.Diff.Lose {
		fmt.Fprintf(out, "- %s\n", textsafe.Clean(g.label()))
	}
	if d.Diff.Public.Before != d.Diff.Public.After {
		state := map[bool]string{true: "on", false: "off"}
		fmt.Fprintf(out, "Public access: %s -> %s\n", state[d.Diff.Public.Before], state[d.Diff.Public.After])
	}
}
