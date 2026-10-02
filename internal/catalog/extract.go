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

// writeRegularFile copies exactly size bytes from r into a newly created file at destPath.
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
