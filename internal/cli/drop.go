package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// siteDoc mirrors a site from the gateway's artifact routes.
type siteDoc struct {
	ID              string  `json:"id"`
	Slug            string  `json:"slug"`
	Title           string  `json:"title"`
	URL             string  `json:"url"`
	Routing         string  `json:"routing"`
	Status          string  `json:"status"`
	Audience        string  `json:"audience"`
	ActiveReleaseID *string `json:"active_release_id"`
	ActiveReleaseAt string  `json:"active_release_at"`
	TotalBytes      int64   `json:"total_bytes"`
	ContentRevision int64   `json:"content_revision"`
	PolicyRevision  int64   `json:"policy_revision"`
	PublicRead      bool    `json:"public_read"`
	PublicExpiresAt *string `json:"public_expires_at"`
	SourceDownload  bool    `json:"source_download"`
}

type releaseWarning struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type releaseFailure struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Candidates []string `json:"candidates"`
	Files      []struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"files"`
}

// releaseDoc mirrors a release.
type releaseDoc struct {
	ID         string           `json:"id"`
	SiteID     string           `json:"site_id"`
	State      string           `json:"state"`
	EntryPath  string           `json:"entry_path"`
	TotalBytes int64            `json:"total_bytes"`
	FileCount  int              `json:"file_count"`
	CreatedAt  string           `json:"created_at"`
	ReadyAt    string           `json:"ready_at"`
	PreviewURL string           `json:"preview_url"`
	Warnings   []releaseWarning `json:"warnings"`
	Failure    *releaseFailure  `json:"failure"`
}

var siteIDPattern = regexp.MustCompile(`^ast_[A-Za-z0-9_-]{6,40}$`)

// DropCmd returns `zuano drop`, which publishes files and folders as sites on
// the gateway's isolated user-content domain.
func DropCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drop",
		Short: "Publish files and folders as sites and manage who can see them",
		Long: `A site is a file or folder served on its own address, with immutable releases:
publishing uploads a new release and makes it live, rollback makes an earlier
one live again. A site is visible only to the people, groups or organisation
you share it with unless you make it public.

An agent session's publish, public share and delete are held for approval
(exit 12), like other writes.`,
	}
	cmd.AddCommand(dropPublishCmd(), dropListCmd(), dropReleasesCmd(), dropRollbackCmd(), dropShareCmd(), dropOpenCmd(), dropDeleteCmd(), dropRestoreCmd())
	return cmd
}

// resolveSite finds a site by id or slug. status "deleted" looks among the
// deleted sites, for restore.
func resolveSite(cmd *cobra.Command, c *client.Client, ref, status string) (siteDoc, error) {
	if siteIDPattern.MatchString(ref) {
		var doc struct {
			Site siteDoc `json:"site"`
		}
		if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/artifacts/sites/"+url.PathEscape(ref), nil, &doc); err != nil {
			return siteDoc{}, err
		}
		return doc.Site, nil
	}
	path := "/api/artifacts/sites"
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	var list struct {
		Sites []siteDoc `json:"sites"`
	}
	if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, path, nil, &list); err != nil {
		return siteDoc{}, err
	}
	for _, s := range list.Sites {
		if s.Slug == ref {
			return s, nil
		}
	}
	return siteDoc{}, fmt.Errorf("no site %q; `zuano drop list` shows yours", textsafe.Clean(ref))
}

func dropListCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the sites you can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			var list struct {
				Sites []siteDoc `json:"sites"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/artifacts/sites", nil, &list)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			if len(list.Sites) == 0 {
				fmt.Fprintln(out, "No sites.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SITE\tTITLE\tVISIBLE TO\tLIVE SINCE\tSIZE\tURL")
			for _, s := range list.Sites {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", textsafe.Clean(s.Slug), textsafe.Clean(s.Title), orDash(textsafe.Clean(s.Audience)), orDash(textsafe.Clean(s.ActiveReleaseAt)), s.TotalBytes, textsafe.Clean(s.URL))
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func dropReleasesCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "releases <site>",
		Short: "List a site's releases, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			site, err := resolveSite(cmd, c, args[0], "")
			if err != nil {
				return err
			}
			var list struct {
				Releases []releaseDoc `json:"releases"`
			}
			raw, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/artifacts/sites/"+url.PathEscape(site.ID)+"/releases", nil, &list)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.jsonOut {
				printRaw(out, raw)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "RELEASE\tSTATE\tCREATED\tFILES\tSIZE\tLIVE")
			for _, r := range list.Releases {
				live := ""
				if site.ActiveReleaseID != nil && *site.ActiveReleaseID == r.ID {
					live = "live"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", textsafe.Clean(r.ID), textsafe.Clean(r.State), textsafe.Clean(r.CreatedAt), r.FileCount, r.TotalBytes, live)
			}
			return tw.Flush()
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func dropRollbackCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "rollback <site> <release>",
		Short: "Make an earlier release live again",
		Long: `Makes a retained release live. Only the content changes: who can see the site
does not. A change to the site's audience since that release was published is
refused (audience_changed); publish again to review it.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			site, err := resolveSite(cmd, c, args[0], "")
			if err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/artifacts/sites/"+url.PathEscape(site.ID)+"/activate",
				map[string]any{"release_id": args[1], "expected_content_revision": site.ContentRevision}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is live again at %s\n", textsafe.Clean(args[1]), textsafe.Clean(site.URL))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}

func dropOpenCmd() *cobra.Command {
	var f approvalFlags
	var preview string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "open <site>",
		Short: "Open a site, or a release's private preview, in the browser",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			site, err := resolveSite(cmd, c, args[0], "")
			if err != nil {
				return err
			}
			target := site.URL
			if preview != "" {
				var doc struct {
					Release releaseDoc `json:"release"`
				}
				if _, err := gatewayJSON(cmd.Context(), c, http.MethodGet, "/api/artifacts/releases/"+url.PathEscape(preview), nil, &doc); err != nil {
					return err
				}
				target = doc.Release.PreviewURL
			}
			u, err := url.Parse(target)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return errors.New("the gateway gave no web address to open for this site")
			}
			if !openIfInteractive(cmd, u.String(), noBrowser) {
				fmt.Fprintln(cmd.OutOrStdout(), textsafe.Clean(u.String()))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Opened %s\n", textsafe.Clean(u.String()))
			return nil
		},
	}
	f.bind(cmd, "Unused")
	_ = cmd.Flags().MarkHidden("json")
	cmd.Flags().StringVar(&preview, "preview", "", "Open this release's private preview instead of the live site")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the address instead of opening it")
	return cmd
}

// askDefaultNo asks a question whose answer is no unless the person types yes.
func askDefaultNo(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprint(out, question)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// confirmOrYes returns nil when the change may go ahead: --yes was given, or a
// person at a terminal agreed.
func confirmOrYes(cmd *cobra.Command, yes bool, question string) error {
	if yes {
		return nil
	}
	if !canPrompt(cmd.InOrStdin()) {
		return errors.New("this needs a confirmation and there is no terminal to ask on; repeat the command with --yes")
	}
	if !askDefaultNo(cmd.InOrStdin(), cmd.OutOrStdout(), question+" [y/N] ") {
		return &ExitError{Code: 1, Msg: "cancelled; nothing was changed"}
	}
	return nil
}

func dropDeleteCmd() *cobra.Command {
	var f approvalFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <site>",
		Short: "Delete a site (it can be restored for a while)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			site, err := resolveSite(cmd, c, args[0], "")
			if err != nil {
				return err
			}
			if err := confirmOrYes(cmd, yes, fmt.Sprintf("Delete %s (%s)? Its address stops working now.", textsafe.Clean(site.Slug), textsafe.Clean(site.URL))); err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodDelete, "/api/artifacts/sites/"+url.PathEscape(site.ID), nil, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				if len(raw) == 0 {
					raw = []byte(`{"deleted":true}`)
				}
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s. `%s` brings it back, private, until it is purged.\n", textsafe.Clean(site.Slug), "zuano drop restore "+textsafe.Clean(site.Slug))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	cmd.Flags().BoolVar(&yes, "yes", false, "Do not ask for confirmation")
	return cmd
}

func dropRestoreCmd() *cobra.Command {
	var f approvalFlags
	cmd := &cobra.Command{
		Use:   "restore <site>",
		Short: "Bring back a deleted site, private",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			site, err := resolveSite(cmd, c, args[0], "deleted")
			if err != nil {
				return err
			}
			raw, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/artifacts/sites/"+url.PathEscape(site.ID)+"/restore", map[string]any{}, nil)
			if err != nil {
				return err
			}
			if f.jsonOut {
				printRaw(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored %s. It is visible to its owner only; share it again with `zuano drop share`.\n", textsafe.Clean(site.Slug))
			return nil
		},
	}
	f.bind(cmd, "Print the response body unchanged")
	return cmd
}
