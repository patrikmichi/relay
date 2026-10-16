package txn

import "os"

// IsLinkMode reports a symlink or an irregular entry. Windows reports a
// directory junction as irregular, and it redirects a path exactly as a
// symlink does.
func IsLinkMode(m os.FileMode) bool { return m&(os.ModeSymlink|os.ModeIrregular) != 0 }
