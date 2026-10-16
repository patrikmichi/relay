package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/agentport"
)

func codexSkillFile(t *testing.T, name, rel string) string {
	t.Helper()
	a, ok := agentport.AdapterByID("codex")
	if !ok {
		t.Fatal("codex adapter missing")
	}
	return filepath.Join(a.UserDirs()[0], name, rel)
}

func seedClaudeArtifacts(t *testing.T, home, skillBody, agentBody string) {
	t.Helper()
	writeTestFile(t, filepath.Join(home, ".claude", "skills", "hello", "SKILL.md"), "---\nname: hello\ndescription: Say hello\n---\n"+skillBody+"\n")
	writeTestFile(t, filepath.Join(home, ".claude", "agents", "reviewer.md"), "---\nname: reviewer\ndescription: Reviews code\n---\n"+agentBody+"\n")
}

var skillArgs = []string{"--to", "claude,codex", "--kind", "skills,subagents"}

func TestSyncPropagatesSourceChangesToRelayInstalledArtifacts(t *testing.T) {
	home := syncHome(t)
	seedClaudeArtifacts(t, home, "Say hello.", "Review carefully.")
	writeTestFile(t, filepath.Join(home, ".claude", "skills", "hello", "scripts", "run.sh"), "echo hi\n")
	if _, err := runCommand(t, SyncCmd(), skillArgs...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(codexSkillFile(t, "hello", "scripts/run.sh")); err != nil {
		t.Fatalf("resource not copied: %v", err)
	}

	seedClaudeArtifacts(t, home, "Say hello warmly.", "Review very carefully.")
	if err := os.Remove(filepath.Join(home, ".claude", "skills", "hello", "scripts", "run.sh")); err != nil {
		t.Fatal(err)
	}

	preview, err := runCommand(t, SyncCmd(), append([]string{"--dry-run"}, skillArgs...)...)
	if err != nil || strings.Count(preview, " updated ") != 2 {
		t.Fatalf("dry run should report two updates: %v\n%s", err, preview)
	}
	if strings.Contains(readTestFile(t, codexSkillFile(t, "hello", "SKILL.md")), "warmly") {
		t.Fatal("dry run wrote the update")
	}

	out, err := runCommand(t, SyncCmd(), skillArgs...)
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if !strings.Contains(readTestFile(t, codexSkillFile(t, "hello", "SKILL.md")), "warmly") {
		t.Errorf("skill not updated:\n%s", out)
	}
	if _, err := os.Stat(codexSkillFile(t, "hello", "scripts/run.sh")); err == nil {
		t.Error("a file dropped from the source skill was left behind")
	}
	if !strings.Contains(readTestFile(t, filepath.Join(home, ".codex", "agents", "reviewer.toml")), "very carefully") {
		t.Error("subagent not updated")
	}

	again, _ := runCommand(t, SyncCmd(), skillArgs...)
	if strings.Contains(again, " updated ") || strings.Contains(again, " added ") || strings.Contains(again, "conflict") {
		t.Errorf("third sync should be a no-op:\n%s", again)
	}
}

func TestSyncKeepsUserEditedAndForeignArtifacts(t *testing.T) {
	home := syncHome(t)
	seedClaudeArtifacts(t, home, "Say hello.", "Review carefully.")
	// A skill the user already had on codex before relay ever ran.
	writeTestFile(t, codexSkillFile(t, "hello", "SKILL.md"), "---\nname: hello\ndescription: Mine\n---\nmy own\n")

	out, err := runCommand(t, SyncCmd(), skillArgs...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not installed by relay") {
		t.Errorf("foreign skill not reported:\n%s", out)
	}
	if !strings.Contains(readTestFile(t, codexSkillFile(t, "hello", "SKILL.md")), "my own") {
		t.Fatal("user skill overwritten")
	}

	// The subagent was installed by relay; the user then edits it.
	agentPath := filepath.Join(home, ".codex", "agents", "reviewer.toml")
	edited := readTestFile(t, agentPath) + "# tweaked by me\n"
	writeTestFile(t, agentPath, edited)
	seedClaudeArtifacts(t, home, "Say hello.", "Review differently.")
	out, _ = runCommand(t, SyncCmd(), skillArgs...)
	if !strings.Contains(out, "edited since relay installed it") {
		t.Errorf("edited subagent not reported:\n%s", out)
	}
	if readTestFile(t, agentPath) != edited {
		t.Fatal("edited subagent overwritten")
	}

	out, err = runCommand(t, SyncCmd(), append([]string{"--force"}, skillArgs...)...)
	if err != nil {
		t.Fatalf("force: %v\n%s", err, out)
	}
	if strings.Contains(readTestFile(t, agentPath), "tweaked by me") || !strings.Contains(readTestFile(t, codexSkillFile(t, "hello", "SKILL.md")), "Say hello.") {
		t.Errorf("--force did not replace both:\n%s", out)
	}
}
