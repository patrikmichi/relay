package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serveBundle(t *testing.T, bundle []byte, agent bool) Doer {
	t.Helper()
	return newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if agent {
			w.Header().Set("X-Resource-Type", "agent")
			w.Header().Set(headerResourceContentSha256, sha256HexOf(bundle))
		}
		w.Header().Set(headerContentSha256, sha256HexOf(bundle))
		w.Header().Set(headerScanVerdict, "approved")
		w.Header().Set(headerCatalogID, "res_abc123")
		w.Header().Set(headerVersion, "1.0.0")
		_, _ = w.Write(bundle)
	})
}

func TestFetchAgent_RootOnlyBundleRejected(t *testing.T) {
	bundle := buildTarGz(t, []tarEntry{{name: "./", typeflag: tar.TypeDir}})
	agent, err := FetchAgent(serveBundle(t, bundle, true), "res_abc123", "", "")
	if err == nil || agent != nil || !strings.Contains(err.Error(), "no root-level .md file") {
		t.Fatalf("got agent=%v err=%v, want a missing-definition error", agent, err)
	}
}

func TestFetchSkill_BundleWithoutSkillFileRejected(t *testing.T) {
	bundle := buildTarGz(t, []tarEntry{{name: "README.txt", body: []byte("no skill here")}})
	skill, err := FetchSkill(serveBundle(t, bundle, false), "res_abc123", "", "")
	if err == nil || skill != nil || !strings.Contains(err.Error(), "load extracted bundle") {
		t.Fatalf("got skill=%v err=%v, want a load error", skill, err)
	}
}

func TestFetch_UnusableTempDirAborts(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	if _, err := FetchSkill(serveBundle(t, simpleBundle(t), false), "res_abc123", "", ""); err == nil || !strings.Contains(err.Error(), "create temp extraction dir") {
		t.Errorf("FetchSkill: got %v, want a temp dir error", err)
	}
	if _, err := FetchAgent(serveBundle(t, simpleAgentBundle(t, "reviewer"), true), "res_abc123", "", ""); err == nil || !strings.Contains(err.Error(), "create temp extraction dir") {
		t.Errorf("FetchAgent: got %v, want a temp dir error", err)
	}
}

func TestExtractTarGz_CorruptTarInsideValidGzip(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	_, _ = gzw.Write(bytes.Repeat([]byte{0xff}, 1024))
	_ = gzw.Close()

	err := extractTarGz(buf.Bytes(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read tar entry") {
		t.Fatalf("got %v, want a tar read error", err)
	}
}

func TestExtractTarGz_SkipsSpecialFiles(t *testing.T) {
	dest := t.TempDir()
	bundle := buildTarGz(t, []tarEntry{
		{name: "pipe", typeflag: tar.TypeFifo},
		{name: "SKILL.md", body: []byte("body")},
	})
	if err := extractTarGz(bundle, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "pipe")); !os.IsNotExist(err) {
		t.Errorf("FIFO entry materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Errorf("regular file missing: %v", err)
	}
}

func TestExtractTarGz_ParentPathIsAFile(t *testing.T) {
	for name, entries := range map[string][]tarEntry{
		"file under file":      {{name: "a", body: []byte("x")}, {name: "a/b", body: []byte("y")}},
		"directory over file":  {{name: "a", body: []byte("x")}, {name: "a/", typeflag: tar.TypeDir}},
		"nested dir over file": {{name: "a", body: []byte("x")}, {name: "a/b/", typeflag: tar.TypeDir}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := extractTarGz(buildTarGz(t, entries), t.TempDir()); err == nil || !strings.Contains(err.Error(), "create directory") {
				t.Fatalf("got %v, want a create-directory error", err)
			}
		})
	}
}
