package client

import (
	"os"
	"testing"
)

// TestMain points the home directory at a temporary one so refresh locks of
// tests that do not isolate it never land in the real config directory.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "zuano-client-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
