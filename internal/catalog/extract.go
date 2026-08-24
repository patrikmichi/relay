package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractTarGz decompresses and untars data into destDir, enforcing the
// same path-safety posture as agentport's frontmatter.go loadResources (skip
// symlinks/non-regular files rather than materializing them) plus tarball-
// specific hardening a directory walk doesn't need:
//
//   - absolute-path entries are refused outright;
//   - entries containing ".." (or that Clean to something outside destDir)
//     are refused — a "zip-slip"-style escape;
//   - symlink and hardlink entries are SKIPPED, never materialized — the
//     same reasoning as loadResources: a malicious bundle could symlink a
//     resource path to an arbitrary local file and have Project()/Write()
//     copy that target's content into every provider's directory;
//   - device files, FIFOs, and any other non-regular/non-directory entry
//     are SKIPPED;
//   - the tarball is capped at MaxBundleEntries entries and MaxBundleBytes
//     of total decompressed content (a gzip-bomb / entry-flood guard) — the
//     compressed wire size is already capped by readLimited before this
//     function ever runs.
//
// destDir must already exist. On any rejected entry, extraction stops
// immediately with an error; the caller (FetchSkill) always extracts into a
// throwaway temp dir it removes on every return path, so a partial
// extraction is never left behind for a caller to accidentally consume.
func extractTarGz(data []byte, destDir string) error {
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve extraction root: %w", err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	var entryCount int
	var totalBytes int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		entryCount++
		if entryCount > MaxBundleEntries {
			return fmt.Errorf("bundle exceeds entry cap (%d entries) — refusing to extract", MaxBundleEntries)
		}

		destPath, ok, err := safeJoin(absDest, hdr.Name)
		if err != nil {
			return err
		}
		if !ok {
			// "." or "./" — the tarball's own root directory entry; nothing
			// to create.
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destPath, 0o755); err != nil {
				return fmt.Errorf("create directory %s: %w", destPath, err)
			}

		case tar.TypeReg:
			if hdr.Size < 0 {
				return fmt.Errorf("tar entry %q declares a negative size", hdr.Name)
			}
			totalBytes += hdr.Size
			if totalBytes > MaxBundleBytes {
				return fmt.Errorf("bundle exceeds size cap (%d bytes) — refusing to extract", MaxBundleBytes)
			}
			if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
				return fmt.Errorf("create directory for %s: %w", destPath, err)
			}
			if err := writeRegularFile(destPath, tr, hdr.Size); err != nil {
				return err
			}

		case tar.TypeSymlink, tar.TypeLink:
			// Never materialize a symlink/hardlink from an untrusted
			// tarball — mirrors agentport/frontmatter.go's loadResources
			// posture on the read side.
			continue

		default:
			// Device files, FIFOs, sockets, etc. — skip rather than read
			// (some of these can block a reader forever) or write.
			continue
		}
	}

	if entryCount == 0 {
		return fmt.Errorf("bundle contains no entries")
	}
	return nil
}

// writeRegularFile copies exactly size bytes from r into a newly created
// file at destPath.
//
// io.CopyN returns io.EOF when r runs dry BEFORE delivering size bytes — the
// signature of a truncated/malformed tar entry (the header declared more
// data than the archive actually contains). A well-formed archive never
// hits this: r has exactly size bytes of this entry's data available before
// the next header/EOF. So ANY error from io.CopyN, io.EOF included, is
// treated as fatal here — never silently accepted as "close enough".
func writeRegularFile(destPath string, r io.Reader, size int64) error {
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	if _, err := io.CopyN(out, r, size); err != nil {
		_ = out.Close()
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", destPath, err)
	}
	return nil
}

// safeJoin validates a tar entry name and, if safe, joins it onto root.
// Returns ok=false (no error) for a "." root-directory entry, which the
// caller should simply skip. Returns an error for any entry that is an
// absolute path, escapes root via "..", or otherwise resolves outside root.
func safeJoin(root, name string) (path string, ok bool, err error) {
	if filepath.IsAbs(name) {
		return "", false, fmt.Errorf("tar entry %q is an absolute path — refusing to extract", name)
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." {
		return "", false, nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("tar entry %q escapes the extraction root — refusing to extract", name)
	}

	joined := filepath.Join(root, cleaned)
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", false, fmt.Errorf("tar entry %q resolves outside the extraction root — refusing to extract", name)
	}
	return joined, true, nil
}
