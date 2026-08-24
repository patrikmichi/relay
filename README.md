# relay

`relay` is a CLI for managing AI agent skills (Claude, Codex, OpenCode,
Cursor) and agent definitions (Claude, opencode) across providers, and, when
you're connected to a gateway, for pulling skills/agents/MCP servers from a
shared catalog. Every command works fully offline against your local
machine; a handful of catalog commands need a gateway to reach.

## Install

**Homebrew (macOS/Linux):**

```bash
brew install patrikmichi/tap/relay
```

**Go install:**

```bash
go install github.com/patrikmichi/relay/cmd/relay@latest
```

Both install a single static `relay` binary — no runtime dependencies.

## Quickstart

```bash
# Authenticate against a gateway (only needed for catalog commands below).
relay login

# Fully offline: migrate a skill you already have installed for one
# provider so it's also available to another.
relay skill migrate my-skill --from claude --to codex

# Requires a gateway: install a skill by catalog id/slug.
relay skill install pr-triage --to claude
```

```bash
# Fully offline: migrate an installed agent from one provider to another.
relay agent migrate reviewer --from claude --to opencode
```

> MCP-server management verbs (`relay mcp ...` beyond `publish`) are not
> yet part of this CLI.

## Agent provider support

`relay agent migrate/list/diff/scan/uninstall/rollback` work offline against
two providers:

| Provider | Shape | Support |
|---|---|---|
| **Claude Code** | flat `<name>.md` (frontmatter + body) at `~/.claude/agents/` (user) / `.claude/agents/` (project) | **Supported** |
| **opencode** | flat `<name>.md` (frontmatter + body) at `~/.config/opencode/agent/` (user) / `.opencode/agent/` (project); `tools` is reshaped to/from opencode's `{tool: bool}` map | **Supported** |
| **Codex** | a `[profiles.*]` TOML block in `~/.codex/config.toml` plus a decoupled prompt file in `~/.codex/prompts/*.md` — no single agent-definition file | **Unsupported** — a TOML profile is not equivalent to a markdown agent |
| **Cursor** | rules (`.cursor/rules/*.mdc`) and settings-defined "modes" | **Unsupported** — Cursor has no first-class subagent primitive |

Migrating between claude and opencode preserves Name/Description/Body/Model
(alias-mapped)/Tools (reshaped); Claude's `memory`/`skills` and opencode's
`temperature`/`mode` have no equivalent on the other side and are reported
as dropped in the fidelity report. Skill management (`relay skill ...`)
supports all 4 providers (Claude, Codex, OpenCode, Cursor).

## The offline-vs-gateway model

Every `relay` command falls into exactly one of two buckets:

| | LOCAL | GATEWAY |
|---|---|---|
| Touches | Only files on this machine (`~/.claude/skills/`, `~/.claude/agents/`, `~/.codex/`, `~/.cursor/`, `~/.config/opencode/`, and relay's own manifest) | A configured gateway over HTTP |
| Needs auth | No | Yes — `relay login` or `GATEWAY_API_KEY` |
| Examples | `relay skill migrate`, `relay skill install <path>`, `relay skill list`, `relay skill diff`, `relay skill scan`, `relay skill uninstall`, `relay skill rollback`, `relay agent migrate`, `relay agent list`, `relay agent diff`, `relay agent scan`, `relay agent uninstall`, `relay agent rollback`, `relay providers` | `relay skill install <catalog-id>`, `relay skill search`, `relay publish`, `relay sync`, `relay services`, `relay call`, `relay help-tools`, `relay login`/`logout`/`whoami`/`authorize`/`tokens` |

**Fail-closed guidance.** If a GATEWAY command can't resolve a gateway URL —
none configured, `--offline` passed, or you're not authenticated — it refuses
to run rather than guessing, and prints:

```
no gateway configured. Run `relay config set-gateway <url>` then `relay login`
to install from the catalog. Local `relay skill install <path>` and
`relay skill migrate` work offline.
```

`--offline` is a root flag (`relay --offline <command>`) that forces this
fail-closed behavior even if a gateway is otherwise configured — useful for
scripts/CI that must never make a network call.

## Command reference

| Command | Kind | Description |
|---|---|---|
| `relay login [--device]` | gateway | Authenticate via Google OAuth (browser or device-code flow) |
| `relay logout` | gateway | Revoke the current session and remove the stored token |
| `relay whoami [--full]` | gateway | Show the current authenticated identity |
| `relay authorize <service>` | gateway | Authorize a specific service (e.g. Google Workspace scopes) |
| `relay tokens list` | gateway | Show active session info |
| `relay tokens revoke` | gateway | Revoke the current session token |
| `relay config set-gateway <url>` | local | Persist a gateway URL to `~/.config/relay/config.json` |
| `relay config get-gateway` | local | Print the effective gateway URL |
| `relay config show` | local | Print all resolved config as JSON |
| `relay services` | gateway | List services available on the configured gateway |
| `relay help-tools [service]` | gateway | List tools for one or all services |
| `relay call <service> <tool> [--arg k=v]` | gateway | Call a tool on a gateway service |
| `relay sync [--dir] [--dry-run]` | gateway | Pull your marketplace manifest into a local Claude Code plugin directory |
| `relay publish <path> [--type] [--watch]` | gateway | Publish a skill/agent/MCP server/prompt/plugin to the catalog |
| `relay publish status <versionId> [--watch]` | gateway | Poll a publish's review status |
| `relay skill publish <path>` | gateway | Alias for `relay publish` scoped to skills |
| `relay agent publish <path>` | gateway | Publish an agent definition |
| `relay mcp publish [--descriptor]` | gateway | Publish an MCP server descriptor |
| `relay skill install <path>` | local | Install a skill from a local directory/file into one or more providers |
| `relay skill install <catalog-id>` | gateway | Install a skill by catalog id/slug into one or more providers |
| `relay skill search [query]` | gateway | Search the catalog for installable skills |
| `relay skill migrate <name> --from <p> [--to <p>...]` | local | Project an installed skill from one provider to another |
| `relay skill list` | local | List installed skills across providers |
| `relay skill diff <name>` | local | Diff a skill's projection across providers |
| `relay skill scan <name>` / `relay skill score <name>` | local | Inspect a skill's manifest/fidelity |
| `relay skill uninstall <name>` | local | Remove an installed skill |
| `relay skill rollback [manifest-entry-id]` | local | Revert to a prior manifest entry |
| `relay agent migrate <name> --from <p> [--to <p>...]` | local | Project an installed agent from one provider to another (claude, opencode) |
| `relay agent list` | local | List installed agents across agent providers |
| `relay agent diff <name>` | local | Diff an agent's projection across providers |
| `relay agent scan <name>` | local | Inspect an agent for dangerous shell patterns/hardcoded secrets |
| `relay agent uninstall <name>` | local | Remove an installed agent |
| `relay agent rollback [manifest-entry-id]` | local | Revert to a prior agent manifest entry |
| `relay providers` | local | List supported providers and their detection status |
| `--offline` (root flag) | — | Force every gateway command in this invocation to fail closed |
| `--version` | — | Print the CLI version, commit, and build date |

Run `relay <command> --help` for full flag documentation on any command.

## Configuration & environment

Persistent config lives at `~/.config/relay/config.json` (migrated
automatically from the older `~/.config/gw/` if present). Manage it with
`relay config set-gateway` / `get-gateway` / `show`, or override per-invocation
with environment variables — env always wins over the config file:

| Variable | Purpose |
|---|---|
| `GATEWAY_URL` | Gateway base URL (overrides config file and any compiled-in default) |
| `GATEWAY_API_KEY` | Non-interactive bearer auth — use in scripts/CI instead of `relay login` |
| `RELAY_EMAIL` | Selects which keychain-stored session to use (overrides the last `relay login`'d identity) |

Interactive sessions authenticate via `relay login` (OAuth, tokens stored in
the OS keychain, auto-refreshing). Non-interactive contexts should set
`GATEWAY_URL` + `GATEWAY_API_KEY` instead.

## License

[Apache-2.0](LICENSE)
