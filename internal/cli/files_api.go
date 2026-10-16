package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/patrikmichi/zuano/internal/client"
	"github.com/patrikmichi/zuano/internal/config"
	"github.com/patrikmichi/zuano/internal/textsafe"
)

// fileDoc mirrors a file in the gateway's file store.
type fileDoc struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContentType   string `json:"content_type"`
	SizeBytes     int64  `json:"size_bytes"`
	SHA256        string `json:"sha256"`
	State         string `json:"state"`
	CreatedVia    string `json:"created_via"`
	SharedWithOrg bool   `json:"shared_with_org"`
	ExpiresAt     string `json:"expires_at"`
	CreatedAt     string `json:"created_at"`
	Scan          struct {
		Verdict string `json:"verdict"`
	} `json:"scan"`
}

// Timings are variables so tests can shorten them.
var (
	filePollInterval = time.Second
	filePollTimeout  = 2 * time.Minute
)

var fileIDPattern = regexp.MustCompile(`^fil_[A-Za-z0-9_-]{6,64}$`)

// parseFileRef reads a file id from a bare id, a file:<id> reference or the
// URI of a resource link (.../api/files/<id>).
func parseFileRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if rest, ok := strings.CutPrefix(ref, "file:"); ok {
		ref = rest
	}
	if strings.Contains(ref, "://") {
		u, err := url.Parse(ref)
		if err != nil {
			return "", fmt.Errorf("%q is not a file id", textsafe.Clean(ref))
		}
		ref = filepath.Base(strings.TrimSuffix(u.Path, "/"))
		if ref == "download" {
			ref = filepath.Base(filepath.Dir(u.Path))
		}
	}
	if !fileIDPattern.MatchString(ref) {
		return "", fmt.Errorf("%q is not a file id (expected fil_...)", textsafe.Clean(ref))
	}
	return ref, nil
}

func fileGet(ctx context.Context, c *client.Client, id string) (fileDoc, error) {
	var doc struct {
		File fileDoc `json:"file"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodGet, "/api/files/"+url.PathEscape(id), nil, &doc); err != nil {
		return fileDoc{}, err
	}
	return doc.File, nil
}

func fileList(ctx context.Context, c *client.Client, state, createdVia string, max int) ([]fileDoc, error) {
	var all []fileDoc
	cursor := ""
	for len(all) < max {
		q := url.Values{}
		for k, v := range map[string]string{"state": state, "created_via": createdVia, "cursor": cursor} {
			if v != "" {
				q.Set(k, v)
			}
		}
		path := "/api/files"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		var page struct {
			Files      []fileDoc `json:"files"`
			NextCursor *string   `json:"next_cursor"`
		}
		if _, err := gatewayJSON(ctx, c, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Files...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
	}
	return all, nil
}

// uploadOptions says how a local file goes into the store.
type uploadOptions struct {
	name     string
	shareOrg bool
	// wait holds the call until the gateway has scanned the file and it is
	// ready to use.
	wait     bool
	progress io.Writer
}

// presignedURL accepts a storage URL only over https, or over plain http to a
// loopback host, which is how tests and local gateways serve it.
func presignedURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, errors.New("the gateway sent an upload or download address that is not a web address")
	}
	if u.Scheme == "https" || (u.Scheme == "http" && config.IsLoopbackOrigin(u.Scheme+"://"+u.Host)) {
		return u, nil
	}
	return nil, fmt.Errorf("the gateway sent a storage address that is not https (%s); refusing to use it", textsafe.Clean(u.Scheme+"://"+u.Host))
}

// storageClient talks to the object store. It carries no credentials and
// follows no redirects: a signed address is the whole authorization.
var storageClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type uploadTarget struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt string            `json:"expires_at"`
}

// uploadFile sends one local file to the store: it hashes the file, asks the
// gateway for an upload target, PUTs the bytes straight to the store, tells the
// gateway the upload is complete and, with o.wait, waits until the file is
// ready.
func uploadFile(ctx context.Context, c *client.Client, path string, o uploadOptions) (fileDoc, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileDoc{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileDoc{}, err
	}
	if !info.Mode().IsRegular() {
		return fileDoc{}, fmt.Errorf("%s is not a regular file", textsafe.Clean(path))
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileDoc{}, fmt.Errorf("read %s: %w", textsafe.Clean(path), err)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	name := firstNonEmptyString(o.name, filepath.Base(path))
	contentType := detectContentType(f, path)

	var created struct {
		FileID string       `json:"file_id"`
		Upload uploadTarget `json:"upload"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodPost, "/api/files", map[string]any{
		"name": name, "content_type": contentType, "size": info.Size(), "sha256": digest, "share_with_org": o.shareOrg,
	}, &created); err != nil {
		return fileDoc{}, err
	}
	if !fileIDPattern.MatchString(created.FileID) {
		return fileDoc{}, errors.New("the gateway did not return a file id for the upload")
	}
	target, err := presignedURL(created.Upload.URL)
	if err != nil {
		return fileDoc{}, err
	}

	var putErr error
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fileDoc{}, err
		}
		putErr = putObject(ctx, target, created.Upload, f, info.Size(), contentType, o.progress, name)
		var netErr *networkError
		if !errors.As(putErr, &netErr) {
			break
		}
	}
	if putErr != nil {
		return fileDoc{}, putErr
	}

	var done struct {
		File fileDoc `json:"file"`
	}
	if _, err := gatewayJSON(ctx, c, http.MethodPost, "/api/files/"+url.PathEscape(created.FileID)+"/complete", map[string]any{}, &done); err != nil {
		return fileDoc{}, err
	}
	if done.File.ID == "" {
		done.File.ID = created.FileID
	}
	if !o.wait {
		return done.File, nil
	}
	return waitForFile(ctx, c, done.File)
}

// networkError marks a failure to reach the store, which is worth one retry;
// a refusal by the store is not.
type networkError struct{ err error }

func (e *networkError) Error() string { return e.err.Error() }
func (e *networkError) Unwrap() error { return e.err }

func putObject(ctx context.Context, target *url.URL, t uploadTarget, body io.Reader, size int64, contentType string, progress io.Writer, name string) error {
	method := firstNonEmptyString(t.Method, http.MethodPut)
	if method != http.MethodPut && method != http.MethodPost {
		return fmt.Errorf("the gateway asked for an unsupported upload method %q", textsafe.Clean(method))
	}
	reqCtx, cancel, err := requestContext(ctx, transferRequest)
	if err != nil {
		return err
	}
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, target.String(), &countingReader{r: body, total: size, out: progress, name: name})
	if err != nil {
		return err
	}
	req.ContentLength = size
	for k, v := range t.Headers {
		switch strings.ToLower(k) {
		case "host", "content-length", "authorization", "cookie":
			continue
		}
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := storageClient.Do(req)
	if err != nil {
		return &networkError{timeoutCause(reqCtx, fmt.Errorf("upload %s: %w", textsafe.Clean(name), err))}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("the object store refused the upload of %s (HTTP %d %s)", textsafe.Clean(name), resp.StatusCode, textsafe.Clean(strings.TrimSpace(string(excerpt))))
	}
	return nil
}

// countingReader reports upload progress when out is a terminal, one updating
// line, and stays silent otherwise.
type countingReader struct {
	r     io.Reader
	total int64
	read  int64
	last  time.Time
	out   io.Writer
	name  string
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.out != nil && (time.Since(c.last) > 250*time.Millisecond || err == io.EOF) {
		c.last = time.Now()
		pct := 100
		if c.total > 0 {
			pct = int(c.read * 100 / c.total)
		}
		fmt.Fprintf(c.out, "\r%s  %d%% (%d of %d bytes)", textsafe.Clean(c.name), pct, c.read, c.total)
		if err == io.EOF {
			fmt.Fprintln(c.out)
		}
	}
	return n, err
}

// waitForFile polls until the gateway has finished with the file.
func waitForFile(ctx context.Context, c *client.Client, f fileDoc) (fileDoc, error) {
	deadline := time.Now().Add(filePollTimeout)
	for {
		switch f.State {
		case "ready":
			return f, nil
		case "quarantined":
			return f, fmt.Errorf("the gateway quarantined %s (scan verdict %s); it cannot be used", textsafe.Clean(f.ID), orDash(textsafe.Clean(f.Scan.Verdict)))
		case "failed", "deleted":
			return f, fmt.Errorf("the gateway could not accept %s (state %s)", textsafe.Clean(f.ID), textsafe.Clean(f.State))
		}
		if time.Now().After(deadline) {
			return f, fmt.Errorf("%s is still %s after %s; check it later with `zuano files list`", textsafe.Clean(f.ID), orDash(textsafe.Clean(f.State)), filePollTimeout)
		}
		select {
		case <-ctx.Done():
			return f, ctx.Err()
		case <-time.After(filePollInterval):
		}
		next, err := fileGet(ctx, c, f.ID)
		if err != nil {
			return f, err
		}
		f = next
	}
}

// detectContentType prefers the extension and falls back to sniffing the
// start of the file.
func detectContentType(f *os.File, path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		return t
	}
	head := make([]byte, 512)
	n, _ := f.ReadAt(head, 0)
	return http.DetectContentType(head[:n])
}

// downloadFile fetches a stored file to dest (a path, a directory, or "" for
// the file's own name in the current directory) and checks it against the
// size and sha256 the gateway recorded. It returns the path written.
func downloadFile(ctx context.Context, c *client.Client, id, dest string, force bool) (fileDoc, string, error) {
	meta, err := fileGet(ctx, c, id)
	if err != nil {
		return fileDoc{}, "", err
	}
	meta.ID = id
	target, err := downloadTarget(dest, meta)
	if err != nil {
		return meta, "", err
	}
	if _, err := os.Lstat(target); err == nil && !force {
		return meta, "", fmt.Errorf("%s already exists; use --force to replace it", textsafe.Clean(target))
	}

	reqCtx, cancel, err := requestContext(ctx, transferRequest)
	if err != nil {
		return meta, "", err
	}
	defer cancel()
	resp, location, err := c.GetRedirect(reqCtx, "/api/files/"+url.PathEscape(id)+"/download")
	if err != nil {
		return meta, "", timeoutCause(reqCtx, err)
	}
	if location != "" {
		u, err := presignedURL(location)
		if err != nil {
			return meta, "", err
		}
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
		if err != nil {
			return meta, "", err
		}
		resp, err = storageClient.Do(req)
		if err != nil {
			return meta, "", timeoutCause(reqCtx, fmt.Errorf("download %s: %w", textsafe.Clean(id), err))
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := readLimitedResponse(resp.Body, maxApprovalBody)
		return meta, "", gatewayStatusError(resp.StatusCode, raw)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".zuano-download-*")
	if err != nil {
		return meta, "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return meta, "", timeoutCause(reqCtx, fmt.Errorf("download %s: %w", textsafe.Clean(id), err))
	}
	if meta.SizeBytes > 0 && n != meta.SizeBytes {
		return meta, "", fmt.Errorf("downloaded %d bytes but the gateway recorded %d for %s", n, meta.SizeBytes, textsafe.Clean(id))
	}
	if want := strings.ToLower(meta.SHA256); want != "" && hex.EncodeToString(h.Sum(nil)) != want {
		return meta, "", fmt.Errorf("sha256 of the download does not match the one the gateway recorded for %s; nothing was saved", textsafe.Clean(id))
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return meta, "", err
	}
	return meta, target, nil
}

// downloadTarget decides where a download goes. A name the gateway supplies
// is cut to its last element, so it can never point outside the directory.
func downloadTarget(dest string, meta fileDoc) (string, error) {
	if dest != "" {
		if info, err := os.Stat(dest); err != nil || !info.IsDir() {
			return dest, nil
		}
	}
	name := filepath.Base(strings.ReplaceAll(meta.Name, "\\", "/"))
	if name == "." || name == ".." || name == "/" || name == "" {
		name = meta.ID
	}
	return filepath.Join(dest, name), nil
}
