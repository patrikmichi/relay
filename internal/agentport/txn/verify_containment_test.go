package txn

import (
	"errors"
	"testing"
)

// TestVerifyNoSymlinks_RejectsContainmentEscapeWithoutAnySymlink proves the
// defensive containment check: a rel that lexically walks outside root via
// ".." is rejected even when no path component along the way is actually a
// symlink — the component-by-component lstat walk alone would otherwise
// let it through, relying entirely on the caller having pre-cleaned rel.
func TestVerifyNoSymlinks_RejectsContainmentEscapeWithoutAnySymlink(t *testing.T) {
	root := t.TempDir()
	if err := VerifyNoSymlinks(root, "../../etc/passwd"); !errors.Is(err, ErrDestinationSymlink) {
		t.Fatalf("VerifyNoSymlinks with escaping rel = %v, want ErrDestinationSymlink", err)
	}
}
