package brand

import "testing"

func TestCmdPrefixesTheCommandName(t *testing.T) {
	if got, want := Cmd("login --agent"), Name+" login --agent"; got != want {
		t.Errorf("Cmd = %q, want %q", got, want)
	}
}
