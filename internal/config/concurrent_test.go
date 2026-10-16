package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/patrikmichi/zuano/internal/config"
)

func TestSetters_ConcurrentUpdatesAreNotLost(t *testing.T) {
	withTempHome(t)
	var wg sync.WaitGroup
	errs := make(chan error, 3*20)
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); errs <- config.SetEmail("a@example.com") }()
		go func() { defer wg.Done(); errs <- config.SetAgentEmail("agent@example.com") }()
		go func() {
			defer wg.Done()
			errs <- config.PinSigningKeys("https://gw.example.com", []config.SigningKey{{KID: "k", Alg: "ed25519", PublicKey: "p"}})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Email != "a@example.com" || cfg.AgentEmail != "agent@example.com" || len(cfg.SigningKeys["https://gw.example.com"]) != 1 {
		t.Errorf("an update was lost: %+v", cfg)
	}
}

func TestSave_LeavesNoTempFilesAndNeverATornFile(t *testing.T) {
	home := withTempHome(t)
	for i := 0; i < 5; i++ {
		if err := config.Save(config.Config{GatewayURL: fmt.Sprintf("https://gw%d.example.com", i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(home, ".config", "zuano"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	if cfg, err := config.Load(); err != nil || cfg.GatewayURL != "https://gw4.example.com" {
		t.Errorf("cfg = %+v, %v", cfg, err)
	}
}
