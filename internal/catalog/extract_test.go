package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// tarEntry describes one entry to write into a test tarball — deliberately
// lower-level than a plain map[string][]byte so malicious-shape tests can
// set Typeflag/Linkname/Name directly (absolute paths, "..", symlinks).
type tarEntry struct {
	name     string
	typeflag byte
	linkname string
	body     []byte
}

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: typeflag,
			Linkname: e.linkname,
			Mode:     0o644,
			Size:     int64(len(e.body)),
		}
		if typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("write body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func simpleBundle(t *testing.T) []byte {
	t.Helper()
	return buildTarGz(t, []tarEntry{
		{name: "SKILL.md", body: []byte("---\nname: pr-triage\ndescription: triage PRs\n---\n\nbody\n")},
		{name: "scripts/", typeflag: tar.TypeDir},
		{name: "scripts/run.sh", body: []byte("#!/bin/sh\necho hi\n")},
	})
}

func TestExtractTarGz_BenignBundleExtractsClean(t *testing.T) {
	dest := t.TempDir()
	if err := extractTarGz(simpleBundle(t), dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Fatalf("expected SKILL.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil {
		t.Fatalf("expected scripts/run.sh: %v", err)
	}
}

func TestExtractTarGz_RejectsDotDotEscape(t *testing.T) {
	dest := t.TempDir()
	bad := buildTarGz(t, []tarEntry{
		{name: "../escape.txt", body: []byte("pwned")},
	})
	if err := extractTarGz(bad, dest); err == nil {
		t.Fatalf("expected an error for a '..'-escaping entry")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected nothing written outside dest, stat err = %v", err)
	}
}

func TestExtractTarGz_RejectsAbsolutePath(t *testing.T) {
	dest := t.TempDir()
	bad := buildTarGz(t, []tarEntry{
		{name: "/etc/passwd", body: []byte("pwned")},
	})
	if err := extractTarGz(bad, dest); err == nil {
		t.Fatalf("expected an error for an absolute-path entry")
	}
}

func TestExtractTarGz_RejectsNestedDotDot(t *testing.T) {
	dest := t.TempDir()
	bad := buildTarGz(t, []tarEntry{
		{name: "scripts/../../escape.txt", body: []byte("pwned")},
	})
	if err := extractTarGz(bad, dest); err == nil {
		t.Fatalf("expected an error for a nested '..'-escaping entry")
	}
}

func TestExtractTarGz_SkipsSymlinkEntry(t *testing.T) {
	dest := t.TempDir()
	bundle := buildTarGz(t, []tarEntry{
		{name: "SKILL.md", body: []byte("---\nname: x\ndescription: d\n---\n\nbody\n")},
		{name: "scripts/evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	if err := extractTarGz(bundle, dest); err != nil {
		t.Fatalf("extractTarGz should skip (not error on) a symlink entry: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "scripts", "evil-link")); !os.IsNotExist(err) {
		t.Fatalf("expected the symlink entry to NOT be materialized, lstat err = %v", err)
	}
}

func TestExtractTarGz_SkipsHardlinkEntry(t *testing.T) {
	dest := t.TempDir()
	bundle := buildTarGz(t, []tarEntry{
		{name: "SKILL.md", body: []byte("---\nname: x\ndescription: d\n---\n\nbody\n")},
		{name: "scripts/evil-hardlink", typeflag: tar.TypeLink, linkname: "SKILL.md"},
	})
	if err := extractTarGz(bundle, dest); err != nil {
		t.Fatalf("extractTarGz should skip (not error on) a hardlink entry: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "scripts", "evil-hardlink")); !os.IsNotExist(err) {
		t.Fatalf("expected the hardlink entry to NOT be materialized, lstat err = %v", err)
	}
}

func TestExtractTarGz_RejectsEntryCountBomb(t *testing.T) {
	dest := t.TempDir()
	var entries []tarEntry
	for i := 0; i < MaxBundleEntries+10; i++ {
		entries = append(entries, tarEntry{name: filepath.Join("f", string(rune('a'+i%26)), "x.txt"), body: []byte("x")})
	}
	bomb := buildTarGz(t, entries)
	if err := extractTarGz(bomb, dest); err == nil {
		t.Fatalf("expected an error for a bundle exceeding MaxBundleEntries")
	}
}

func TestExtractTarGz_RejectsSizeBomb(t *testing.T) {
	dest := t.TempDir()
	big := bytes.Repeat([]byte("a"), 1<<20) // 1 MiB per entry
	var entries []tarEntry
	// enough entries * 1 MiB to exceed MaxBundleBytes (50 MiB), staying well
	// under MaxBundleEntries so the entry-count cap doesn't trigger first.
	for i := 0; i < 60; i++ {
		entries = append(entries, tarEntry{name: filepathJoinf("big", i), body: big})
	}
	bomb := buildTarGz(t, entries)
	if err := extractTarGz(bomb, dest); err == nil {
		t.Fatalf("expected an error for a bundle exceeding MaxBundleBytes")
	}
}

func filepathJoinf(prefix string, i int) string {
	return prefix + "/" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".bin"
}

func TestExtractTarGz_EmptyBundleErrors(t *testing.T) {
	dest := t.TempDir()
	empty := buildTarGz(t, nil)
	if err := extractTarGz(empty, dest); err == nil {
		t.Fatalf("expected an error for an empty bundle")
	}
}

func TestExtractTarGz_MalformedGzipErrors(t *testing.T) {
	dest := t.TempDir()
	if err := extractTarGz([]byte("not a gzip stream"), dest); err == nil {
		t.Fatalf("expected an error for malformed gzip data")
	}
}

func TestExtractTarGz_RegularFileOverExistingDirErrors(t *testing.T) {
	dest := t.TempDir()
	bundle := buildTarGz(t, []tarEntry{
		{name: "conflict", typeflag: tar.TypeDir},
		{name: "conflict", typeflag: tar.TypeReg, body: []byte("x")},
	})
	if err := extractTarGz(bundle, dest); err == nil {
		t.Fatalf("expected an error writing a regular file over an existing directory path")
	}
}

// buildTruncatedTarGz writes a single tar entry whose HEADER declares
// declaredSize bytes but whose ACTUAL body is shorter (len(actualBody) <
// declaredSize) — simulating a truncated/malformed archive (m1 regression).
// tar.Writer only detects a short write at the NEXT WriteHeader/Close call,
// so by deliberately never calling either, the raw buffer we gzip contains
// exactly: a complete, valid header block + a short, unpadded body — which
// is what a tar.Reader sees when the underlying stream was cut short.
func buildTruncatedTarGz(t *testing.T, name string, declaredSize int64, actualBody []byte) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	hdr := &tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     declaredSize,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header %s: %v", name, err)
	}
	if _, err := tw.Write(actualBody); err != nil {
		t.Fatalf("write truncated body %s: %v", name, err)
	}
	// Deliberately do NOT call tw.Close() — that would pad/verify the
	// entry and defeat the truncation this test needs.

	var gz bytes.Buffer
	gzw := gzip.NewWriter(&gz)
	if _, err := gzw.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("gzip truncated tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return gz.Bytes()
}

func TestExtractTarGz_RejectsTruncatedEntry(t *testing.T) {
	dest := t.TempDir()
	bad := buildTruncatedTarGz(t, "SKILL.md", 100, []byte("short-body"))
	if err := extractTarGz(bad, dest); err == nil {
		t.Fatalf("expected an error for a tar entry shorter than its declared header size")
	}
	// Note: extractTarGz's caller (FetchSkill) always extracts into a
	// throwaway temp dir it removes on every return path (see
	// gateway_source.go's doc comment), so a partial file left behind by an
	// aborted writeRegularFile is never actually exposed to a caller — same
	// posture as TestExtractTarGz_RegularFileOverExistingDirErrors above.
}

func TestSafeJoin_RootEntrySkippedNotError(t *testing.T) {
	_, ok, err := safeJoin("/tmp/dest", ".")
	if err != nil {
		t.Fatalf("unexpected error for root entry: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for a root '.' entry")
	}
}
