package cli

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// Tests that configure a gateway only to see what a sync writes into agent
// files must not fetch pack assignments from it; the pack tests turn the
// real loader back on against their own server.
func TestMain(m *testing.M) {
	packSyncLoader = func(*cobra.Command, string) (*packSync, error) { return nil, nil }
	os.Exit(m.Run())
}
