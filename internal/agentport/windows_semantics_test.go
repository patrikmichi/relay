package agentport

import (
	"os"
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
