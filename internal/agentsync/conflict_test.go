package agentsync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/patrikmichi/zuano/internal/agentport/txn"
)

func TestApplyFailsWhenFileChangedAfterPlanning(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude.json")
	writeFile(t, path, "{\n  \"numStartups\": 1\n}\n")

	plan, err := Build(env, userBundle(fixtureBundle()), userOpts(Claude))
	if err != nil {
		t.Fatal(err)
	}
	const edited = "{\n  \"numStartups\": 2\n}\n"
	writeFile(t, path, edited)

	if err := plan.Apply(context.Background()); !errors.Is(err, txn.ErrConflict) {
		t.Fatalf("Apply = %v, want txn.ErrConflict", err)
	}
	if got := readFile(t, path); got != edited {
		t.Fatalf("concurrent edit was overwritten:\n%s", got)
	}
}
