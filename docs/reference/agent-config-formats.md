# Agent config formats

What `relay sync` and `relay mcp install` write for each coding agent, and why.
Formats were checked on 2026-10-16 against each vendor's documentation (linked
per agent). Skill and subagent formats are covered by
[agent formats](../agent-formats.md) and the provider files under
`internal/agentport/providers`.

Paths starting with `~` are the user scope; the others are relative to the
project directory (the current directory when `--scope project` is used).

## How entries are owned

| Where relay writes | Marker | Consequence |
|---|---|---|
| JSON object keys (`mcpServers`, `mcp`, `servers`) | hash ledger in `~/.config/relay/sync-ledger.json` | Updated or removed only while the on-disk value still matches what relay wrote |
| JSON hook arrays (`hooks.<Event>[]`) | hash ledger, per item | A hook you already wrote with the same matcher and command is not duplicated |
| Codex `config.toml` | `# >>> relay-managed:begin mcp >>>` ... `# <<< relay-managed:end mcp <<<` | A table you defined outside the block wins and is reported as a conflict |
| Shared Markdown (`CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, ...) | `<!-- relay-managed:begin rules -->` ... `<!-- relay-managed:end rules -->` | Text outside the block is never touched |
| Whole files (`.mdc`, command and workflow files, the OpenCode plugin) | `relay-managed` in the first kilobyte | A same-named file without the marker is yours and is left alone |

Files are edited in place: JSON keeps key order, indentation and unrelated
keys, and symlinked configs (dotfile managers) are written through the link.
JSON with comments (`opencode.jsonc`-style) cannot be rewritten safely, so the
file is skipped and reported.

Remote MCP entries never contain a credential. Where the agent can read an
environment variable in a header or a token field relay writes the reference
(for example `Bearer ${GATEWAY_API_KEY}`); otherwise it writes none and the
agent signs in with OAuth.

## Claude Code

Docs: <https://code.claude.com/docs>

| Kind | User | Project |
|---|---|---|
| MCP | `~/.claude.json` `mcpServers` | `.mcp.json` `mcpServers` |
| Hooks | `~/.claude/settings.json` `hooks` | `.claude/settings.json` `hooks` |
| Rules | block in `~/.claude/CLAUDE.md` | block in `CLAUDE.md` |
| Commands | `~/.claude/commands/<name>.md` | `.claude/commands/<name>.md` |

- Remote servers: `{"type": "http", "url": ..., "headers": {"Authorization": "Bearer ${VAR}"}}`; local: `{"type": "stdio", "command", "args", "env"}`.
- Hook groups: `{"matcher", "hooks": [{"type": "command", "command", "timeout"}]}` under `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, `SubagentStop`, `PreCompact`, `Notification`.
- Commands: frontmatter `description`, `argument-hint`; `$ARGUMENTS` in the body.

## Codex

Docs: <https://learn.chatgpt.com/docs/hooks>, <https://learn.chatgpt.com/docs/custom-prompts>

| Kind | User | Project |
|---|---|---|
| MCP | `~/.codex/config.toml` `[mcp_servers.<name>]` | `.codex/config.toml` |
| Hooks | `~/.codex/hooks.json` | `.codex/hooks.json` (loaded only when the project's `.codex/` layer is trusted) |
| Rules | block in `~/.codex/AGENTS.md` | block in `AGENTS.md` |
| Commands | `~/.codex/prompts/<name>.md` | unsupported (prompts are read from the user directory only) |

- Remote servers: `url` plus `bearer_token_env_var`; local: `command`, `args`, `[mcp_servers.<name>.env]`.
- Hooks use the same group shape as Claude Code, without `Notification`. Codex asks you to review new hooks with `/hooks` before they run.
- Custom prompts are deprecated upstream in favour of skills; relay still writes them because they are the only command primitive Codex has.

## Cursor

Docs: <https://cursor.com/docs/hooks>

| Kind | User | Project |
|---|---|---|
| MCP | `~/.cursor/mcp.json` `mcpServers` | `.cursor/mcp.json` |
| Hooks | `~/.cursor/hooks.json` | `.cursor/hooks.json` |
| Rules | unsupported (user rules live in Settings) | `.cursor/rules/<name>.mdc` |
| Commands | `~/.cursor/commands/<name>.md` | `.cursor/commands/<name>.md` |

- Remote servers: `{"url", "headers": {"Authorization": "Bearer ${env:VAR}"}}`.
- Hooks: `{"version": 1, "hooks": {"<event>": [{"command", "timeout", "matcher"}]}}` with `sessionStart`, `sessionEnd`, `beforeSubmitPrompt`, `preToolUse`, `postToolUse`, `stop`, `subagentStop`, `preCompact`; timeout in seconds.
- Rules: frontmatter `description`, `globs` (comma separated), `alwaysApply`.

## OpenCode

Docs: <https://opencode.ai/docs/mcp-servers/>, <https://opencode.ai/docs/commands/>, <https://opencode.ai/docs/plugins/>

| Kind | User | Project |
|---|---|---|
| MCP | `~/.config/opencode/opencode.json` `mcp` | `opencode.json` |
| Hooks | `~/.config/opencode/plugins/relay-hooks.js` | `.opencode/plugins/relay-hooks.js` |
| Rules | block in `~/.config/opencode/AGENTS.md` | block in `AGENTS.md` |
| Commands | `~/.config/opencode/commands/<name>.md` | `.opencode/commands/<name>.md` |

- Remote servers: `{"type": "remote", "url", "enabled": true, "headers": {"Authorization": "Bearer {env:VAR}"}}`; local: `{"type": "local", "command": [cmd, args...], "enabled": true, "environment": {}}`. New files get the `$schema` key.
- OpenCode has no declarative hook config, so relay generates one plugin that runs the configured commands with the event payload as JSON on stdin; exit code 2 from a `pre-tool-use` command blocks the tool call. It maps `pre-tool-use` to `tool.execute.before`, `post-tool-use` to `tool.execute.after`, `pre-compact` to `experimental.session.compacting`, and `session-start`, `stop`, `session-end` to the `session.created`, `session.idle`, `session.deleted` events. `user-prompt-submit`, `subagent-stop` and `notification` have no plugin hook and are reported as unsupported. Matchers are case-insensitive regexes because OpenCode tool names are lowercase.

## Gemini CLI

Docs: <https://geminicli.com/docs/hooks/>, <https://geminicli.com/docs/tools/mcp-server/>, <https://geminicli.com/docs/cli/custom-commands/>

| Kind | User | Project |
|---|---|---|
| MCP | `~/.gemini/settings.json` `mcpServers` | `.gemini/settings.json` |
| Hooks | `~/.gemini/settings.json` `hooks` | `.gemini/settings.json` |
| Rules | block in `~/.gemini/GEMINI.md` | block in `GEMINI.md` |
| Commands | `~/.gemini/commands/<name>.toml` | `.gemini/commands/<name>.toml` |

- Remote servers use `httpUrl` (streamable HTTP) with `headers`.
- Hook events: `SessionStart`, `SessionEnd`, `BeforeAgent` (user-prompt-submit), `BeforeTool`, `AfterTool`, `AfterAgent` (stop), `PreCompress`, `Notification`; `timeout` is in milliseconds and each hook carries `"name": "relay"`. `subagent-stop` is unsupported. The neutral matchers `Bash`, `Edit`, `MultiEdit`, `Write` and `Read` are translated to Gemini's tool names; other matchers pass through.
- Commands: `description` and a multi-line `prompt`; `$ARGUMENTS` becomes `{{args}}`.

## Windsurf

Docs: <https://docs.devin.ai/desktop/cascade/hooks>, <https://docs.devin.ai/desktop/cascade/mcp>, <https://docs.devin.ai/desktop/cascade/memories>

| Kind | User | Project |
|---|---|---|
| MCP | `~/.codeium/windsurf/mcp_config.json` `mcpServers` | unsupported (global config only) |
| Hooks | `~/.codeium/windsurf/hooks.json` | `.windsurf/hooks.json` (`.devin/hooks.json` when only `.devin/` exists) |
| Rules | block in `~/.codeium/windsurf/memories/global_rules.md` | `.windsurf/rules/<name>.md` (`.devin/rules/` when only `.devin/` exists) |
| Commands | `~/.codeium/windsurf/global_workflows/<name>.md` | `.windsurf/workflows/<name>.md` |

- Remote servers: `{"serverUrl", "headers": {"Authorization": "Bearer ${env:VAR}"}}`. The product is being renamed Devin Desktop and its docs list `~/.config/devin/mcp_config.json`; relay writes the `~/.codeium/windsurf` path that Windsurf installs have used.
- Hooks are per action: `pre-tool-use` and `post-tool-use` expand by matcher (`Bash` is `*_run_command`, `Read`/`Grep`/`Glob` are `*_read_code`, `Write`/`Edit`/`MultiEdit` are `*_write_code`, `mcp__*` is `*_mcp_tool_use`; no matcher covers all four); `user-prompt-submit` is `pre_user_prompt`; `stop` is `post_cascade_response`. Other events, and matchers with no Windsurf action, are reported as unsupported. Hook entries are `{"command", "show_output": false}`.
- Project rules use frontmatter `trigger` (`always_on`, `glob`, `model_decision`, `manual`), `description`, `globs`. Global rules are limited to 6,000 characters; relay warns when the block is longer.

## Cline

Docs: <https://docs.cline.bot/mcp/mcp-overview.md>, <https://docs.cline.bot/customization/cline-rules.md>

| Kind | User | Project |
|---|---|---|
| MCP | `~/.cline/data/settings/cline_mcp_settings.json` (also the VS Code extension's `cline_mcp_settings.json` when present; `CLINE_MCP_SETTINGS_PATH` overrides) | unsupported |
| Hooks | unsupported | unsupported |
| Rules | `~/Documents/Cline/Rules/<name>.md` | `.clinerules/<name>.md` |
| Commands (workflows) | `~/Documents/Cline/Workflows/<name>.md` | `.clinerules/workflows/<name>.md` |

- Remote servers: `{"type": "streamableHttp", "url", "disabled": false}`. Cline does not read environment variables in settings, so no `Authorization` header is written and the sync report says to authenticate the server in Cline.
- Hooks are SDK plugins with no declarative config file.
- A `.clinerules` that is a single file is left alone and rules and workflows are reported as unsupported until it becomes a directory.
- Rules with globs get a `paths:` frontmatter list. The workflow locations were not re-verified against Cline's docs in this pass.
- Cline subagents are a built-in tool (`spawn_agent`), not user-defined files, so subagent sync is unsupported.

## GitHub Copilot

Project scope only.

| Kind | Project |
|---|---|
| MCP | `.vscode/mcp.json` `servers` (`{"type": "http", "url", "headers": {"Authorization": "Bearer ${env:VAR}"}}`) |
| Rules | block in `.github/copilot-instructions.md` |
| Commands | `.github/prompts/<name>.prompt.md` (frontmatter `description`, `mode: agent`) |
| Hooks, skills, subagents | unsupported |

Copilot is detected only when the project already has
`.github/copilot-instructions.md` or `.vscode/mcp.json`; name it with
`--to copilot` otherwise.

## Neutral source formats

`hooks.json`:

```json
{
  "hooks": [
    { "event": "pre-tool-use", "matcher": "Bash", "command": "~/bin/guard.sh", "timeout": 10, "agents": ["claude", "codex"] }
  ]
}
```

Events: `session-start`, `session-end`, `user-prompt-submit`, `pre-tool-use`,
`post-tool-use`, `stop`, `subagent-stop`, `pre-compact`, `notification`.
`timeout` is in seconds; `agents` limits the hook to the listed agents.
Matchers are written for Claude Code's tool names and passed through unchanged
except where noted above.

`rules/<name>.md` and `commands/<name>.md` take optional YAML frontmatter:

| Field | Applies to | Meaning |
|---|---|---|
| `description` | both | one-line summary |
| `globs` | rules | comma separated string or list; the rule applies to matching files where the agent can scope it |
| `always` | rules | `false` makes a rule without globs opt-in |
| `argument-hint` | commands | shown after the command name |

Names must match `[A-Za-z0-9][A-Za-z0-9._-]*`.
