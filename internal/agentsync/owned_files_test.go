package agentsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWithTheMarkerButNoLedgerEntryIsTheUsers(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude", "commands", "review.md")
	own := "# my own review command\n<!-- " + Marker + " -->\n"
	writeFile(t, path, own)

	plan := syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	if got := statusOf(plan.Items, KindCommands, "review"); got != StatusConflict {
		t.Fatalf("status = %q, want conflict", got)
	}
	if readFile(t, path) != own {
		t.Fatal("a file relay never wrote was overwritten because it contained the marker")
	}

	plan = syncAll(t, env, userBundle(Bundle{}), userOpts(Claude))
	if readFile(t, path) != own {
		t.Fatal("a file relay never wrote was removed because it contained the marker")
	}
	if got := statusOf(plan.Items, KindCommands, "review"); got == StatusRemoved {
		t.Fatalf("status = %q", got)
	}
}

func TestEditedManagedFileIsKeptAndRemovedOnlyWhenUntouched(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude", "commands", "review.md")
	syncAll(t, env, userBundle(fixtureBundle()), userOpts(Claude))
	written := readFile(t, path)

	edited := written + "\nMy own addition.\n"
	writeFile(t, path, edited)

	changed := fixtureBundle()
	changed.Commands[0].Body = "Review harder."
	plan := syncAll(t, env, userBundle(changed), userOpts(Claude))
	if got := statusOf(plan.Items, KindCommands, "review"); got != StatusConflict {
		t.Fatalf("edited file status = %q, want conflict", got)
	}
	if readFile(t, path) != edited {
		t.Fatal("user edits were overwritten")
	}

	plan = syncAll(t, env, userBundle(Bundle{}), userOpts(Claude))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an edited managed file was removed: %v", err)
	}
	if got := statusOf(plan.Items, KindCommands, "review"); got != StatusConflict {
		t.Fatalf("status = %q, want conflict", got)
	}

	writeFile(t, path, written)
	syncAll(t, env, userBundle(Bundle{}), userOpts(Claude))
	if _, err := os.Stat(path); err == nil {
		t.Fatal("an untouched managed file was not removed")
	}
}

func TestForceAdoptsAnUnledgeredFile(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude", "commands", "review.md")
	writeFile(t, path, "mine\n")
	opts := userOpts(Claude)
	opts.Force = true
	syncAll(t, env, userBundle(fixtureBundle()), opts)
	if !strings.Contains(readFile(t, path), "Review $ARGUMENTS") {
		t.Fatal("--force did not replace the file")
	}
	syncAll(t, env, userBundle(Bundle{}), userOpts(Claude))
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a file relay wrote under --force was not removed once unwanted")
	}
}
