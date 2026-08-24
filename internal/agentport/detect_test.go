package agentport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetect_TempHomeNoneInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, a := range AllAdapters() {
		if a.Detect() {
			t.Errorf("%s: Detect() = true on an empty temp HOME, want false", a.ID())
		}
	}
	if got := DetectedProviders(); len(got) != 0 {
		t.Errorf("DetectedProviders() = %v, want none", got)
	}
}

func TestDetect_ClaudeInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	claude := NewClaudeAdapter()
	if !claude.Detect() {
		t.Errorf("Detect() = false, want true once ~/.claude/skills exists")
	}

	for _, a := range AllAdapters() {
		if a.ID() == ProviderClaude {
			continue
		}
		if a.Detect() {
			t.Errorf("%s: Detect() = true, want false (only Claude's dir was created)", a.ID())
		}
	}

	detected := DetectedProviders()
	if len(detected) != 1 || detected[0].ID() != ProviderClaude {
		t.Errorf("DetectedProviders() = %v, want [claude]", detected)
	}
}

func TestDetect_AllFourInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dirs := []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(home, ".config", "opencode", "skills"),
		filepath.Join(home, ".cursor", "skills"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	detected := DetectedProviders()
	if len(detected) != 4 {
		t.Fatalf("DetectedProviders() = %v (%d), want all 4", detected, len(detected))
	}
}

func TestAdapterByID(t *testing.T) {
	for _, id := range []ProviderID{ProviderClaude, ProviderCodex, ProviderOpencode, ProviderCursor} {
		a, ok := AdapterByID(id)
		if !ok {
			t.Errorf("AdapterByID(%s): ok = false, want true", id)
			continue
		}
		if a.ID() != id {
			t.Errorf("AdapterByID(%s).ID() = %s", id, a.ID())
		}
	}
	if _, ok := AdapterByID("not-a-real-provider"); ok {
		t.Errorf("AdapterByID(unknown): ok = true, want false")
	}
}
