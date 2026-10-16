package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// UpdateKeyHex1 and UpdateKeyHex2 are the public keys, in hex, that update
// manifests are signed with; they are set at build time. Two allow the key to
// be rotated. With neither set the connector never updates itself.
var (
	UpdateKeyHex1 string
	UpdateKeyHex2 string
)

// UpdateKeys parses the compiled-in keys.
func UpdateKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, h := range []string{UpdateKeyHex1, UpdateKeyHex2} {
		if b, err := hex.DecodeString(h); err == nil && len(b) == ed25519.PublicKeySize {
			out = append(out, ed25519.PublicKey(b))
		}
	}
	return out
}

// UpdateArtifact is one downloadable binary.
type UpdateArtifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// UpdateManifest lists a release.
type UpdateManifest struct {
	Version     string           `json:"version"`
	PublishedAt time.Time        `json:"published_at"`
	MinProto    int              `json:"min_proto"`
	Artifacts   []UpdateArtifact `json:"artifacts"`
}

// Update errors.
var (
	ErrUpdateSignature = errors.New("the update manifest is not signed by a key this connector trusts")
	ErrNoUpdateKeys    = errors.New("this build has no update key, so it cannot verify an update")
)

// ParseVersion reads major.minor.patch, with an optional leading v and any
// pre-release or build suffix ignored.
func ParseVersion(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	var v [3]int
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// CompareVersions returns -1, 0 or 1; ok is false when either is not a version.
func CompareVersions(a, b string) (cmp int, ok bool) {
	va, oka := ParseVersion(a)
	vb, okb := ParseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range va {
		switch {
		case va[i] < vb[i]:
			return -1, true
		case va[i] > vb[i]:
			return 1, true
		}
	}
	return 0, true
}

// BumpClass says how far apart two versions are: major, minor or patch.
func BumpClass(from, to string) string {
	a, _ := ParseVersion(from)
	b, _ := ParseVersion(to)
	switch {
	case a[0] != b[0]:
		return "major"
	case a[1] != b[1]:
		return "minor"
	}
	return "patch"
}

// ManifestFromDist builds the manifest of a release from the bare binaries in
// dir, named <binary>_<os>_<arch>[.exe]; baseURL is where they are published.
func ManifestFromDist(dir, binary, version, baseURL string, publishedAt time.Time) (UpdateManifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return UpdateManifest{}, err
	}
	m := UpdateManifest{Version: strings.TrimPrefix(version, "v"), PublishedAt: publishedAt.UTC(), MinProto: ProtoVersion}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".exe")
		rest, ok := strings.CutPrefix(name, binary+"_")
		if !ok || e.IsDir() {
			continue
		}
		goos, goarch, ok := strings.Cut(rest, "_")
		if !ok || goos == "" || goarch == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return UpdateManifest{}, err
		}
		sum := sha256.Sum256(raw)
		m.Artifacts = append(m.Artifacts, UpdateArtifact{
			OS: goos, Arch: goarch, URL: strings.TrimRight(baseURL, "/") + "/" + e.Name(),
			SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw)),
		})
	}
	if len(m.Artifacts) == 0 {
		return UpdateManifest{}, fmt.Errorf("no %s_<os>_<arch> binaries in %s", binary, dir)
	}
	sort.Slice(m.Artifacts, func(i, j int) bool {
		if m.Artifacts[i].OS != m.Artifacts[j].OS {
			return m.Artifacts[i].OS < m.Artifacts[j].OS
		}
		return m.Artifacts[i].Arch < m.Artifacts[j].Arch
	})
	return m, nil
}

// SignManifest returns the manifest's JSON and its detached signature in
// base64, the two files a release publishes.
func SignManifest(m UpdateManifest, key ed25519.PrivateKey) (manifest, signature []byte, err error) {
	manifest, err = json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	sig := ed25519.Sign(key, manifest)
	return manifest, []byte(base64.StdEncoding.EncodeToString(sig)), nil
}

// VerifyManifest checks the manifest bytes against its detached signature.
func VerifyManifest(raw, sig []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return ErrNoUpdateKeys
	}
	if s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig))); err == nil && len(s) == ed25519.SignatureSize {
		sig = s
	}
	for _, k := range keys {
		if ed25519.Verify(k, raw, sig) {
			return nil
		}
	}
	return ErrUpdateSignature
}

func fetch(ctx context.Context, c *http.Client, rawURL string, max int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !secureURL(u) {
		return nil, fmt.Errorf("%q is not an https address", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", u.Host, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// UpdateInfo is a newer release that applies to this machine.
type UpdateInfo struct {
	Manifest UpdateManifest
	Artifact UpdateArtifact
	Class    string
}

// CheckUpdate fetches and verifies the channel's manifest and returns the
// release when it is newer than current (or any different one with
// allowDowngrade) and has an artifact for this platform. It returns nil when
// there is nothing to do.
func CheckUpdate(ctx context.Context, c *http.Client, releasesURL, channel, current string, keys []ed25519.PublicKey, allowDowngrade bool) (*UpdateInfo, error) {
	base := strings.TrimRight(releasesURL, "/") + "/connector/" + url.PathEscape(channel) + "/manifest.json"
	raw, err := fetch(ctx, c, base, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("fetch the update manifest: %w", err)
	}
	sig, err := fetch(ctx, c, base+".sig", 4096)
	if err != nil {
		return nil, fmt.Errorf("fetch the manifest signature: %w", err)
	}
	if err := VerifyManifest(raw, sig, keys); err != nil {
		return nil, err
	}
	var m UpdateManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the manifest is not JSON: %w", err)
	}
	if m.MinProto > ProtoVersion {
		return nil, fmt.Errorf("release %s needs protocol %d, which this connector cannot update across", m.Version, m.MinProto)
	}
	cmp, ok := CompareVersions(m.Version, current)
	switch {
	case !ok:
		return nil, nil
	case cmp == 0 || (cmp < 0 && !allowDowngrade):
		return nil, nil
	}
	for _, a := range m.Artifacts {
		if a.OS == runtime.GOOS && a.Arch == runtime.GOARCH {
			return &UpdateInfo{Manifest: m, Artifact: a, Class: BumpClass(current, m.Version)}, nil
		}
	}
	return nil, fmt.Errorf("release %s has no build for %s/%s", m.Version, runtime.GOOS, runtime.GOARCH)
}

// Download fetches the artifact next to dest and checks its size and SHA-256
// before it can be used. The file is synced.
func Download(ctx context.Context, c *http.Client, art UpdateArtifact, dir string) (string, error) {
	u, err := url.Parse(art.URL)
	if err != nil || !secureURL(u) {
		return "", fmt.Errorf("the artifact address %q is not https", art.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, art.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %d", u.Host, resp.StatusCode)
	}
	f, err := os.CreateTemp(dir, ".update-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, art.Size+1))
	if err == nil && (n != art.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), art.SHA256)) {
		err = errors.New("the download does not match the size and SHA-256 in the signed manifest")
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// PreviousSuffix names the copy of the binary kept for a rollback.
const PreviousSuffix = ".previous"

// PendingSuffix marks a binary that was just swapped in and has not yet shown
// it can connect.
const PendingSuffix = ".pending"

// Swap puts the new binary in place of exe, keeping the old one as
// exe.previous, and marks the swap as pending.
func Swap(exe, newPath string) error {
	if err := os.Chmod(newPath, 0o755); err != nil {
		return err
	}
	prev := exe + PreviousSuffix
	_ = os.Remove(prev)
	if err := os.Rename(exe, prev); err != nil {
		return fmt.Errorf("keep the old binary: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		_ = os.Rename(prev, exe)
		return fmt.Errorf("install the new binary: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(exe)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return os.WriteFile(exe+PendingSuffix, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// Rollback restores exe.previous.
func Rollback(exe string) error {
	prev := exe + PreviousSuffix
	if _, err := os.Stat(prev); err != nil {
		return errors.New("there is no previous binary to go back to")
	}
	if err := os.Rename(prev, exe); err != nil {
		return err
	}
	_ = os.Remove(exe + PendingSuffix)
	return nil
}

// Confirm clears the pending mark once the new binary connected.
func Confirm(exe string) { _ = os.Remove(exe + PendingSuffix) }

// Pending reports a swap that has not been confirmed.
func Pending(exe string) bool {
	_, err := os.Stat(exe + PendingSuffix)
	return err == nil
}

// InContainer reports an image that is immutable: the registry or the chart
// owns updates, never the process.
func InContainer() bool {
	if os.Getenv("ZUANO_CONNECTOR_CONTAINER") != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// AutoUpdateAllowed combines the two vetoes: the local config and the
// network's setting (off, patch or minor).
func AutoUpdateAllowed(localAuto bool, networkPolicy, class string) bool {
	if !localAuto {
		return false
	}
	switch networkPolicy {
	case "patch":
		return class == "patch"
	case "minor":
		return class == "patch" || class == "minor"
	}
	return false
}
