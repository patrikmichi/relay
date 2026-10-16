package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/drop"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// Timings are variables so tests can shorten them.
var (
	dropPollInterval = time.Second
	dropPollTimeout  = 5 * time.Minute
)

const dropUploadWorkers = 4

var siteSlugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9])*$`)

// slugFrom derives a site slug from a file or folder name: lower case letters
// and digits separated by single hyphens, 3 to 30 characters.
func slugFrom(name string) (string, error) {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	slug := b.String()
	if len(slug) > 30 {
		slug = strings.TrimRight(slug[:30], "-")
	}
	if len(slug) < 3 || !siteSlugPattern.MatchString(slug) {
		return "", fmt.Errorf("cannot make a site name (3 to 30 letters, digits and hyphens) from %q; give one with --site", textsafe.Clean(name))
	}
	return slug, nil
}

// uploadSlot is the gateway's answer for one file of an upload.
type uploadSlot struct {
	Path    string        `json:"path"`
	FileKey string        `json:"file_key"`
	Status  string        `json:"status"`
	Upload  *uploadTarget `json:"upload"`
}

func dropPublishCmd() *cobra.Command {
	var f approvalFlags
	var site, title, entry string
	var spa, noActivate bool
	cmd := &cobra.Command{
		Use:   "publish <path>",
		Short: "Publish a file or folder as a site",
		Long: `Uploads a file or folder as a new release of a site and, unless --no-activate,
makes it live. A standalone file gets its own site. The site is named by --site
or after the file or folder. Relative paths are kept; symlinks, node_modules,
OS metadata and what a .dropignore lists are left out, and anything that looks
like a credential (.env, private keys, .git) stops the publish.

If the folder has several pages that could be the entry (and no index.html),
the gateway asks for one: give it with --entry. The release address of a site
is visible to your organisation only until you share it.`,
		Example: `  zuano drop publish ./dist --site docs --title "Team docs"
  zuano drop publish notes.md
  zuano drop publish ./report --no-activate`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDropPublish(cmd, f, args[0], site, title, entry, spa, noActivate)
		},
	}
	f.bind(cmd, "Print the release and site as one JSON document")
	cmd.Flags().StringVar(&site, "site", "", "Site name (default: from the file or folder name)")
	cmd.Flags().StringVar(&title, "title", "", "Site title")
	cmd.Flags().StringVar(&entry, "entry", "", "Path of the entry page within the release")
	cmd.Flags().BoolVar(&spa, "spa", false, "Serve the entry page for paths that match no file (single-page app)")
	cmd.Flags().BoolVar(&noActivate, "no-activate", false, "Prepare the release without making it live; it can be previewed")
	return cmd
}

func runDropPublish(cmd *cobra.Command, f approvalFlags, root, site, title, entry string, spa, noActivate bool) error {
	m, err := drop.Build(root)
	if err != nil {
		return err
	}
	if site == "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		if site, err = slugFrom(filepath.Base(abs)); err != nil {
			return err
		}
	} else if !siteSlugPattern.MatchString(site) || len(site) < 3 || len(site) > 30 {
		return fmt.Errorf("invalid --site %q: use 3 to 30 lower-case letters, digits and single hyphens", textsafe.Clean(site))
	}
	if entry != "" {
		if entry, err = drop.Canonicalize(entry); err != nil {
			return fmt.Errorf("invalid --entry: %w", err)
		}
	}
	c, err := approvalsClient(f.gatewayURL)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if !f.jsonOut {
		for _, w := range m.Warnings {
			fmt.Fprintf(errOut, "warning: %s\n", textsafe.Clean(w))
		}
		fmt.Fprintf(errOut, "Publishing %d files (%d bytes) to %s...\n", len(m.Files), m.TotalBytes(), textsafe.Clean(site))
	}

	siteSpec := map[string]any{"slug": site}
	if title != "" {
		siteSpec["title"] = title
	}
	if spa {
		siteSpec["routing"] = "spa"
	}
	files := make([]map[string]any, len(m.Files))
	for i, mf := range m.Files {
		files[i] = map[string]any{"path": mf.Path, "size": mf.Size, "sha256": mf.SHA256}
	}
	body := map[string]any{"site": siteSpec, "files": files, "idempotency_key": client.NewCorrelationID()}
	if entry != "" {
		body["entry"] = entry
	}
	var created struct {
		UploadID string       `json:"upload_id"`
		Files    []uploadSlot `json:"files"`
	}
	if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/artifacts/uploads", body, &created); err != nil {
		return err
	}
	if created.UploadID == "" {
		return errors.New("the gateway did not return an upload")
	}
	if err := uploadSlots(cmd.Context(), m, created.Files, errOut, f.jsonOut); err != nil {
		return err
	}

	var fin struct {
		ReleaseID string `json:"release_id"`
	}
	if _, err := gatewayWrite(cmd, c, f.jsonOut, http.MethodPost, "/api/artifacts/uploads/"+url.PathEscape(created.UploadID)+"/finalize", map[string]any{"activate": !noActivate}, &fin); err != nil {
		return err
	}
	rel, siteDoc, raw, err := waitForRelease(cmd.Context(), c, fin.ReleaseID)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printRaw(out, raw)
	}
	if rel.State != "ready" {
		return releaseFailureError(rel)
	}
	if !f.jsonOut {
		printPublished(out, rel, siteDoc, noActivate)
	}
	return nil
}

// uploadSlots sends the files the gateway does not already hold, a few at a
// time, retrying a file once when the store cannot be reached.
func uploadSlots(ctx context.Context, m drop.Manifest, slots []uploadSlot, status io.Writer, quiet bool) error {
	byPath := make(map[string]drop.File, len(m.Files))
	for _, f := range m.Files {
		byPath[f.Path] = f
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sent     int
	)
	type job struct {
		slot   uploadSlot
		file   drop.File
		target *url.URL
	}
	var jobs []job
	for _, s := range slots {
		if s.Upload == nil {
			continue
		}
		file, ok := byPath[s.Path]
		if !ok {
			return fmt.Errorf("the gateway asked for %s, which is not in the release", textsafe.Clean(s.Path))
		}
		target, err := presignedURL(s.Upload.URL)
		if err != nil {
			return err
		}
		jobs = append(jobs, job{s, file, target})
	}
	sem := make(chan struct{}, dropUploadWorkers)
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if ctx.Err() != nil {
				return
			}
			if err := putFile(ctx, j.target, *j.slot.Upload, j.file); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			sent++
			if !quiet && isInteractiveTerminal(os.Stderr) {
				fmt.Fprintf(status, "\r  uploaded %d files", sent)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if sent > 0 && !quiet && isInteractiveTerminal(os.Stderr) {
		fmt.Fprintln(status)
	}
	return firstErr
}

func putFile(ctx context.Context, target *url.URL, t uploadTarget, f drop.File) error {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Path)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var fh *os.File
		if fh, err = os.Open(f.Abs); err != nil {
			return err
		}
		err = putObject(ctx, target, t, fh, f.Size, contentType, nil, f.Path)
		_ = fh.Close()
		var netErr *networkError
		if !errors.As(err, &netErr) {
			return err
		}
	}
	return err
}

// waitForRelease polls the release the gateway is preparing.
func waitForRelease(ctx context.Context, c *client.Client, id string) (releaseDoc, siteDoc, []byte, error) {
	if id == "" {
		return releaseDoc{}, siteDoc{}, nil, errors.New("the gateway did not return a release")
	}
	deadline := time.Now().Add(dropPollTimeout)
	for {
		var doc struct {
			Release releaseDoc `json:"release"`
			Site    siteDoc    `json:"site"`
		}
		raw, err := gatewayJSON(ctx, c, http.MethodGet, "/api/artifacts/releases/"+url.PathEscape(id), nil, &doc)
		if err != nil {
			return releaseDoc{}, siteDoc{}, nil, err
		}
		if doc.Release.State == "ready" || doc.Release.State == "failed" || doc.Release.State == "deleted" {
			return doc.Release, doc.Site, raw, nil
		}
		if time.Now().After(deadline) {
			return releaseDoc{}, siteDoc{}, nil, fmt.Errorf("release %s is still being prepared after %s; check it with `zuano drop releases`", textsafe.Clean(id), dropPollTimeout)
		}
		select {
		case <-ctx.Done():
			return releaseDoc{}, siteDoc{}, nil, ctx.Err()
		case <-time.After(dropPollInterval):
		}
	}
}

func releaseFailureError(rel releaseDoc) error {
	fail := rel.Failure
	if fail == nil {
		return fmt.Errorf("release %s failed (state %s)", textsafe.Clean(rel.ID), textsafe.Clean(rel.State))
	}
	var b strings.Builder
	switch fail.Code {
	case "entry_required":
		fmt.Fprintf(&b, "several pages could be the entry: %s. Choose one with --entry", textsafe.Clean(strings.Join(fail.Candidates, ", ")))
	default:
		b.WriteString(textsafe.Clean(firstNonEmptyString(fail.Message, "the release could not be prepared")))
		if fail.Code != "" {
			fmt.Fprintf(&b, " (%s)", textsafe.Clean(fail.Code))
		}
	}
	for _, f := range fail.Files {
		fmt.Fprintf(&b, "\n  %s: %s", textsafe.Clean(f.Path), textsafe.Clean(f.Error))
	}
	return errors.New(b.String())
}

func printPublished(out io.Writer, rel releaseDoc, s siteDoc, noActivate bool) {
	for _, w := range rel.Warnings {
		line := textsafe.Clean(firstNonEmptyString(w.Message, w.Kind))
		if w.Path != "" {
			line = textsafe.Clean(w.Path) + ": " + line
			if w.Line > 0 {
				line = fmt.Sprintf("%s (line %d)", line, w.Line)
			}
		}
		fmt.Fprintf(out, "warning: %s\n", line)
	}
	if noActivate {
		fmt.Fprintf(out, "Release %s is ready but not live.\nPreview: %s\n", textsafe.Clean(rel.ID), textsafe.Clean(rel.PreviewURL))
		return
	}
	fmt.Fprintf(out, "Published %s (%s, %d files)\n%s\n", textsafe.Clean(firstNonEmptyString(s.Slug, rel.SiteID)), textsafe.Clean(rel.ID), rel.FileCount, textsafe.Clean(s.URL))
	if s.Audience != "" {
		fmt.Fprintf(out, "Visible to: %s\n", textsafe.Clean(s.Audience))
	}
}
