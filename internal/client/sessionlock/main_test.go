package sessionlock

import (
	"os"
	"testing"
)

// TestMain points the home directory at a temporary one so the lock files of
// tests that do not isolate it never land in the real config directory.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "relay-sessionlock-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
