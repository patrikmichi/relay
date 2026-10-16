package drop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type vectors struct {
	Canonical []struct{ Input, Want string }   `json:"canonical"`
	Invalid   []struct{ Input, Reason string } `json:"invalid"`
	TooLong   int                              `json:"too_long_bytes"`
	Collision []struct {
		Paths  []string `json:"paths"`
		Reason string   `json:"reason"`
	} `json:"collisions"`
	NoCollision [][]string `json:"no_collision"`
	Denied      []string   `json:"denied"`
	Allowed     []string   `json:"allowed"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

var reasons = map[string]error{
	"empty": ErrEmpty, "absolute": ErrAbsolute, "traversal": ErrTraversal, "empty_component": ErrEmptyComponent,
	"backslash": ErrBackslash, "drive_prefix": ErrDrivePrefix, "control": ErrControl, "reserved": ErrReserved,
	"too_deep": ErrTooDeep, "too_long": ErrTooLong,
}

func TestCanonicalize_Vectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Canonical {
		if got, err := Canonicalize(c.Input); err != nil || got != c.Want {
			t.Errorf("Canonicalize(%q) = %q, %v; want %q", c.Input, got, err, c.Want)
		}
	}
	for _, c := range v.Invalid {
		if _, err := Canonicalize(c.Input); !errors.Is(err, reasons[c.Reason]) {
			t.Errorf("Canonicalize(%q) = %v; want %s", c.Input, err, c.Reason)
		}
	}
	long := strings.Repeat("a", v.TooLong)
	if _, err := Canonicalize(long); !errors.Is(err, ErrTooLong) {
		t.Errorf("a %d byte path = %v", v.TooLong, err)
	}
	if _, err := Canonicalize(strings.Repeat("a", 240)); err != nil {
		t.Errorf("a 240 byte path = %v", err)
	}
	if _, err := Canonicalize("a/\xff.html"); !errors.Is(err, ErrInvalidUTF8) {
		t.Errorf("invalid UTF-8 = %v", err)
	}
}

func TestCheckSet_Vectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Collision {
		canon := make([]string, len(c.Paths))
		for i, p := range c.Paths {
			canon[i], _ = Canonicalize(p)
		}
		if err := CheckSet(canon); err == nil {
			t.Errorf("%v (%s) was accepted", c.Paths, c.Reason)
		}
	}
	for _, set := range v.NoCollision {
		if err := CheckSet(set); err != nil {
			t.Errorf("%v: %v", set, err)
		}
	}
}

func TestDenied_Vectors(t *testing.T) {
	v := loadVectors(t)
	for _, p := range v.Denied {
		if _, bad := Denied(p); !bad {
			t.Errorf("%q was allowed", p)
		}
	}
	for _, p := range v.Allowed {
		if why, bad := Denied(p); bad {
			t.Errorf("%q was denied (%s)", p, why)
		}
	}
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuild_WalksHashesAndSkipsWhatDoesNotBelong(t *testing.T) {
	root := t.TempDir()
	write(t, root, "index.html", "<h1>hi</h1>")
	write(t, root, "css/site.css", "body{}")
	write(t, root, "node_modules/x/index.js", "x")
	write(t, root, ".DS_Store", "x")
	write(t, root, "drafts/secret.html", "x")
	write(t, root, ".dropignore", "# private\ndrafts/\n*.tmp\n")
	write(t, root, "a.tmp", "x")
	if err := os.Symlink(filepath.Join(root, "index.html"), filepath.Join(root, "link.html")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	m, err := Build(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range m.Files {
		got = append(got, f.Path)
	}
	if strings.Join(got, ",") != ".dropignore,css/site.css,index.html" {
		t.Errorf("files = %v", got)
	}
	sum := sha256.Sum256([]byte("<h1>hi</h1>"))
	if m.Files[2].SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q", m.Files[2].SHA256)
	}
	if m.TotalBytes() != int64(len("<h1>hi</h1>")+len("body{}")+len("# private\ndrafts/\n*.tmp\n")) {
		t.Errorf("total = %d", m.TotalBytes())
	}
	warn := strings.Join(m.Warnings, "|")
	for _, want := range []string{"skipped symlink link.html", "left out node_modules/", "left out .DS_Store"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warnings %q lack %q", warn, want)
		}
	}
}

func TestBuild_RefusesCredentialsUnlessIgnored(t *testing.T) {
	root := t.TempDir()
	write(t, root, "index.html", "x")
	write(t, root, ".env", "TOKEN=1")
	write(t, root, ".git/config", "x")
	_, err := Build(root)
	if err == nil || !strings.Contains(err.Error(), ".env (environment file)") || !strings.Contains(err.Error(), ".git/") || !strings.Contains(err.Error(), ".dropignore") {
		t.Fatalf("err = %v", err)
	}
	write(t, root, ".dropignore", ".env\n.git/\n")
	if _, err := Build(root); err != nil {
		t.Errorf("ignored credentials still refused: %v", err)
	}
}

func TestBuild_ASingleFileAndBadInput(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.md", "# hi")
	m, err := Build(filepath.Join(dir, "notes.md"))
	if err != nil || len(m.Files) != 1 || m.Files[0].Path != "notes.md" || m.Files[0].Size != 4 || len(m.Files[0].SHA256) != 64 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if _, err := Build(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing path was accepted")
	}
	empty := t.TempDir()
	if _, err := Build(empty); err == nil || !strings.Contains(err.Error(), "nothing to publish") {
		t.Errorf("empty folder err = %v", err)
	}
	write(t, dir, ".env", "x")
	if _, err := Build(filepath.Join(dir, ".env")); err == nil {
		t.Error("a credential file was accepted")
	}
	write(t, empty, "A.html", "x")
	write(t, empty, "a.html", "y")
	entries, _ := os.ReadDir(empty)
	if len(entries) == 2 {
		if _, err := Build(empty); err == nil || !strings.Contains(err.Error(), "differ only in case") {
			t.Errorf("case collision err = %v", err)
		}
	}
}
