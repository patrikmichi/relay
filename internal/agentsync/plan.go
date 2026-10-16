package agentsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/patrikmichi/zuano/internal/agentport/txn"
)

// Env locates the user's home and the project being synced. Tests point
// both at temporary directories.
type Env struct {
	Home    string
	Project string
	Getenv  func(string) string

	// windsurfDir pins the workspace config directory (.windsurf or .devin)
	// the ledger already holds files in, so creating the other one later
	// does not strand them.
	windsurfDir string
}

// pinWindsurfDir fixes windsurfProjectDir to the directory the ledger
// already records files under.
func pinWindsurfDir(env Env, led *Ledger) Env {
	if env.Project == "" {
		return env
	}
	roots := []string{env.Project}
	if real, err := filepath.EvalSymlinks(env.Project); err == nil && real != env.Project {
		roots = append(roots, real)
	}
	for _, d := range []string{".windsurf", ".devin"} {
		for file := range led.Files {
			for _, root := range roots {
				if strings.HasPrefix(file, filepath.Join(root, d)+string(filepath.Separator)) {
					env.windsurfDir = filepath.Join(env.Project, d)
					return env
				}
			}
		}
	}
	return env
}

func (e Env) getenv(k string) string {
	if e.Getenv == nil {
		return os.Getenv(k)
	}
	return e.Getenv(k)
}

func (e Env) root(scope Scope) string {
	if scope == ScopeProject {
		return e.Project
	}
	return e.Home
}

// Edit is one file change in a plan. Root and Rel locate Path for the
// symlink check that runs again right before the write.
type Edit struct {
	Path    string
	Root    string
	Rel     string
	Before  []byte
	After   []byte
	Existed bool
	Remove  bool
	Mode    fs.FileMode
}

// Ctx carries planning state. Reads see edits staged earlier in the same
// plan so two agents sharing a file (AGENTS.md) compose instead of racing.
type Ctx struct {
	Env    Env
	Ledger *Ledger
	Force  bool

	edits  map[string]*Edit
	order  []string
	places map[string][2]string
	links  map[string]string
}

// unsafePathError marks a config path relay will not touch.
type unsafePathError struct{ path, reason string }

func (e *unsafePathError) Error() string { return e.path + ": " + e.reason }

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// guard maps path to the file relay may edit. A project is untrusted input, so
// a symlink anywhere below it is refused. A user config may be a dotfile-manager
// symlink, followed only while it stays inside the home directory.
func (c *Ctx) guard(scope Scope, path string) (string, error) {
	path = filepath.Clean(path)
	root := filepath.Clean(c.Env.root(scope))
	rel, err := filepath.Rel(root, path)
	if scope == ScopeUser && (err != nil || escapes(rel)) {
		// A config directory the user relocated (XDG_CONFIG_HOME and friends).
		root, rel, err = filepath.Dir(path), filepath.Base(path), nil
	}
	if err != nil || escapes(rel) {
		return "", &unsafePathError{path, "outside " + root}
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootReal = root
	}
	target := path
	if scope == ScopeProject {
		if err := txn.VerifyNoSymlinks(rootReal, rel); err != nil {
			return "", &unsafePathError{path, "a symlink below the project would redirect the write; relay does not follow project symlinks"}
		}
	} else {
		resolved := resolvePath(path)
		rr, err := filepath.Rel(rootReal, resolved)
		if err != nil || escapes(rr) {
			return "", &unsafePathError{path, "symlink resolves outside " + root}
		}
		if rr != rel {
			target = filepath.Join(root, rr)
			if c.links == nil {
				c.links = map[string]string{}
			}
			c.links[target] = path
		}
		rel = rr
	}
	if c.places == nil {
		c.places = map[string][2]string{}
	}
	c.places[target] = [2]string{rootReal, rel}
	return target, nil
}

func (c *Ctx) read(scope Scope, path string) (data []byte, exists bool, real string, err error) {
	real, err = c.guard(scope, path)
	if err != nil {
		return nil, false, filepath.Clean(path), err
	}
	if e, ok := c.edits[real]; ok {
		if e.Remove {
			return nil, false, real, nil
		}
		return e.After, true, real, nil
	}
	data, err = os.ReadFile(real)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, real, nil
	}
	return data, err == nil, real, err
}

func (c *Ctx) stage(real string, mode fs.FileMode, after []byte, remove bool) error {
	e, ok := c.edits[real]
	if !ok {
		pl, ok := c.places[real]
		if !ok {
			return fmt.Errorf("%s was not checked before writing", real)
		}
		e = &Edit{Path: real, Root: pl[0], Rel: pl[1], Mode: mode}
		info, err := os.Stat(real)
		switch {
		case err == nil && info.Mode().IsRegular():
			before, rerr := os.ReadFile(real)
			if rerr != nil {
				return rerr
			}
			e.Before, e.Existed, e.Mode = before, true, info.Mode().Perm()
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return err
		case err == nil:
			return fmt.Errorf("%s is not a regular file", real)
		}
		if c.edits == nil {
			c.edits = map[string]*Edit{}
		}
		c.edits[real] = e
		c.order = append(c.order, real)
	}
	e.After, e.Remove = after, remove
	return nil
}

func (c *Ctx) write(real string, mode fs.FileMode, data []byte) error {
	return c.stage(real, mode, data, false)
}

func (c *Ctx) remove(real string) error { return c.stage(real, 0, nil, true) }

// resolvePath follows symlinks (dotfile managers commonly link agent
// configs) down to the real file, resolving the nearest existing ancestor
// when the file itself is absent.
func resolvePath(path string) string {
	path = filepath.Clean(path)
	var tail []string
	cur := path
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				r = filepath.Join(r, tail[i])
			}
			return r
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// Op is one managed mutation of a config file or file set.
type Op interface {
	Run(c *Ctx) ([]Item, error)
}

type meta struct {
	agent AgentID
	kind  Kind
	scope Scope
}

func (m meta) item(name, path, status, detail string) Item {
	return Item{Agent: m.agent, Kind: m.kind, Scope: m.scope, Name: name, Path: path, Status: status, Detail: detail}
}

// refused turns a refused path into a skipped item so the rest of the plan
// still runs.
func (m meta) refused(err error) ([]Item, bool) {
	var u *unsafePathError
	if !errors.As(err, &u) {
		return nil, false
	}
	return []Item{m.item("*", u.path, StatusSkipped, u.reason)}, true
}

// Plan is a computed set of file edits plus the per-entry report.
type Plan struct {
	Edits  []Edit
	Items  []Item
	ledger *Ledger
}

// Changed reports whether applying the plan would write anything.
func (p *Plan) Changed() bool { return len(p.Edits) > 0 || p.ledger.dirty }

func (c *Ctx) finish(items []Item) *Plan {
	for i, it := range items {
		if from, ok := c.links[it.Path]; ok {
			items[i].Detail = strings.TrimSpace(it.Detail + " (followed symlink " + from + ")")
		}
	}
	p := &Plan{Items: items, ledger: c.Ledger}
	for _, path := range c.order {
		e := c.edits[path]
		if e.Remove && !e.Existed {
			continue
		}
		if !e.Remove && e.Existed && bytes.Equal(e.Before, e.After) {
			continue
		}
		p.Edits = append(p.Edits, *e)
	}
	sort.SliceStable(p.Edits, func(i, j int) bool { return p.Edits[i].Path < p.Edits[j].Path })
	return p
}

const applyLockTimeout = 30 * time.Second

// Apply writes every edit in one transaction and then the ledger, so a
// failure rolls all touched files back to their previous bytes.
func (p *Plan) Apply(ctx context.Context) error {
	if len(p.Edits) == 0 {
		return p.ledger.Save()
	}
	keys := make([]string, 0, len(p.Edits))
	for _, e := range p.Edits {
		keys = append(keys, "agentsync:"+e.Path)
	}
	lockCtx, cancel := context.WithTimeout(ctx, applyLockTimeout)
	defer cancel()
	tx, err := txn.Begin(lockCtx, "agent sync", keys)
	if err != nil {
		return err
	}
	for _, e := range p.Edits {
		before := txn.Before{Data: e.Before, Existed: e.Existed}
		if e.Remove {
			err = tx.StageRemovalFrom(e.Root, e.Rel, before)
		} else {
			err = tx.StageFrom(e.Root, e.Rel, e.After, e.Mode, before)
		}
		if err != nil {
			return tx.Discard(err)
		}
	}
	if err := tx.Persist(); err != nil {
		return tx.Discard(err)
	}
	if err := tx.Apply(txn.VerifyNoSymlinks); err != nil {
		return tx.Discard(err)
	}
	return tx.CommitWith(ledgerFinalizer, p.ledger.ops, p.ledger.Save)
}

// Ledger records the content hash of every JSON entry relay wrote, keyed by
// real file path, so a later sync can tell an entry it may update or remove
// from one the user edited or created.
type Ledger struct {
	Version int                          `json:"version"`
	Files   map[string]map[string]string `json:"files"`

	path  string
	dirty bool
	// ops are this process's changes, replayed on the file's current
	// content when saving so a concurrent sync's entries are not lost.
	ops []ledgerOp
}

type ledgerOp struct {
	File string `json:"file"`
	ID   string `json:"id"`
	Hash string `json:"hash,omitempty"`
	Drop bool   `json:"drop,omitempty"`
}

// ledgerFinalizer names the ledger update a sync transaction owes, so
// recovery can replay it after a crash. Replaying is safe: every operation
// sets or drops an absolute value.
const ledgerFinalizer = "agentsync-ledger"

func init() {
	txn.RegisterFinalizer(ledgerFinalizer, func(raw json.RawMessage) error {
		var ops []ledgerOp
		if err := json.Unmarshal(raw, &ops); err != nil {
			return fmt.Errorf("decode the pending ledger update: %w", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		l := &Ledger{Version: 1, Files: map[string]map[string]string{}, path: LedgerPath(home), ops: ops, dirty: len(ops) > 0}
		return l.Save()
	})
}

func LedgerPath(home string) string {
	return filepath.Join(home, ".config", "relay", "sync-ledger.json")
}

func LoadLedger(home string) (*Ledger, error) { return readLedger(LedgerPath(home)) }

func readLedger(path string) (*Ledger, error) {
	l := &Ledger{Version: 1, Files: map[string]map[string]string{}, path: path}
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", l.path, err)
	}
	if l.Files == nil {
		l.Files = map[string]map[string]string{}
	}
	return l, nil
}

func (l *Ledger) entries(file string) map[string]string {
	m, ok := l.Files[file]
	if !ok {
		m = map[string]string{}
		l.Files[file] = m
	}
	return m
}

func (l *Ledger) set(file, id, hash string) {
	m := l.entries(file)
	if m[id] != hash {
		m[id] = hash
		l.dirty = true
		l.ops = append(l.ops, ledgerOp{File: file, ID: id, Hash: hash})
	}
}

func (l *Ledger) drop(file, id string) {
	m := l.entries(file)
	if _, ok := m[id]; ok {
		delete(m, id)
		l.dirty = true
		l.ops = append(l.ops, ledgerOp{File: file, ID: id, Drop: true})
	}
	if len(m) == 0 {
		delete(l.Files, file)
	}
}

func (l *Ledger) apply(op ledgerOp) {
	if op.Drop {
		delete(l.Files[op.File], op.ID)
	} else {
		if l.Files[op.File] == nil {
			l.Files[op.File] = map[string]string{}
		}
		l.Files[op.File][op.ID] = op.Hash
	}
	if len(l.Files[op.File]) == 0 {
		delete(l.Files, op.File)
	}
}

func (l *Ledger) Save() error {
	if !l.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	// Another sync may have saved since this one loaded the ledger: take the
	// lock, reread the file and replay only this process's changes on it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	locks, err := txn.AcquireAll(ctx, []string{"sync-ledger"})
	if err != nil {
		return fmt.Errorf("lock the sync ledger: %w", err)
	}
	defer locks.Unlock()

	fresh, err := readLedger(l.path)
	if err != nil {
		return err
	}
	for _, op := range l.ops {
		fresh.apply(op)
	}
	for file, entries := range fresh.Files {
		if len(entries) == 0 {
			delete(fresh.Files, file)
		}
	}
	raw, err := json.MarshalIndent(fresh, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), "sync-ledger-*.tmp")
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp.Name(), l.path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	l.Files, l.ops, l.dirty = fresh.Files, nil, false
	return nil
}
