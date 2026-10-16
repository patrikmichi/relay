package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Source is where a marketplace entry's package is fetched from.
type Source struct {
	Source string `json:"source"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Entry is one plugin of a marketplace.
type Entry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	DisplayName string `json:"displayName"`
	Source      Source `json:"source"`
}

// Marketplace is the document a feed serves for one platform and channel.
type Marketplace struct {
	Name  string `json:"name"`
	Owner struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"owner"`
	Plugins []Entry `json:"plugins"`
}

// ParseMarketplace reads a marketplace document. `owner` is an object; every
// entry must name a url source with a sha256, since an install is pinned to it.
func ParseMarketplace(raw []byte) (Marketplace, error) {
	var m Marketplace
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("the marketplace is not valid: %w", err)
	}
	if m.Name == "" {
		return m, errors.New("the marketplace has no name")
	}
	seen := map[string]bool{}
	for _, p := range m.Plugins {
		switch {
		case p.Name == "" || p.Version == "":
			return m, errors.New("a marketplace entry has no name or version")
		case seen[p.Name]:
			return m, fmt.Errorf("the marketplace lists %s twice", p.Name)
		case p.Source.Source != "url" || p.Source.URL == "":
			return m, fmt.Errorf("%s has no url source", p.Name)
		case !sha256Hex.MatchString(p.Source.SHA256):
			return m, fmt.Errorf("%s lists no sha256 for its package", p.Name)
		}
		seen[p.Name] = true
	}
	return m, nil
}

// Find returns the entry for name. A version, when given, must be the one
// listed: a feed offers one version of a plugin per channel.
func (m Marketplace) Find(name, version string) (Entry, error) {
	for _, p := range m.Plugins {
		if p.Name != name {
			continue
		}
		if version != "" && p.Version != version {
			return Entry{}, fmt.Errorf("%s is at version %s in this channel, not %s", name, p.Version, version)
		}
		return p, nil
	}
	return Entry{}, fmt.Errorf("no plugin %q in marketplace %s: %w", name, m.Name, ErrNotListed)
}

// ErrNotListed means a marketplace does not list the plugin: it was never
// published to the channel, or its version was revoked.
var ErrNotListed = errors.New("not listed")

// Ext is the archive type of the entry's package, from its URL.
func (e Entry) Ext() string {
	u, err := url.Parse(e.Source.URL)
	if err != nil {
		return ""
	}
	return path.Ext(u.Path)
}

// Fetcher downloads feed documents and packages. The bearer token is sent only
// to Origin, and redirects are not followed, so a package URL that points
// elsewhere is refused instead of receiving the token.
type Fetcher struct {
	HTTP   *http.Client
	Origin string
	Token  string
}

const maxFeedBody = 128 << 20

// Get fetches rawURL, which must be on the feed's origin.
func (f Fetcher) Get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	o, oerr := url.Parse(f.Origin)
	if err != nil || oerr != nil || u.Scheme != o.Scheme || u.Host != o.Host || u.User != nil {
		return nil, fmt.Errorf("refusing to fetch %s: it is not on %s", sanitizeURL(rawURL), f.Origin)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	hc := f.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", sanitizeURL(rawURL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode, Body: readExcerpt(resp.Body)}
	}
	if limit <= 0 {
		limit = maxFeedBody
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sanitizeURL(rawURL), err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", sanitizeURL(rawURL), limit)
	}
	return body, nil
}

// StatusError is a feed answering something other than 200.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("the feed answered HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("the feed answered HTTP %d", e.Status)
}

func readExcerpt(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 256))
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, string(b)))
}

// sanitizeURL drops the query, which may hold a signature.
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid address)"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}
