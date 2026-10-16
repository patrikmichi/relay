package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/plugins"
)

func TestDoctor_ReportsPluginDriftAndAMissingClaudeHelper(t *testing.T) {
	home := setupDoctor(t)
	output, err := runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "plugins"); err != nil || !c.OK || c.Detail != "no plugins installed" {
		t.Fatalf("empty: %+v, %v", c, err)
	}

	file := filepath.Join(home, ".cursor", "plugins", "local", "toolkit", "a.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("as written"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ := plugins.LoadLedger(plugins.LedgerPath(home))
	l.Set("toolkit", "cursor", plugins.Install{Version: "1.2.0", Method: "native", Files: []plugins.FileRef{{Path: file, SHA256: sha256Hex([]byte("as written"))}}})
	l.Set("toolkit", "claude", plugins.Install{Version: "1.2.0", Method: "native", Marketplace: "zuano-acme"})
	if err := l.Save(plugins.LedgerPath(home)); err != nil {
		t.Fatal(err)
	}
	output, err = runDoctorJSON(t)
	if c := doctorCheckNamed(t, output, "plugins"); !c.OK || !strings.Contains(c.Detail, "2 plugin install(s), no drift") {
		t.Fatalf("clean: %+v, %v", c, err)
	}
	if c := doctorCheckNamed(t, output, "plugin-helper:claude"); c.OK || !strings.Contains(c.Detail, "plugin feeds setup --to claude") {
		t.Errorf("helper: %+v", c)
	}

	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"extraKnownMarketplaces":{"zuano-acme":{"headersHelper":"zuano auth header --json"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runDoctorJSON(t)
	if err == nil {
		t.Error("doctor passed with drifted plugin files")
	}
	if c := doctorCheckNamed(t, output, "plugins"); c.OK || !strings.Contains(c.Detail, "1 installed file(s) differ") {
		t.Errorf("drift: %+v", c)
	}
	if c := doctorCheckNamed(t, output, "plugin-helper:claude"); !c.OK {
		t.Errorf("helper: %+v", c)
	}
}
