package cli

import (
	"strings"
	"testing"
)

func TestSyncCmd_RejectsArgsAndTheRemovedMarketplaceFlags(t *testing.T) {
	if _, err := runCommand(t, SyncCmd(), "extra"); err == nil {
		t.Error("expected positional args to be rejected")
	}
	for _, flag := range []string{"--marketplace", "--require-signature"} {
		if _, err := runCommand(t, SyncCmd(), flag); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s = %v, want an unknown flag error", flag, err)
		}
	}
	if _, err := runCommand(t, SyncCmd(), "--dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("--dir = %v, want an unknown flag error", err)
	}
	withNoGateway(t)
	t.Chdir(t.TempDir()) // the project scope looks at the working directory too
	if _, err := runCommand(t, SyncCmd()); err == nil || !strings.Contains(err.Error(), "no supported coding agent detected") {
		t.Errorf("expected the no-agent error, got %v", err)
	}
}
