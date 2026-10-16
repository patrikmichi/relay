# Sync routes

Saved routes tell a plain `relay sync` what to copy where. They are stored in
the `sync` key of `~/.config/relay/config.json`:

```json
{
  "sync": {
    "routes": [
      { "from": "claude", "to": ["cursor"], "kinds": ["skills", "subagents"] },
      { "from": "cursor", "to": ["codex"], "kinds": ["rules"] }
    ],
    "defaults": {
      "hooks": ["claude", "codex"]
    }
  }
}
```

Edit them with `relay sync routes add|list|remove`; the file is written under
a lock, so concurrent relay commands do not lose each other's changes.

## Routes

| Field | Meaning |
|---|---|
| `from` | Where the kinds are read. For `skills` and `subagents`, a provider (`relay providers`). For settings kinds, an agent (`claude`, `codex`, `cursor`, `opencode`, `gemini-cli`, `windsurf`, `cline`, `copilot`) or `relay`, the relay sync directory (also the meaning of leaving it out). |
| `to` | Agents that receive the kinds, or `all`. |
| `kinds` | `skills`, `subagents`, or settings: `mcp`, `env`, `hooks`, `rules`, `commands`. |

One route may mix skills and settings (`--from claude --kind skills,rules,mcp`).
`env` is relay's agent profile and is never copied between agents. A route
cannot copy an agent to itself, and routes that copy a kind in a circle
(claude to cursor, cursor back to claude) are refused when added and when
planned.

## Defaults

`defaults` maps a settings kind to the agents that receive it, from the relay
sync directory, when no route lists that kind.
`relay sync routes add --kind hooks,rules --to claude,codex` (without
`--from`) sets them.

## What a plain `relay sync` does

- With no saved routes or defaults, it syncs every kind to every detected agent.
- With saved settings, it plans each route, then the defaults grouped by target
  list, and syncs nothing else. Kinds nothing covers are named in a warning.
- `--to`, `--from` or `--kind` ignore the saved routes and run one ordinary
  sync.
- `--dry-run` prints a per-route summary followed by the diffs. `--json` adds a
  `routes` array.
- Two routes may not write the same file (for example `hooks` and `env` for
  Claude Code both live in `settings.json`); give them one route.
- The confirmation rules are unchanged: hooks, stdio MCP servers, project rules
  and project remote servers need a terminal confirmation or `--yes`, and the
  command is refused while `RELAY_AGENT` is set. Copied hooks and stdio servers
  are shown in full, like any other.

`relay doctor` reports each route whose agents are not installed on this
machine, or that no longer validates.

## Copying settings between agents

Reading an agent returns only what the user wrote there. Relay's own blocks
(`relay-managed` markers), files (marker line) and entries (the sync ledger)
are skipped, so a sync never feeds its own output back in as a source. Each
copy then goes through the normal writers, with the ledger, conflict handling
and confirmation. Anything that cannot be carried over is a warning in the
report, never dropped silently.

### What is read

| Kind | Agents readable | Source |
|---|---|---|
| rules | Claude Code, Codex, OpenCode, Gemini CLI, Copilot (project), Windsurf, Cursor (project), Cline | `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `.github/copilot-instructions.md`, `global_rules.md`, `.cursor/rules/*.mdc`, `.windsurf/rules`, `.clinerules` |
| commands | all but Cline | `.claude/commands`, `~/.codex/prompts`, `.cursor/commands`, OpenCode `commands`, Windsurf `workflows`, `.github/prompts`, `.gemini/commands/*.toml` |
| hooks | Claude Code, Codex, Gemini CLI, Cursor | the `hooks` key of `settings.json` / `hooks.json` |
| mcp | all | each agent's MCP config (Codex: `mcp_servers` in `config.toml`) |

### What is lossy

- **Rules in a single instructions file** (Claude Code, Codex, OpenCode,
  Gemini CLI, Copilot, Windsurf user) are read as one always-on rule named
  `<agent>-instructions` with no description or globs. Written to a file-per-rule
  agent (Cursor, Windsurf project, Cline) it keeps that shape; written to
  another single-file agent it becomes a section inside relay's block.
- **Rule scoping** survives only between Cursor, Windsurf project and Cline.
  Cursor `alwaysApply` and Windsurf `trigger: always_on` mean always on; Windsurf
  `model_decision` and `manual` read as not always on. Single-file targets
  print the globs as text and cannot enforce them.
- **Cursor commands** have no frontmatter, so a command copied to Cursor loses
  its description and argument hint. OpenCode and Windsurf keep the description
  only; Gemini keeps the description and turns `$ARGUMENTS` into `{{args}}`
  (and back when read). Subdirectory (namespaced) commands are not read.
  Codex prompts exist at user scope only and Copilot prompts at project scope
  only; Cline has no command format.
- **Hooks** copy only `command` hooks (prompt and agent hooks are reported).
  The event is mapped to relay's neutral names: Codex has no `notification`,
  Gemini no `subagent-stop`, Cursor no `notification`, OpenCode has six events
  and Windsurf expands a tool matcher into its per-action events; what an
  agent lacks is reported as unsupported. Gemini tool names are mapped
  (`run_shell_command` is `Bash`); timeouts are converted between seconds and
  milliseconds. Windsurf, OpenCode, Cline and Copilot hooks cannot be read.
- **MCP servers** keep the command, arguments, environment and URL. A literal
  `Authorization` header is never copied (the server is skipped); a bearer
  environment reference is. Environment references are translated to each
  target's syntax, and a server that uses one is skipped for Codex, Cline and
  Copilot, which do not expand them. A literal secret in a server's `env` is
  copied as written and flagged; use a `${VAR}` reference. Cline and Copilot
  remote servers get no auth header and must be authorized in the app.
  Windsurf, Cline and Copilot have one scope, and the gateway entry relay itself
  wrote is not read back.
