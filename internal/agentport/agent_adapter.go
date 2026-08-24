package agentport

// AgentAdapter converts between one provider's on-disk agent format and the
// canonical Agent IR — the Agent-IR analogue of Adapter (adapter.go).
// Deliberately a SEPARATE interface (not a type parameter / generic
// Adapter[T]) per relay-standalone design §3b Option A: the byte-exact
// Skill path (config_adapter_parity_test.go) must never be put at risk by
// agent-only plumbing, and every Skill call site keeps using the exact
// Adapter/*Skill signatures it always has.
type AgentAdapter interface {
	// ID returns the provider identifier (claude|opencode today — Codex and
	// Cursor have no agent-file primitive; see relay-standalone design §3d).
	ID() ProviderID

	// UserDirs returns the per-user (global) directories this provider
	// searches for agent definitions, in priority order.
	UserDirs() []string

	// ProjectDirs returns the per-project directories this provider
	// searches for agent definitions, in priority order, relative to a
	// project root.
	ProjectDirs() []string

	// Detect reports whether this provider appears to be installed on this
	// machine — i.e. whether any of its UserDirs() exist on disk.
	Detect() bool

	// Load reads an agent from path — a flat `<name>.md` file (every
	// shipped agent provider is layout: flat; see agents/*.yml) — and
	// returns its canonical Agent IR.
	Load(path string) (*Agent, error)

	// Project serializes an Agent into this provider's on-disk file
	// layout. files is keyed by path relative to the agent's target
	// directory (a single "<name>.md" entry — agents carry no resources).
	// loss reports, per IR field the target format can't fully represent
	// or can only represent via a best-effort mapping/reshape, what
	// happened to it.
	Project(a *Agent) (files map[string][]byte, loss []LossItem, err error)

	// Capabilities declares which Agent IR extension fields this
	// provider's format can represent at all (see agent_caps.go).
	Capabilities() AgentCapSet

	// OwnUserDirCount reports how many LEADING entries of UserDirs() are
	// this provider's own writable directories — the Agent-IR analogue of
	// Adapter.OwnUserDirCount, used by the same "never touch a directory
	// the provider doesn't own" destructive-operation scoping. Every
	// shipped agent provider returns 1 (a single own user dir; no
	// legacy/compat agent directories exist yet).
	OwnUserDirCount() int

	// OwnProjectDirCount is the ProjectDirs() analogue of
	// OwnUserDirCount.
	OwnProjectDirCount() int
}
