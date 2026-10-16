package agentsync

import (
	"errors"
	"fmt"
)

// Options selects what Build plans.
type Options struct {
	Agents []AgentID
	Scopes []Scope
	// Kinds limits the plan; empty means every file kind.
	Kinds []Kind
	// Force lets relay overwrite same-named entries it does not manage.
	Force bool
}

func runBuilder[T any](b builder[T], env Env, scope Scope, items []T) ([]Op, string) {
	if b == nil {
		return nil, "not supported by relay"
	}
	return b(env, scope, items)
}

// Build plans the sync of bundles (one per scope) into every selected
// agent. It reads files but writes nothing; call Plan.Apply to write.
func Build(env Env, bundles map[Scope]Bundle, opts Options) (*Plan, error) {
	kinds := opts.Kinds
	if len(kinds) == 0 {
		kinds = FileKinds
	}
	led, err := LoadLedger(env.Home)
	if err != nil {
		return nil, err
	}
	env = pinWindsurfDir(env, led)
	c := &Ctx{Env: env, Ledger: led, Force: opts.Force}

	var items []Item
	for _, scope := range opts.Scopes {
		if scope == ScopeProject && env.Project == "" {
			return nil, errors.New("project scope needs a project directory")
		}
		b := bundles[scope]
		for _, id := range opts.Agents {
			a, ok := agentByID(id)
			if !ok {
				return nil, fmt.Errorf("unknown agent %q", id)
			}
			for _, kind := range kinds {
				var ops []Op
				var why string
				switch kind {
				case KindMCP:
					ops, why = runBuilder(a.MCP, env, scope, b.MCP)
				case KindEnv:
					ops, why = runBuilder(a.Env, env, scope, b.Env)
				case KindHooks:
					ops, why = runBuilder(a.Hooks, env, scope, b.Hooks)
				case KindRules:
					ops, why = runBuilder(a.Rules, env, scope, b.Rules)
				case KindCommands:
					ops, why = runBuilder(a.Commands, env, scope, b.Commands)
				default:
					return nil, fmt.Errorf("kind %q is not written by agentsync", kind)
				}
				if why != "" {
					if b.count(kind) > 0 {
						items = append(items, Item{Agent: id, Kind: kind, Scope: scope, Name: "*", Status: StatusUnsupported, Detail: why})
					}
					continue
				}
				for _, op := range ops {
					got, err := op.Run(c)
					if err != nil {
						return nil, fmt.Errorf("%s %s (%s): %w", id, kind, scope, err)
					}
					items = append(items, got...)
				}
			}
		}
	}
	return c.finish(items), nil
}

// MatrixRow is one cell of the agent x artifact support matrix.
type MatrixRow struct {
	Agent     AgentID `json:"agent"`
	Kind      Kind    `json:"kind"`
	Scope     Scope   `json:"scope"`
	Supported bool    `json:"supported"`
	Reason    string  `json:"reason,omitempty"`
}

// Skill and subagent writers live in the agentport engine; these are the
// providers it carries them for.
var (
	skillAgents    = map[AgentID]bool{Claude: true, Codex: true, Cursor: true, OpenCode: true, Gemini: true, Windsurf: true, Cline: true}
	subagentAgents = map[AgentID]bool{Claude: true, Codex: true, Cursor: true, OpenCode: true, Gemini: true}
)

// Matrix reports what relay can write for every agent, kind and scope.
func Matrix(env Env) []MatrixRow {
	var rows []MatrixRow
	for _, a := range Agents() {
		for _, kind := range []Kind{KindMCP, KindEnv, KindHooks, KindRules, KindCommands, KindSkills, KindSubagents} {
			for _, scope := range []Scope{ScopeUser, ScopeProject} {
				row := MatrixRow{Agent: a.ID, Kind: kind, Scope: scope, Supported: true}
				switch kind {
				case KindSkills:
					if !skillAgents[a.ID] {
						row.Supported, row.Reason = false, "no skill directory published"
					}
				case KindSubagents:
					if !subagentAgents[a.ID] {
						row.Supported, row.Reason = false, "no subagent file format published"
					}
				default:
					if reason := Support(env, a.ID, kind, scope); reason != "" {
						row.Supported, row.Reason = false, reason
					}
				}
				rows = append(rows, row)
			}
		}
	}
	return rows
}
