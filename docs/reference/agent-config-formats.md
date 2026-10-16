# Agent config formats

What `relay sync` and `relay mcp install` write for each coding agent, and why.
Every path and key below was re-checked against the vendor documentation
linked in the agent's "Sources" line on 2026-10-16. Where the docs do not say,
the entry is marked as undocumented instead of guessed. Skill and subagent
formats are covered by [agent formats](../agent-formats.md) and the provider
files under `internal/agentport/providers`.

Paths starting with `~` are the user scope; the others are relative to the
project directory (the current directory when `--scope project` is used).

## How entries are owned

| Where relay writes | Marker | Consequence |
|---|---|---|
| JSON object keys (`mcpServers`, `mcp`, `servers`) | hash ledger in `~/.config/relay/sync-ledger.json` | Updated or removed only while the on-disk value still matches what relay wrote |
| JSON hook arrays (`hooks.<Event>[]`) | hash ledger, per item | A hook you already wrote with the same matcher and command is not duplicated |
| Codex `config.toml` | `# >>> relay-managed:begin mcp >>>` ... `# <<< relay-managed:end mcp <<<` | A table you defined outside the block wins and is reported as a conflict |
| Codex `config.toml`, environment | `# >>> relay-managed:begin env >>>` ... `# <<< relay-managed:end env <<<` | A `[shell_environment_policy.set]` table you defined outside the block wins and is reported as a conflict |
| Claude Code `settings.json` `env` | hash ledger, per variable | A variable you set to another value is a conflict; others are untouched |
| Shared Markdown (`CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, ...) | `<!-- relay-managed:begin rules -->` ... `<!-- relay-managed:end rules -->` | Text outside the block is never touched |
| Whole files (`.mdc`, command and workflow files, the OpenCode plugin) | `relay-managed` in the first kilobyte | A same-named file without the marker is yours and is left alone |
| Skills and subagents | the agentport manifest (`relay history`) | Unmodified relay installs are updated when the source changes; user-owned or edited ones are conflicts until `--force` |

Files are edited in place: JSON keeps key order, indentation and unrelated
keys, and symlinked configs (dotfile managers) are written through the link.
JSON with comments cannot be rewritten safely, so the file is skipped and
reported.

Remote MCP entries never contain a credential. Where the agent documents
environment-variable references in headers or token fields relay writes the
reference (for example `Bearer ${GATEWAY_API_KEY}`) only when asked with
`--mcp-auth api-key`; otherwise it writes none and the agent signs in with
OAuth, which the gateway marks as an agent's.

## Claude Code

Sources: [MCP](https://code.claude.com/docs/en/mcp), [settings](https://code.claude.com/docs/en/settings), [hooks](https://code.claude.com/docs/en/hooks), [memory](https://code.claude.com/docs/en/memory), [slash commands](https://code.claude.com/docs/en/slash-commands)

| Kind | User | Project |
|---|---|---|
| MCP | `~/.claude.json` top-level `mcpServers` | `.mcp.json` `mcpServers` |
| Environment | `~/.claude/settings.json` `env` | unsupported (set in the user settings) |
| Hooks | `~/.claude/settings.json` `hooks` | `.claude/settings.json` `hooks` |
| Rules | block in `~/.claude/CLAUDE.md` | block in `CLAUDE.md` (`./CLAUDE.md` or `./.claude/CLAUDE.md` are both read) |
| Commands | `~/.claude/commands/<name>.md` | `.claude/commands/<name>.md` |

- Confirmed: user and project MCP locations; `${VAR}` expansion in `url` and `headers` (not for Claude's own credential variables such as `ANTHROPIC_AUTH_TOKEN`, which expand to nothing); hooks in the two settings files with `matcher` and a `hooks` array, timeout in seconds; both CLAUDE.md locations.
- Commands: the docs say custom commands "have been merged into skills" and recommend skills for new work. `.claude/commands/<name>.md` keeps working with `description` and `argument-hint` frontmatter and `$ARGUMENTS`, but the user-level `~/.claude/commands/` directory is not documented on that page; relay writes it because older and current Claude Code builds load it.
- Remote servers: `{"type": "http", "url", "headers"}`; local: `{"type": "stdio", "command", "args", "env"}`.
- Environment: the `env` object of a settings file is applied to every session. relay writes `RELAY_AGENT` to the user file only, since a project file is shared with a team.

## Codex

Sources: [hooks](https://learn.chatgpt.com/docs/hooks), [MCP](https://learn.chatgpt.com/docs/extend/mcp.md), [config files](https://learn.chatgpt.com/docs/config-file/config-basic.md), [advanced config](https://learn.chatgpt.com/docs/config-file/config-advanced.md), [AGENTS.md](https://learn.chatgpt.com/docs/agent-configuration/agents-md), [custom prompts](https://learn.chatgpt.com/docs/custom-prompts)

| Kind | User | Project |
|---|---|---|
| MCP | `~/.codex/config.toml` `[mcp_servers.<name>]` | `.codex/config.toml` (trusted projects only) |
| Environment | `~/.codex/config.toml` `[shell_environment_policy.set]` | unsupported (set in the user config) |
| Hooks | `~/.codex/hooks.json` | `.codex/hooks.json` (trusted projects only) |
| Rules | block in `~/.codex/AGENTS.md` | block in `AGENTS.md` |
| Commands | `~/.codex/prompts/<name>.md` | unsupported (prompts are read from the Codex home only) |

- Confirmed: HTTP servers take `url`, `bearer_token_env_var`, `http_headers`, `env_http_headers`; stdio servers take `command`, `args`, `env`. Hook events and the group shape match Claude Code, without `Notification`; timeouts are seconds. New hooks must be reviewed with `/hooks` before they run.
- Environment: `shell_environment_policy.set` passes variables to the commands Codex runs. It applies after the exclusion rules, but an include-pattern allowlist can still remove the value. relay writes it as a `[shell_environment_policy.set]` table, which is the same setting as the inline `set = { ... }` form; an existing definition of either form is reported as a conflict.
- A global `~/.codex/AGENTS.override.md`, when present, is read instead of `AGENTS.md`; relay still writes `AGENTS.md`, so remove the override file if the synced rules do not show up.
- Custom prompts are deprecated upstream in favour of skills; relay still writes them because they are the only command primitive Codex has.

## Cursor

Sources: [MCP](https://cursor.com/docs/context/mcp), [rules](https://cursor.com/docs/context/rules), [hooks](https://cursor.com/docs/hooks)

| Kind | User | Project |
|---|---|---|
| MCP | `~/.cursor/mcp.json` `mcpServers` | `.cursor/mcp.json` |
| Hooks | `~/.cursor/hooks.json` | `.cursor/hooks.json` |
| Rules | unsupported (user rules live in Settings > Rules) | `.cursor/rules/<name>.mdc` |
| Commands | `~/.cursor/commands/<name>.md` | `.cursor/commands/<name>.md` |

- Confirmed: both MCP files; `${env:NAME}` in `command`, `args`, `env`, `url`, `headers`; `.mdc` rules with `description`, `globs` (comma separated) and `alwaysApply`; hooks `{"version": 1, "hooks": {...}}` with `sessionStart`, `sessionEnd`, `beforeSubmitPrompt`, `preToolUse`, `postToolUse`, `stop`, `subagentStop`, `preCompact`, timeout in seconds.
- Commands: the Cursor docs fetched describe only the migration of "user-level and workspace-level" slash commands to skills and do not state the directory or format. `.cursor/commands/*.md` as plain Markdown is relay's existing assumption and is not confirmed by the current docs.

## OpenCode

Sources: [MCP servers](https://opencode.ai/docs/mcp-servers/), [config](https://opencode.ai/docs/config/), [commands](https://opencode.ai/docs/commands/), [rules](https://opencode.ai/docs/rules/), [plugins](https://opencode.ai/docs/plugins/)

| Kind | User | Project |
|---|---|---|
| MCP | `~/.config/opencode/opencode.json` `mcp` | `opencode.json` |
| Hooks | `~/.config/opencode/plugins/relay-hooks.js` | `.opencode/plugins/relay-hooks.js` |
| Rules | block in `~/.config/opencode/AGENTS.md` | block in `AGENTS.md` |
| Commands | `~/.config/opencode/commands/<name>.md` | `.opencode/commands/<name>.md` |

- Confirmed: all paths above; remote `{"type": "remote", "url", "enabled", "headers"}` with `{env:NAME}`; local `{"type": "local", "command": [...], "environment"}`; command frontmatter `description`, `agent`, `model`; `$ARGUMENTS`.
- OpenCode has no declarative hook config, so relay generates one plugin that runs the configured commands with the event payload as JSON on stdin; exit code 2 from a `pre-tool-use` command blocks the tool call. It maps `pre-tool-use` to `tool.execute.before`, `post-tool-use` to `tool.execute.after`, `pre-compact` to `experimental.session.compacting`, and `session-start`, `stop`, `session-end` to the `session.created`, `session.idle`, `session.deleted` events. `user-prompt-submit`, `subagent-stop` and `notification` have no documented plugin hook and are reported as unsupported. Matchers are case-insensitive regexes because OpenCode tool names are lowercase.

## Gemini CLI

Sources: [hooks](https://geminicli.com/docs/hooks/), [MCP servers](https://geminicli.com/docs/tools/mcp-server/), [GEMINI.md](https://geminicli.com/docs/cli/gemini-md/), [custom commands](https://geminicli.com/docs/cli/custom-commands/)

| Kind | User | Project |
|---|---|---|
| MCP | `~/.gemini/settings.json` `mcpServers` | `.gemini/settings.json` |
| Hooks | `~/.gemini/settings.json` `hooks` | `.gemini/settings.json` |
| Rules | block in `~/.gemini/GEMINI.md` | block in `GEMINI.md` |
| Commands | `~/.gemini/commands/<name>.toml` | `.gemini/commands/<name>.toml` |

- Confirmed: settings locations; remote servers use `httpUrl` (streamable HTTP) with `headers`; hook events `SessionStart`, `SessionEnd`, `BeforeAgent`, `AfterAgent`, `BeforeTool`, `AfterTool`, `PreCompress`, `Notification` with `timeout` in milliseconds and an optional `name`; command TOML with `prompt` (required), `description` and `{{args}}`.
- The MCP page documents `$VAR` / `${VAR}` interpolation for `env` values only, not for `headers`, so a `Bearer ${GATEWAY_API_KEY}` header may be sent literally. Prefer `--mcp-auth oauth` for Gemini CLI.
- `subagent-stop` is unsupported. The neutral matchers `Bash`, `Edit`, `MultiEdit`, `Write` and `Read` are translated to Gemini's tool names; other matchers pass through.

## Windsurf (Devin Desktop)

Sources: [MCP](https://docs.devin.ai/desktop/cascade/mcp), [hooks](https://docs.devin.ai/desktop/cascade/hooks), [rules and memories](https://docs.devin.ai/desktop/cascade/memories), [workflows](https://docs.devin.ai/desktop/cascade/workflows). `docs.windsurf.com` redirects to these pages.

| Kind | User | Project |
|---|---|---|
| MCP | `$XDG_CONFIG_HOME/devin/mcp_config.json` (default `~/.config/devin/mcp_config.json`), plus `~/.codeium/windsurf/mcp_config.json` when `~/.codeium/windsurf/` exists | unsupported (global config only) |
| Hooks | `~/.codeium/windsurf/hooks.json` | `.devin/hooks.json`, else `.windsurf/hooks.json` |
| Rules | block in `~/.codeium/windsurf/memories/global_rules.md` | `.devin/rules/<name>.md`, else `.windsurf/rules/<name>.md` |
| Commands (workflows) | `~/.codeium/windsurf/global_workflows/<name>.md` | `.devin/workflows/<name>.md`, else `.windsurf/workflows/<name>.md` |

- MCP path: the current docs list only `~/.config/devin/mcp_config.json` (and `%APPDATA%\devin` on Windows) for the legacy Cascade agent and never mention `~/.codeium/windsurf/mcp_config.json`. relay writes the documented path always, and the older path as well when that directory exists, so either generation of the app picks the servers up. Devin Local keeps MCP servers in the Devin CLI config, which relay does not manage.
- Project directory: `.devin/` is preferred and `.windsurf/` is the documented fallback; relay uses `.devin/` when the project has it.
- Remote servers: `{"serverUrl", "headers": {"Authorization": "Bearer ${env:VAR}"}}`; `${env:VAR}` works in `command`, `args`, `env`, `serverUrl`, `url`, `headers`.
- Hooks are per action: `pre-tool-use` and `post-tool-use` expand by matcher (`Bash` is `*_run_command`, `Read`/`Grep`/`Glob` are `*_read_code`, `Write`/`Edit`/`MultiEdit` are `*_write_code`, `mcp__*` is `*_mcp_tool_use`; no matcher covers all four); `user-prompt-submit` is `pre_user_prompt`; `stop` is `post_cascade_response`. Other events, and matchers with no Windsurf action, are reported as unsupported. Entries are `{"command", "show_output": false}`.
- Project rules use frontmatter `trigger` (`always_on`, `glob`, `model_decision`, `manual`), `description`, `globs`. Global rules are limited to 6,000 characters (12,000 per workspace rule or workflow); relay warns when the global block is longer. Workflow frontmatter is not documented, so relay writes the body with `description` only.

## Cline

Sources: [MCP](https://docs.cline.bot/mcp/mcp-overview.md), [CLI reference](https://docs.cline.bot/cli/cli-reference.md), [rules](https://docs.cline.bot/customization/cline-rules.md), [SDK plugins](https://docs.cline.bot/sdk/plugins)

| Kind | User | Project |
|---|---|---|
| MCP | `~/.cline/data/settings/cline_mcp_settings.json` (`CLINE_MCP_SETTINGS_PATH` overrides; also the VS Code extension's file when its `globalStorage` settings directory exists) | unsupported |
| Hooks | unsupported | unsupported |
| Rules | `~/Documents/Cline/Rules/<name>.md` | `.clinerules/<name>.md` |
| Commands | unsupported | unsupported |

- Confirmed: the CLI settings path and its override variable; remote servers `{"type": "streamableHttp", "url", "disabled": false}` (`type` defaults to the legacy `sse` when omitted); global rules in `~/Documents/Cline/Rules` (Cline also reads `~/.cline/rules` and `~/Cline/Rules`); workspace rules in `.clinerules/` or `.cline/rules/`; `paths:` frontmatter with glob patterns.
- The VS Code extension's settings file location is not documented (the docs say to open it from the MCP Servers panel); relay writes it only when the usual `~/.config/Code/User/globalStorage/saoudrizwan.claude-dev/settings` directory exists.
- Cline does not read environment variables in settings, so no `Authorization` header is written and the sync report says to authenticate the server in Cline.
- Hooks are functions in an `AgentPlugin` module (`beforeTool`, `afterTool`, ...), not a config file. The CLI reference lists `~/.cline/hooks/` and `.cline/hooks/` directories, but their file format is not documented.
- Workflows: the current docs only say `cline config workflows` prints the target directory, so the earlier `.clinerules/workflows` and `~/Documents/Cline/Workflows` locations are no longer written.
- A `.clinerules` that is a single file is left alone and rules are reported as unsupported until it becomes a directory.
- Cline subagents are a built-in tool (`spawn_agent`), not user-defined files, so subagent sync is unsupported.

## GitHub Copilot

Sources: [custom instructions](https://code.visualstudio.com/docs/copilot/customization/custom-instructions), [prompt files](https://code.visualstudio.com/docs/copilot/customization/prompt-files), [MCP servers](https://code.visualstudio.com/docs/copilot/customization/mcp-servers), [MCP configuration reference](https://code.visualstudio.com/docs/agents/reference/mcp-configuration)

Project scope only.

| Kind | Project |
|---|---|
| MCP | `.vscode/mcp.json` `servers` (`{"type": "http", "url"}`; `{"type": "stdio", "command", "args", "env"}`) |
| Rules | block in `.github/copilot-instructions.md` |
| Commands | `.github/prompts/<name>.prompt.md` (frontmatter `description`, `argument-hint`, `agent: agent`) |
| Hooks, skills, subagents | unsupported |

- Prompt files use `agent` (`ask`, `agent`, `plan` or a custom agent name); the `mode` key relay wrote before is not documented and is gone.
- VS Code documents `${input:...}` variables for secrets and does not document `${env:NAME}`, so remote entries carry no header and VS Code asks for authorization itself.
- The user-level profile `mcp.json` exists (open it with **MCP: Open User Configuration**) but its path is not documented, so relay does not write it.

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
