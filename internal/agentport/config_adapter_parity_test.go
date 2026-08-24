package agentport

import (
	"encoding/base64"
	"path/filepath"
	"testing"
)

// This file is the byte-exact PARITY HARNESS (design §5.1/§5.2/R1): golden
// output captured from the ORIGINAL hard-coded adapter_{claude,codex,
// cursor,opencode}.go implementations — before they were deleted — for the
// git-helper fixture each provider's *_test.go already exercises. Every
// assertion below runs against NewClaudeAdapter()/NewCodexAdapter()/
// NewCursorAdapter()/NewOpencodeAdapter(), which now return *configAdapter
// (built from providers/<id>.yml); byte-for-byte equality with these
// golden constants is the proof the generic serializer reproduces the
// deleted structs' Marshal output exactly (key order + universal
// `,omitempty` semantics — R1).
//
// Golden bytes were captured by running the pre-refactor
// claudeAdapter/codexAdapter/cursorAdapter/opencodeAdapter's own
// Load(testdata/<provider>/git-helper) -> Project() and base64-encoding the
// resulting "SKILL.md" (and, for Codex, "agents/openai.yaml") file bytes,
// in the same working tree, immediately before those 4 files were deleted
// in this refactor.
//
// Coverage note: this byte-exact harness covers the 4 base fixtures
// (claude/codex/cursor/opencode) plus the Codex sidecar. It intentionally
// does NOT re-cover every other pre-refactor edge case — those are
// retained in their own pre-refactor test files, which still exercise the
// configAdapter-backed constructors directly: adapter_claude_test.go
// (legacy flat-command form), adapter_cursor_test.go (paths declared as a
// bare string, not a list), and list_test.go (recursive project-scope
// discovery). A future reader can trust the "byte-exact parity" claim by
// checking this file plus those three, rather than re-deriving it.

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 golden constant: %v", err)
	}
	return b
}

const goldenClaudeSkillMD = `LS0tCm5hbWU6IGdpdC1oZWxwZXIKZGVzY3JpcHRpb246IFN1bW1hcml6ZXMgdW5jb21taXR0ZWQgZ2l0IGNoYW5nZXMgYW5kIGZsYWdzIHJpc2t5IGRpZmZzLiBVc2Ugd2hlbiBhc2tlZCB3aGF0IGNoYW5nZWQgb3IgZm9yIGEgY29tbWl0IG1lc3NhZ2UuCmFsbG93ZWQtdG9vbHM6CiAgICAtIEJhc2goZ2l0IGRpZmYgKikKICAgIC0gQmFzaChnaXQgc3RhdHVzICopCmRpc2FibGUtbW9kZWwtaW52b2NhdGlvbjogZmFsc2UKbGljZW5zZTogTUlUCi0tLQoKIyMgSW5zdHJ1Y3Rpb25zCgpTdW1tYXJpemUgdGhlIGN1cnJlbnQgZ2l0IGRpZmYgaW4gdHdvIG9yIHRocmVlIGJ1bGxldCBwb2ludHMsIHRoZW4gbGlzdCBhbnkKcmlza3MgeW91IG5vdGljZSAobWlzc2luZyBlcnJvciBoYW5kbGluZywgaGFyZGNvZGVkIHZhbHVlcywgdGVzdHMgbmVlZGluZwp1cGRhdGVzKS4KClJ1biB0aGUgYnVuZGxlZCBoZWxwZXIgc2NyaXB0IGZvciBhIHF1aWNrIHN0YXQgc3VtbWFyeToKCmBgYGJhc2gKc2NyaXB0cy9ydW4uc2gKYGBgCg==`

const goldenCodexSkillMD = `LS0tCm5hbWU6IGdpdC1oZWxwZXIKZGVzY3JpcHRpb246IFN1bW1hcml6ZXMgdW5jb21taXR0ZWQgZ2l0IGNoYW5nZXMgYW5kIGZsYWdzIHJpc2t5IGRpZmZzLiBVc2Ugd2hlbiBhc2tlZCB3aGF0IGNoYW5nZWQgb3IgZm9yIGEgY29tbWl0IG1lc3NhZ2UuCi0tLQoKIyMgSW5zdHJ1Y3Rpb25zCgpTdW1tYXJpemUgdGhlIGN1cnJlbnQgZ2l0IGRpZmYgaW4gdHdvIG9yIHRocmVlIGJ1bGxldCBwb2ludHMsIHRoZW4gbGlzdCBhbnkKcmlza3MgeW91IG5vdGljZSAobWlzc2luZyBlcnJvciBoYW5kbGluZywgaGFyZGNvZGVkIHZhbHVlcywgdGVzdHMgbmVlZGluZwp1cGRhdGVzKS4KClNlZSByZWZlcmVuY2VzL25vdGVzLm1kIGZvciBleHRyYSBjb250ZXh0Lgo=`

const goldenCodexSidecar = `aW50ZXJmYWNlOgogICAgZGlzcGxheV9uYW1lOiBHaXQgSGVscGVyCiAgICBzaG9ydF9kZXNjcmlwdGlvbjogU3VtbWFyaXplIGdpdCBkaWZmcwogICAgaWNvbl9zbWFsbDogZ2l0LXNtYWxsLnBuZwogICAgaWNvbl9sYXJnZTogZ2l0LWxhcmdlLnBuZwogICAgYnJhbmRfY29sb3I6ICcjZjM0ZjI5JwogICAgZGVmYXVsdF9wcm9tcHQ6IFdoYXQgY2hhbmdlZCBpbiBteSB3b3JraW5nIHRyZWU/CnBvbGljeToKICAgIGFsbG93X2ltcGxpY2l0X2ludm9jYXRpb246IHRydWUKZGVwZW5kZW5jaWVzOgogICAgdG9vbHM6CiAgICAgICAgLSBnaXQtbWNwCg==`

const goldenCursorSkillMD = `LS0tCm5hbWU6IGdpdC1oZWxwZXIKZGVzY3JpcHRpb246IFN1bW1hcml6ZXMgdW5jb21taXR0ZWQgZ2l0IGNoYW5nZXMgYW5kIGZsYWdzIHJpc2t5IGRpZmZzLiBVc2Ugd2hlbiBhc2tlZCB3aGF0IGNoYW5nZWQgb3IgZm9yIGEgY29tbWl0IG1lc3NhZ2UuCnBhdGhzOgogICAgLSAnKiovKi5nbycKICAgIC0gJyoqLyoudHMnCmRpc2FibGUtbW9kZWwtaW52b2NhdGlvbjogdHJ1ZQptZXRhZGF0YToKICAgIGF1dGhvcjogYWNtZQotLS0KCiMjIEluc3RydWN0aW9ucwoKU3VtbWFyaXplIHRoZSBjdXJyZW50IGdpdCBkaWZmIGluIHR3byBvciB0aHJlZSBidWxsZXQgcG9pbnRzLCB0aGVuIGxpc3QgYW55CnJpc2tzIHlvdSBub3RpY2UgKG1pc3NpbmcgZXJyb3IgaGFuZGxpbmcsIGhhcmRjb2RlZCB2YWx1ZXMsIHRlc3RzIG5lZWRpbmcKdXBkYXRlcykuCg==`

const goldenOpencodeSkillMD = `LS0tCm5hbWU6IGdpdC1oZWxwZXIKZGVzY3JpcHRpb246IFN1bW1hcml6ZXMgdW5jb21taXR0ZWQgZ2l0IGNoYW5nZXMgYW5kIGZsYWdzIHJpc2t5IGRpZmZzLiBVc2Ugd2hlbiBhc2tlZCB3aGF0IGNoYW5nZWQgb3IgZm9yIGEgY29tbWl0IG1lc3NhZ2UuCmxpY2Vuc2U6IE1JVApjb21wYXRpYmlsaXR5OiBvcGVuY29kZT49MS4wLjAKbWV0YWRhdGE6CiAgICBhdXRob3I6IGFjbWUKICAgIHRlYW06IHBsYXRmb3JtCi0tLQoKIyMgSW5zdHJ1Y3Rpb25zCgpTdW1tYXJpemUgdGhlIGN1cnJlbnQgZ2l0IGRpZmYgaW4gdHdvIG9yIHRocmVlIGJ1bGxldCBwb2ludHMsIHRoZW4gbGlzdCBhbnkKcmlza3MgeW91IG5vdGljZSAobWlzc2luZyBlcnJvciBoYW5kbGluZywgaGFyZGNvZGVkIHZhbHVlcywgdGVzdHMgbmVlZGluZwp1cGRhdGVzKS4K`

func TestParity_ByteExactProjectOutput(t *testing.T) {
	cases := []struct {
		provider    string
		adapter     Adapter
		goldenSkill string
	}{
		{"claude", NewClaudeAdapter(), goldenClaudeSkillMD},
		{"codex", NewCodexAdapter(), goldenCodexSkillMD},
		{"cursor", NewCursorAdapter(), goldenCursorSkillMD},
		{"opencode", NewOpencodeAdapter(), goldenOpencodeSkillMD},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			dir := filepath.Join("testdata", c.provider, "git-helper")
			s, err := c.adapter.Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			files, loss, err := c.adapter.Project(s)
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			if len(loss) != 0 {
				t.Fatalf("Loss = %#v, want none (matches pre-refactor round-trip)", loss)
			}
			want := mustB64(t, c.goldenSkill)
			if string(files["SKILL.md"]) != string(want) {
				t.Fatalf("Project() SKILL.md bytes differ from the pre-refactor golden capture.\n--- got ---\n%s\n--- want ---\n%s", files["SKILL.md"], want)
			}
		})
	}
}

func TestParity_ByteExactCodexSidecar(t *testing.T) {
	s, err := NewCodexAdapter().Load(filepath.Join("testdata", "codex", "git-helper"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	files, _, err := NewCodexAdapter().Project(s)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := mustB64(t, goldenCodexSidecar)
	if string(files["agents/openai.yaml"]) != string(want) {
		t.Fatalf("codex sidecar bytes differ from the pre-refactor golden capture.\n--- got ---\n%s\n--- want ---\n%s", files["agents/openai.yaml"], want)
	}
}

// TestParity_DirsAndCounts locks in the exact UserDirs/ProjectDirs/
// OwnUserDirCount/OwnProjectDirCount values captured from the pre-refactor
// concrete adapters, run against a fixed $HOME so the comparison is
// deterministic.
func TestParity_DirsAndCounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		provider            string
		adapter             Adapter
		wantUserDirs        []string
		wantProjectDirs     []string
		wantOwnUserCount    int
		wantOwnProjectCount int
	}{
		{
			"claude", NewClaudeAdapter(),
			[]string{filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".claude", "commands")},
			[]string{filepath.Join(".claude", "skills"), filepath.Join(".claude", "commands")},
			2, 2,
		},
		{
			"codex", NewCodexAdapter(),
			[]string{filepath.Join(home, ".agents", "skills"), codexAdminSkillsDir},
			[]string{filepath.Join(".agents", "skills")},
			1, 1,
		},
		{
			"cursor", NewCursorAdapter(),
			[]string{
				filepath.Join(home, ".cursor", "skills"),
				filepath.Join(home, ".agents", "skills"),
				filepath.Join(home, ".claude", "skills"),
				filepath.Join(home, ".codex", "skills"),
			},
			[]string{
				filepath.Join(".cursor", "skills"),
				filepath.Join(".agents", "skills"),
				filepath.Join(".claude", "skills"),
				filepath.Join(".codex", "skills"),
			},
			1, 1,
		},
		{
			"opencode", NewOpencodeAdapter(),
			[]string{
				filepath.Join(home, ".config", "opencode", "skills"),
				filepath.Join(home, ".claude", "skills"),
				filepath.Join(home, ".agents", "skills"),
			},
			[]string{
				filepath.Join(".opencode", "skills"),
				filepath.Join(".claude", "skills"),
				filepath.Join(".agents", "skills"),
			},
			1, 1,
		},
	}

	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			gotUser := c.adapter.UserDirs()
			if !stringSlicesEqual(gotUser, c.wantUserDirs) {
				t.Errorf("UserDirs() = %v, want %v", gotUser, c.wantUserDirs)
			}
			gotProject := c.adapter.ProjectDirs()
			if !stringSlicesEqual(gotProject, c.wantProjectDirs) {
				t.Errorf("ProjectDirs() = %v, want %v", gotProject, c.wantProjectDirs)
			}
			if got := c.adapter.OwnUserDirCount(); got != c.wantOwnUserCount {
				t.Errorf("OwnUserDirCount() = %d, want %d", got, c.wantOwnUserCount)
			}
			if got := c.adapter.OwnProjectDirCount(); got != c.wantOwnProjectCount {
				t.Errorf("OwnProjectDirCount() = %d, want %d", got, c.wantOwnProjectCount)
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParity_LossFieldsUnchanged is the loss-report parity check (R2):
// cross-provider migrations must report the exact same dropped fields as
// before the refactor — already exercised end-to-end by migrate_test.go's
// TestMigrate_* cases (which now run against configAdapter, since
// New*Adapter() returns one), plus a couple of same-provider zero-loss
// smoke checks here for the two new byte-exact cases above.
func TestParity_LossFieldsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		provider string
		adapter  Adapter
	}{
		{"claude", NewClaudeAdapter()},
		{"codex", NewCodexAdapter()},
		{"cursor", NewCursorAdapter()},
		{"opencode", NewOpencodeAdapter()},
	} {
		dir := filepath.Join("testdata", tc.provider, "git-helper")
		s, err := tc.adapter.Load(dir)
		if err != nil {
			t.Fatalf("%s Load: %v", tc.provider, err)
		}
		_, loss, err := tc.adapter.Project(s)
		if err != nil {
			t.Fatalf("%s Project: %v", tc.provider, err)
		}
		if len(loss) != 0 {
			t.Errorf("%s: same-provider round trip Loss = %#v, want none", tc.provider, loss)
		}
	}
}
