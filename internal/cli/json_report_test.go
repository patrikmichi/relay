package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runSplit runs cmd with stdout and stderr kept apart.
func runSplit(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

func decodeOne(t *testing.T, stdout string, into any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(into); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%q", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one document:\n%s", stdout)
	}
}

func TestSkillInstallJSONIsOneDocumentWithPlanAndWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(t.TempDir(), "my-skill")
	writeSkillFiles(t, src, map[string][]byte{"SKILL.md": []byte("---\nname: my-skill\ndescription: d\n---\n\nbody\n")})

	stdout, stderr, err := runSplit(t, SkillInstallCmd(), src, "--to", "claude", "--json")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, stderr)
	}
	var doc artifactReport
	decodeOne(t, stdout, &doc)
	if doc.Command != "skill install" || doc.DryRun || len(doc.Plans) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	p := doc.Plans[0]
	wantPath := filepath.Join(home, ".claude", "skills", "my-skill")
	if p.To != "claude" || p.Status != "written" || p.Written != filepath.Join(wantPath) || p.Loss == nil {
		t.Errorf("plan = %+v", p)
	}
	if !strings.Contains(stderr, "fidelity") || !strings.Contains(stderr, "written:") {
		t.Errorf("progress should be on stderr, got %q", stderr)
	}
	if strings.Contains(stdout, "written:") {
		t.Errorf("text leaked into the JSON stdout:\n%s", stdout)
	}
	if !strings.Contains(strings.Join(doc.Warnings, " "), "local path") {
		t.Errorf("the local-source warning is missing: %v", doc.Warnings)
	}
}

func TestSkillMigrateJSONDryRunAndRefusal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserSkill(t, home)
	writeSkillFiles(t, filepath.Join(home, ".claude", "skills", "plain"), map[string][]byte{"SKILL.md": []byte("---\nname: plain\ndescription: d\n---\n\nbody\n")})

	stdout, stderr, err := runSplit(t, SkillMigrateCmd(), "plain", "--from", "claude", "--to", "codex", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, stderr)
	}
	var doc artifactReport
	decodeOne(t, stdout, &doc)
	if !doc.DryRun || len(doc.Plans) != 1 || doc.Plans[0].Status != "dry-run" || doc.Plans[0].Written != "" {
		t.Fatalf("doc = %+v", doc)
	}

	// A refused projection (a tool restriction the target cannot hold) is in
	// the document with the security item that caused it.
	stdout, _, err = runSplit(t, SkillMigrateCmd(), "git-helper", "--from", "claude", "--to", "codex", "--json")
	if err == nil {
		t.Fatal("a lost tool restriction was accepted")
	}
	var refused artifactReport
	decodeOne(t, stdout, &refused)
	if len(refused.Plans) != 1 || refused.Plans[0].Status != "refused" || refused.Error == "" {
		t.Fatalf("refusal doc = %+v", refused)
	}
	var security bool
	for _, l := range refused.Plans[0].Loss {
		security = security || l.Security
	}
	if !security {
		t.Errorf("the security item is missing: %+v", refused.Plans[0].Loss)
	}

	// A failure still prints the document, with the error, and exits non-zero.
	stdout, _, err = runSplit(t, SkillMigrateCmd(), "does-not-exist", "--from", "claude", "--json")
	if err == nil {
		t.Fatal("a missing skill succeeded")
	}
	var failed artifactReport
	decodeOne(t, stdout, &failed)
	if failed.Error == "" || failed.Plans == nil {
		t.Errorf("failure document = %+v", failed)
	}
}

func TestSkillReadCommandsJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserSkill(t, home)
	writeSkillFiles(t, filepath.Join(home, ".claude", "skills", "plain"), map[string][]byte{"SKILL.md": []byte("---\nname: plain\ndescription: d\n---\n\nbody\n")})

	stdout, stderr, err := runSplit(t, SkillListCmd(), "--json")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, stderr)
	}
	var list struct {
		Command string `json:"command"`
		Skills  []struct {
			Provider string `json:"provider"`
			Name     string `json:"name"`
			Scope    string `json:"scope"`
			Path     string `json:"path"`
		} `json:"skills"`
	}
	decodeOne(t, stdout, &list)
	if list.Command != "skill list" || len(list.Skills) < 2 || list.Skills[0].Path == "" {
		t.Errorf("list = %+v", list)
	}
	if !strings.Contains(stderr, "plain") || strings.Contains(stdout, "plain  ") {
		t.Errorf("the text listing belongs on stderr:\nstderr %q\nstdout %q", stderr, stdout)
	}

	stdout, _, err = runSplit(t, SkillListCmd(), "--provider", "cline", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var empty map[string]any
	decodeOne(t, stdout, &empty)
	if skills, ok := empty["skills"].([]any); !ok || len(skills) != 0 {
		t.Errorf("an empty listing must be an empty array, got %v", empty["skills"])
	}

	stdout, _, err = runSplit(t, SkillDiffCmd(), "plain", "--from", "claude", "--to", "codex", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var diff map[string]any
	decodeOne(t, stdout, &diff)
	if diff["command"] != "skill diff" || diff["from"] != "claude" || diff["to"] != "codex" || diff["fidelity"] == nil || diff["fieldDiff"] == nil || diff["wouldWrite"] == nil {
		t.Errorf("diff = %v", diff)
	}

	stdout, _, err = runSplit(t, SkillScanCmd(), "plain", "--from", "claude", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var scan map[string]any
	decodeOne(t, stdout, &scan)
	if scan["command"] != "skill scan" || scan["result"] == nil {
		t.Errorf("scan = %v", scan)
	}

	stdout, _, err = runSplit(t, SkillScanCmd(), "does-not-exist", "--from", "claude", "--json")
	if err == nil {
		t.Fatal("a missing skill scanned")
	}
	var failed map[string]any
	decodeOne(t, stdout, &failed)
	if failed["command"] != "skill scan" || failed["error"] == nil {
		t.Errorf("a failed scan must say why in the document: %v", failed)
	}
}

func TestAgentCommandsJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeUserAgent(t, home)

	stdout, stderr, err := runSplit(t, AgentMigrateCmd(), "reviewer", "--from", "claude", "--to", "opencode", "--accept-loss", "--json")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, stderr)
	}
	var doc artifactReport
	decodeOne(t, stdout, &doc)
	if doc.Command != "agent migrate" || len(doc.Plans) != 1 || doc.Plans[0].Status != "written" || len(doc.Plans[0].Loss) == 0 {
		t.Fatalf("doc = %+v", doc)
	}
	if _, err := os.Stat(doc.Plans[0].Written); err != nil {
		t.Errorf("the reported path was not written: %v", err)
	}

	stdout, _, err = runSplit(t, AgentListCmd(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Command string `json:"command"`
		Agents  []struct {
			Provider, Name string
		} `json:"agents"`
	}
	decodeOne(t, stdout, &list)
	if list.Command != "agent list" || len(list.Agents) < 2 {
		t.Errorf("list = %+v", list)
	}

	stdout, _, err = runSplit(t, AgentDiffCmd(), "reviewer", "--from", "claude", "--to", "opencode", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var diff map[string]any
	decodeOne(t, stdout, &diff)
	if diff["command"] != "agent diff" || diff["fidelity"] == nil || diff["fieldDiff"] == nil {
		t.Errorf("diff = %v", diff)
	}

	stdout, _, err = runSplit(t, AgentScanCmd(), "reviewer", "--from", "claude", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var scan map[string]any
	decodeOne(t, stdout, &scan)
	if scan["command"] != "agent scan" || scan["result"] == nil {
		t.Errorf("scan = %v", scan)
	}

	stdout, _, err = runSplit(t, AgentUninstallCmd(), "reviewer", "--from", "opencode", "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var un map[string]any
	decodeOne(t, stdout, &un)
	if un["removed"] != true || un["path"] == nil {
		t.Errorf("uninstall = %v", un)
	}

	stdout, _, err = runSplit(t, AgentRollbackCmd(), "--last", "--json")
	var rb map[string]any
	decodeOne(t, stdout, &rb)
	if err == nil && rb["rolledBack"] == nil {
		t.Errorf("rollback = %v", rb)
	}
	if err != nil && rb["error"] == nil {
		t.Errorf("a failed rollback must say why in the document: %v", rb)
	}
}
