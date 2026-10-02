package agentport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/patrikmichi/relay/internal/agentport/txn"
)

// agentLockKey identifies the artifact an agent write mutates — see
// skillLockKey (engine.go) for why this must be the same identity
// Rollback/uninstall use.
func agentLockKey(provider ProviderID, scope Scope, name, projectRoot string) string {
	return fmt.Sprintf("agent:%s:%s:%s:%s", provider, scope, name, projectRoot)
}

type AgentPlan struct {
	Agent  *Agent
	Target AgentAdapter
	Scope  Scope
	Files  map[string][]byte // relative path -> content, from Target.Project() — always a single "<name>.md" entry (agents carry no resources)
	Loss   []LossItem

	TargetPaths string
}

func (p *AgentPlan) HasDropped() bool {
	for _, l := range p.Loss {
		if l.Kind == LossDropped {
			return true
		}
	}
	return false
}

// AgentTargetDir resolves the absolute directory an agent would be written to (or read back from) for the given target adapter + scope — the Agent-IR analogue of TargetDir.
func AgentTargetDir(target pathAdapter, scope Scope) (string, error) {
	if target == nil {
		return "", fmt.Errorf("nil target adapter")
	}
	dirs := target.UserDirs()
	if scope == ScopeProject {
		dirs = target.ProjectDirs()
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("provider %s has no directories for scope %s", target.ID(), scope)
	}

	base := dirs[0]
	if scope == ScopeProject {
		abs, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", base, err)
		}
		base = abs
	}
	return base, nil
}

// MigrateAgent projects src (already-canonical Agent IR) into target's
// on-disk format, producing a previewable AgentPlan. It does not write
// anything — the Agent-IR analogue of Migrate.
func MigrateAgent(src *Agent, target AgentAdapter, scope Scope) (*AgentPlan, error) {
	if src == nil {
		return nil, fmt.Errorf("source agent is nil")
	}
	if target == nil {
		return nil, fmt.Errorf("target adapter is nil")
	}
	if err := ValidateName(src.Name); err != nil {
		return nil, err
	}

	files, loss, err := target.Project(src)
	if err != nil {
		return nil, fmt.Errorf("project to %s: %w", target.ID(), err)
	}

	targetDir, err := AgentTargetDir(target, scope)
	if err != nil {
		return nil, err
	}

	return &AgentPlan{
		Agent:       src,
		Target:      target,
		Scope:       scope,
		Files:       files,
		Loss:        loss,
		TargetPaths: targetDir,
	}, nil
}

func WriteAgent(plan *AgentPlan) error {
	if err := PreflightWriteAgent(plan); err != nil {
		return err
	}

	projectRoot := ""
	if plan.Scope == ScopeProject {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve project root: %w", err)
		}
		projectRoot = cwd
	}
	lockKey := agentLockKey(plan.Target.ID(), plan.Scope, plan.Agent.Name, projectRoot)

	ctx, cancel := context.WithTimeout(context.Background(), txnLockAcquireTimeout)
	defer cancel()
	tx, err := txn.Begin(ctx, fmt.Sprintf("agent write %s -> %s", plan.Agent.Name, plan.Target.ID()), []string{lockKey})
	if err != nil {
		return err
	}

	if err := PreflightWriteAgent(plan); err != nil {
		return tx.Discard(err)
	}

	if err := os.MkdirAll(plan.TargetPaths, 0o755); err != nil {
		return tx.Discard(fmt.Errorf("create target dir %s: %w", plan.TargetPaths, err))
	}
	for rel, data := range plan.Files {
		if err := tx.Stage(plan.TargetPaths, rel, data, 0o644); err != nil {
			return tx.Discard(err)
		}
	}
	if err := tx.Persist(); err != nil {
		return tx.Discard(err)
	}
	if err := tx.Apply(verifyPathHasNoSymlinks); err != nil {
		return tx.Discard(err)
	}

	entry := ManifestEntry{
		Name:           plan.Agent.Name,
		Kind:           KindAgent,
		Provider:       plan.Target.ID(),
		Scope:          plan.Scope,
		SourceProvider: plan.Agent.Provenance.SourceProvider,
		TargetPaths:    HashFiles(plan.Files),
		Timestamp:      time.Now().UTC(),
		Provenance:     plan.Agent.Provenance,
		ProjectRoot:    projectRoot,
		TransactionID:  tx.ID(),
	}
	return tx.Commit(func() error { return RecordEntry(entry) })
}
