package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// contractFixturesEnv names a directory of fixtures written by the gateway's
// own route tests. Unset, the vendored copy under testdata/contracts is used.
const contractFixturesEnv = "ZUANO_CONTRACT_FIXTURES"

// The package tests change directory, so the vendored path is fixed at load.
var vendoredFixtures, _ = filepath.Abs(filepath.Join("testdata", "contracts"))

// contractFixtureDir is where fixtures are read from for this run.
func contractFixtureDir() string {
	if dir := os.Getenv(contractFixturesEnv); dir != "" {
		return dir
	}
	return vendoredFixtures
}

// contractFixture reads one gateway response document. A fixture missing from
// a directory given through the environment is a failure, not a fallback:
// the gateway is expected to produce every document the client reads.
func contractFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractFixtureDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestContractFixtureDir_PrefersTheEnvironment(t *testing.T) {
	if got := contractFixtureDir(); got != vendoredFixtures {
		t.Errorf("default dir = %q, want %q", got, vendoredFixtures)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(contractFixturesEnv, dir)
	if got := contractFixture(t, "x.json"); got != `{"a":1}` {
		t.Errorf("fixture = %q", got)
	}
}
