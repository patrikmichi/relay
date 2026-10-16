package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
)

func TestInstallIDIsCreatedOnceAndKept(t *testing.T) {
	home := withTempHome(t)
	a, err := config.InstallID()
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) {
		t.Fatalf("id = %q, %v", a, err)
	}
	b, err := config.InstallID()
	if err != nil || a != b {
		t.Errorf("second id = %q, %v", b, err)
	}
	info, err := os.Stat(filepath.Join(home, ".config", "zuano", "install.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file = %v, %v", info, err)
	}
}

func TestInstallKeepsTheIDWhenOtherFieldsChangeAndRepairsAGarbledFile(t *testing.T) {
	home := withTempHome(t)
	id, _ := config.InstallID()
	in, _ := config.LoadInstall()
	in.HostLabel, in.HostLabelSet = "", true
	if err := config.SaveInstall(in); err != nil {
		t.Fatal(err)
	}
	if err := config.RecordReport(3); err != nil {
		t.Fatal(err)
	}
	got, _ := config.LoadInstall()
	if got.InstallID != id || !got.HostLabelSet || got.LastReportAt == "" || got.LastReportSent != 3 {
		t.Errorf("install = %+v", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "zuano", "install.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := config.InstallID(); err != nil || again == "" || again == id {
		t.Errorf("repaired id = %q, %v", again, err)
	}
}

func TestInstallFailsWhenTheFileCannotBeRead(t *testing.T) {
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".config", "zuano", "install.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadInstall(); err == nil {
		t.Error("a directory was read as install.json")
	}
}
