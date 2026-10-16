# Sync routes

Saved routes tell a plain `relay sync` what to copy where. They are stored in
the `sync` key of `~/.config/relay/config.json`:

```json
{
  "sync": {
    "routes": [
      { "from": "claude", "to": ["cursor"], "kinds": ["skills", "subagents"] }
    ],
    "defaults": {
      "hooks": ["claude", "codex"],
      "rules": ["claude", "codex"]
    }
  }
}
```

Edit them with `relay sync routes add|list|remove`; the file is written under
a lock, so concurrent relay commands do not lose each other's changes.

## Routes

| Field | Meaning |
|---|---|
| `from` | For `skills` and `subagents`: the provider to copy from (`relay providers`). For settings kinds: `relay`, the relay sync directory (or leave it out). |
| `to` | Agents that receive the kinds (`claude`, `codex`, `cursor`, `opencode`, `gemini-cli`, `windsurf`, `cline`, `copilot`, or `all`). |
| `kinds` | `skills`, `subagents`, or settings: `mcp`, `env`, `hooks`, `rules`, `commands`. |

A route copies either skills and subagents or settings, not both. Rules, hooks,
commands and MCP servers are not read back from another agent's files; they
come from `~/.config/relay/sync` (and `<project>/.relay`), so a route cannot
name an agent as the source of a settings kind.

## Defaults

`defaults` maps a settings kind to the agents that receive it when no route
lists that kind. `relay sync routes add --kind hooks,rules --to claude,codex`
(without `--from`) sets them.

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
  command is refused while `RELAY_AGENT` is set.

`relay doctor` reports each route whose agents are not installed on this
machine, or that no longer validates.
