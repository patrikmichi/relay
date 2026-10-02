package txn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrDestinationSymlink = errors.New("destination path contains a symlink")

func VerifyNoSymlinks(root, rel string) error {
	// Defense in depth: confirm the joined path actually stays under root
	// even though every caller is expected to pass an already-cleaned rel.
	// filepath.Join lexically collapses ".." with no containment check of
	// its own, so a rel that smuggled a ".." past its caller (e.g. a
	// persisted owned-file record round-tripped without re-validation)
	// would otherwise sail through the symlink-component walk below with
	// nothing ever landing outside root to actually be a symlink.
	rootClean := filepath.Clean(root)
	full := filepath.Join(root, filepath.FromSlash(rel))
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s escapes %s", ErrDestinationSymlink, full, root)
	}

	if info, err := os.Lstat(root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrDestinationSymlink, root)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", root, err)
	}

	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrDestinationSymlink, cur)
		}
	}
	return nil
}
