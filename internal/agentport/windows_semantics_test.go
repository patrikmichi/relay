package agentport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

func TestPermissionsNeverDifferOnWindows(t *testing.T) {
	if permissionsDiffer("windows", 0o666, 0o600) {
		t.Error("a synthetic Windows mode was compared with the installed mode")
	}
	if !permissionsDiffer("linux", 0o644, 0o600) {
		t.Error("a changed mode on Linux was not reported")
	}
	if permissionsDiffer("linux", 0o600, 0o600) {
		t.Error("an unchanged mode was reported")
	}
}

func TestSameProjectRootResolvesSymlinksAndFoldsCaseWhereTheFilesystemDoes(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if !sameProjectRoot("linux", link, real) {
		t.Error("a symlinked spelling of the same project was treated as another project")
	}
	other := t.TempDir()
	if sameProjectRoot("linux", real, other) {
		t.Error("different projects compared equal")
	}
	upper := strings.ToUpper(real)
	if sameProjectRoot("linux", upper, real) && upper != real {
		t.Error("case was ignored on a case-sensitive filesystem")
	}
	if !sameProjectRoot("windows", `C:\Work\App`, `c:\work\app`) || !sameProjectRoot("darwin", "/Users/Me/App", "/users/me/app") {
		t.Error("case differences were not ignored on Windows and macOS")
	}
}

func TestIrregularEntriesCountAsLinks(t *testing.T) {
	for name, m := range map[string]os.FileMode{"symlink": os.ModeSymlink, "junction": os.ModeIrregular, "symlink dir": os.ModeSymlink | os.ModeDir} {
		if !txn.IsLinkMode(m) {
			t.Errorf("%s was not treated as a link", name)
		}
	}
	if txn.IsLinkMode(0o644) || txn.IsLinkMode(os.ModeDir|0o755) {
		t.Error("an ordinary file or directory was treated as a link")
	}
}
