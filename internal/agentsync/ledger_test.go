package agentsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

func TestLedgerSaveKeepsAConcurrentSyncsEntries(t *testing.T) {
	env := newEnv(t)
	a, err := LoadLedger(env.Home)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadLedger(env.Home)
	if err != nil {
		t.Fatal(err)
	}
	a.set("/cfg/a.json", "mcp:one", "h1")
	b.set("/cfg/b.json", "mcp:two", "h2")
	var wg sync.WaitGroup
	for _, l := range []*Ledger{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Save(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got, err := LoadLedger(env.Home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files["/cfg/a.json"]["mcp:one"] != "h1" || got.Files["/cfg/b.json"]["mcp:two"] != "h2" {
		t.Fatalf("an entry was lost: %+v", got.Files)
	}
}

func TestLedgerDoesNotPersistEmptyFileEntries(t *testing.T) {
	env := newEnv(t)
	l, err := LoadLedger(env.Home)
	if err != nil {
		t.Fatal(err)
	}
	l.entries("/cfg/untouched.json") // a read of a file relay never wrote
	l.set("/cfg/a.json", "mcp:one", "h1")
	l.drop("/cfg/a.json", "mcp:one")
	l.set("/cfg/b.json", "mcp:two", "h2")
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(LedgerPath(env.Home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "untouched") || strings.Contains(string(raw), "a.json") {
		t.Errorf("empty entries were persisted:\n%s", raw)
	}
}

func TestCorruptHookLedgerKeyDoesNotPanic(t *testing.T) {
	env := newEnv(t)
	settings := filepath.Join(env.Home, ".claude", "settings.json")
	writeFile(t, settings, "{}\n")
	led, err := LoadLedger(env.Home)
	if err != nil {
		t.Fatal(err)
	}
	led.set(settings, "hook:nocolon", "x")
	if err := led.Save(); err != nil {
		t.Fatal(err)
	}
	syncAll(t, env, userBundle(fixtureBundle()), Options{Agents: []AgentID{Claude}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindHooks}})
}

func TestCrashBetweenFilesAndLedgerIsRecoveredForward(t *testing.T) {
	for _, stage := range []string{"committing", "applied"} {
		t.Run(stage, func(t *testing.T) {
			env := newEnv(t)
			opts := Options{Agents: []AgentID{Claude}, Scopes: []Scope{ScopeUser}, Kinds: []Kind{KindMCP, KindHooks}}
			plan, err := Build(env, userBundle(fixtureBundle()), opts)
			if err != nil {
				t.Fatal(err)
			}
			restore := txn.SetCommitHook(func(s string) error {
				if s == stage {
					return errors.New("injected crash")
				}
				return nil
			})
			err = plan.Apply(context.Background())
			restore()
			if err == nil {
				t.Fatal("the injected crash did not stop the commit")
			}
			if !fileExists(filepath.Join(env.Home, ".claude.json")) {
				t.Fatal("setup: the files were not written before the crash")
			}

			if _, err := txn.RecoverAll(context.Background()); err != nil {
				t.Fatalf("RecoverAll: %v", err)
			}
			if !fileExists(filepath.Join(env.Home, ".claude.json")) {
				t.Fatal("recovery rolled the files back")
			}
			// The ledger caught up, so the next sync sees relay's own entries,
			// not user entries it must treat as conflicts.
			again := syncAll(t, env, userBundle(fixtureBundle()), opts)
			for _, it := range again.Items {
				if it.Status == StatusConflict || it.Status == StatusAdded || it.Status == StatusUpdated {
					t.Errorf("after recovery the next sync reports %s %s %s", it.Kind, it.Name, it.Status)
				}
			}
		})
	}
}

func TestWindsurfProjectDirStaysWhereTheLedgerWrote(t *testing.T) {
	env := newEnv(t)
	opts := Options{Agents: []AgentID{Windsurf}, Scopes: []Scope{ScopeProject}, Kinds: []Kind{KindRules}}
	syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, opts)
	if !fileExists(filepath.Join(env.Project, ".windsurf", "rules", "style.md")) {
		t.Fatal("setup: rules not written under .windsurf")
	}

	if err := os.MkdirAll(filepath.Join(env.Project, ".devin"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := syncAll(t, env, map[Scope]Bundle{ScopeProject: fixtureBundle()}, opts)
	for _, it := range plan.Items {
		if it.Status == StatusAdded {
			t.Errorf("creating .devin re-added %s elsewhere", it.Name)
		}
	}
	if fileExists(filepath.Join(env.Project, ".devin", "rules", "style.md")) {
		t.Error("rules were duplicated into .devin")
	}
}
