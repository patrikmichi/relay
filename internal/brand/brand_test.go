package brand

import "testing"

func TestInstallHeaderIsDerivedFromTheProductName(t *testing.T) {
	if InstallHeader != "X-"+Product+"-Install" {
		t.Errorf("InstallHeader = %q", InstallHeader)
	}
}

func TestCmdPrefixesTheCommandName(t *testing.T) {
	if got, want := Cmd("login --agent"), Name+" login --agent"; got != want {
		t.Errorf("Cmd = %q, want %q", got, want)
	}
}
