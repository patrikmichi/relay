# relay

`relay` is a CLI for managing AI agent skills (Claude, Codex, OpenCode,
Cursor, Cline, Gemini CLI, Windsurf — see [Skill provider
support](#skill-provider-support)) and agent definitions (Claude, opencode,
Codex, Cursor, Gemini CLI — see [Agent provider
support](#agent-provider-support)) across providers, and, when you're
connected to a gateway, for pulling skills/agents/MCP servers from a shared
catalog. `relay sync` keeps MCP servers, hooks, rules, commands, skills and
subagents consistent across Claude Code, Codex, Cursor, OpenCode, Gemini CLI,
Windsurf, Cline and GitHub Copilot (see [Syncing across coding
agents](#syncing-across-coding-agents)). Every command works fully offline
against your local machine; a handful of catalog commands need a gateway to
reach. `relay providers` is the live, authoritative list of loaded providers
— it never goes stale as new providers are added.

## Install

> **v0.1.0 is withdrawn.** Its binaries had a private gateway hostname
> compiled in and its tag had no source. Upgrade to v0.2.0 or later; see the
> [v0.2.0 release notes](docs/releases/v0.2.0.md).

**Homebrew (macOS/Linux):**

```bash
brew install patrikmichi/tap/relay
```

**Scoop (Windows):**

```powershell
scoop bucket add patrikmichi https://github.com/patrikmichi/scoop-bucket
scoop install patrikmichi/relay
```

**Go install** (requires Go 1.26 or newer):

```bash
go install github.com/patrikmichi/relay/cmd/relay@latest
```

**Direct download:** archives for macOS, Linux and Windows (amd64, arm64) are
on the [releases page](https://github.com/patrikmichi/relay/releases).
`SHA256SUMS.txt` is signed with Sigstore cosign, and every archive has an
SBOM and a GitHub build provenance attestation
(`gh attestation verify <archive> --repo patrikmichi/relay`).

Every install is a single static `relay` binary with no runtime
dependencies and no built-in gateway: point it at one with
`relay config set-gateway <url>` or `GATEWAY_URL`.

## Quickstart

```bash
# Authenticate against a gateway for catalog access and service calls.
relay login

# Fully offline: migrate a skill you already have installed for one
# provider so it's also available to another.
relay skill migrate my-skill --from claude --to codex

# Requires a gateway: install a skill by catalog id/slug. If it needs
# connectors you have not linked, relay offers `relay connect` for each.
relay skill install pr-triage --to claude
relay skill info pr-triage
```

```bash
# Fully offline: migrate an installed agent from one provider to another.
relay agent migrate reviewer --from claude --to opencode

# Requires a gateway: install an agent by catalog id/slug.
relay agent install res_abc123 --to claude
```

```bash
# Fully offline: write your rules, commands, hooks and MCP servers into every
# coding agent installed on this machine. Preview first.
relay sync --dry-run
relay sync

# Register an MCP server in all of them.
relay mcp install docs --url https://docs.example.com/mcp --bearer-env DOCS_TOKEN

# Requires a gateway: browse the connector catalog and link your account.
relay connectors search invoice
relay connect fakturoid
```

## Skill provider support

`relay skill migrate/install/list/diff/scan/score/uninstall/rollback` work
offline against 7 providers, every one vendor-verified against its own
docs (run `relay providers` for the live, detected list — this table is a
point-in-time summary, not the source of truth). "Extra fields" is what
that provider's on-disk format can carry beyond the universal name +
description; migrating a skill that uses one of them onto a provider whose
column doesn't list it reports that field `[dropped]` in the fidelity
report (verified 2026-09-05 against a real 7-way `skill migrate` run):

| Provider | Format status | Extra fields carried |
|---|---|---|
| **Claude Code** | supported | allowed-tools, disable-model-invocation, license |
| **Codex** | supported | (name + description only; sidecar `agents/openai.yaml` carries Codex-only interface/policy/dependency extras) |
| **opencode** | supported | license, compatibility, metadata |
| **Cursor** | supported | paths, disable-model-invocation, metadata |
| **Cline** | supported | metadata |
| **Gemini CLI** | supported | metadata |
| **Windsurf** | supported | metadata |

## Agent provider support

`relay agent migrate/list/diff/scan/uninstall/rollback` work offline against
5 providers; `relay agent install <path|catalog-id>` (a local Claude
Markdown agent works offline; a catalog id needs the gateway — same
fan-out/`--to`/manifest/rollback model as `relay skill install`) projects
onto the same 5:

| Provider | Shape | Support |
|---|---|---|
| **Claude Code** | flat `<name>.md` (frontmatter + body) at `~/.claude/agents/` (user) / `.claude/agents/` (project) | **Supported** |
| **opencode** | flat `<name>.md` (frontmatter + body) at `~/.config/opencode/agents/` (user) / `.opencode/agents/` (project); `tools` is reshaped to/from opencode's `{tool: bool}` map | **Supported** |
| **Codex** | flat `<name>.toml` (GA 2026-03-16 custom-agent format — NOT the deprecated `[profiles.*]` block) at `~/.codex/agents/` (user) / `.codex/agents/` (project); no frontmatter/body split — the instructions live in a `developer_instructions` TOML string key | **Supported** — via a stdlib-only flat-TOML codec, no new dependency |
| **Cursor** | flat `<name>.md` (frontmatter + body) at `~/.cursor/agents/` (user) / `.cursor/agents/` (project, also reads `.claude/agents/`/`.codex/agents/` for compat) | **Supported** — no `tools` allowlist key (subagents inherit the parent's tools) |
| **Gemini CLI** | flat `<name>.md` (frontmatter + body) at `~/.gemini/agents/` (user) / `.gemini/agents/` (project) | **Supported** — `tools` carried as a real list |
| **Cline**, **Windsurf** | no subagent/custom-agent file primitive published | **Unsupported** — `agent migrate --to cline` (or windsurf) errors naming the supported agent providers instead of silently no-op'ing |

Fidelity matrix (verified 2026-09-05 against a real 5-way `agent migrate`
run) — `x` = preserved/carried, `~` = degraded (reshaped or alias-mapped,
not silently identical), `-` = dropped, reported in the fidelity report:

| Field | claude | opencode | codex | cursor | gemini-cli |
|---|---|---|---|---|---|
| Name/Description/Body | x | x | x | x | x |
| Model | x | ~ (alias-mapped) | - (no alias table; omitted, target default used) | - (no alias table; omitted, target default used) | - (no alias table; omitted, target default used) |
| Tools | x | ~ (list ↔ `{tool: bool}` map) | - (no tools key) | - (no tools key) | x (real list) |
| Temperature | - | x | - | - | x |
| Mode | - | x | - | - | - |
| Memory | x | - | - | - | - |
| Skills | x | - | - | - | - |

Skill management (`relay skill ...`) supports 7 providers — see [Skill
provider support](#skill-provider-support) above.

## Syncing across coding agents

`relay sync` writes MCP servers, hooks, rules, commands, skills and subagents
into the config of every coding agent it supports, at the user scope
(`~/...`) and the project scope (the current directory). It works offline.

| Agent | MCP servers | Hooks | Rules | Commands | Skills | Subagents |
|---|---|---|---|---|---|---|
| **Claude Code** | user, project | user, project | user, project | user, project | yes | yes |
| **Codex** | user, project | user, project | user, project | user (prompts) | yes | yes |
| **Cursor** | user, project | user, project | project | user, project | yes | yes |
| **OpenCode** | user, project | user, project (plugin) | user, project | user, project | yes | yes |
| **Gemini CLI** | user, project | user, project | user, project | user, project | yes | yes |
| **Windsurf** | user | user, project | user, project | user, project (workflows) | yes | - |
| **Cline** | user | - | user, project | - | yes | - |
| **GitHub Copilot** | project | - | project | project | - | - |

Besides these, the `env` kind sets `RELAY_AGENT=1` in the environment of
Claude Code and Codex (user scope) and in stdio MCP servers that run relay, so
the relay an agent starts uses the agent profile. See
[Agents and approvals](#agents-and-approvals); `--kind mcp,rules` leaves it out.

`relay sync --matrix` prints the live matrix with the reason for every gap.
Anything an agent has no format for is reported as `unsupported` in the sync
report; nothing is skipped silently. Per-agent paths, keys and caveats are in
[docs/reference/agent-config-formats.md](docs/reference/agent-config-formats.md).

```bash
relay sync --dry-run                     # diff of every file that would change
relay sync                               # all detected agents, user scope
relay sync --to claude,codex --scope all # named agents, user + current project
relay sync --kind mcp,rules --to all     # only some kinds, every supported agent
```

**Safety.** A sync changes what your agents run, so it is gated:

- It is refused while `RELAY_AGENT` is set (`--yes` does not override that).
- A write that adds or changes a hook or a stdio MCP command shows the full
  diff and asks on a terminal. Without a terminal it refuses; `--yes` skips the
  question after you reviewed `--dry-run`. A project's `.relay/` is untrusted
  input, which is why hooks from it are shown, not just applied.
- Project paths are never followed through symlinks. A user config that is a
  symlink is followed only while it stays inside your home directory, and the
  report says so. A file that changed after the plan was made is not
  overwritten.
- The gateway entry uses the gateway saved in the config file, never
  `$GATEWAY_URL`. Another gateway needs an explicit `--gateway-url` and a
  confirmation.
- `GATEWAY_API_KEY` is sent only to `RELAY_API_KEY_ORIGIN`, or else the saved
  gateway; a flag or variable that points elsewhere is refused (a loopback
  development gateway excepted).
- Skill, agent and plugin downloads are verified against the gateway's pinned
  signing keys; see
  [docs/reference/signed-downloads.md](docs/reference/signed-downloads.md).
  `--require-signature` refuses unsigned ones and `relay config trust-key`
  reviews changed keys.

**What gets synced.** MCP servers come from the gateway (when one is
configured) plus servers added with `relay mcp install`. Hooks, rules and
commands come from files you keep in one place:

```
~/.config/relay/sync/            (project scope: <project>/.relay/)
├── rules/<name>.md              frontmatter: description, globs, always
├── commands/<name>.md           frontmatter: description, argument-hint; body uses $ARGUMENTS
├── hooks.json                   {"hooks": [{"event": "pre-tool-use", "matcher": "Bash", "command": "..."}]}
└── mcp.json                     written by relay mcp install / remove
```

Skills and subagents are copied from `--from` (default `claude`) to the other
agents through the same engine as `relay skill migrate` and
`relay agent migrate`, so its fidelity reports and refusals apply.

**Your entries are never clobbered.** Shared files such as `~/.claude.json`,
`~/.claude/settings.json`, `~/.codex/config.toml`, `.cursor/mcp.json`,
`opencode.json` and `~/.gemini/settings.json` are merged, not rewritten: key
order and formatting are kept, and relay changes only what it wrote.
What relay wrote is recorded in a hash ledger (`~/.config/relay/sync-ledger.json`):
the entry, hook, block or file as written. Comment markers only show where a
TOML or Markdown block starts and ends. A later sync updates what still matches
the ledger and removes the ones you dropped from the source. An entry with
the same name that you created, or one you edited after relay wrote it, is
reported as a `conflict` and left alone; `--force` replaces it. A config file
relay cannot parse (for example JSON with comments) is skipped and reported.
All files in a run are written in one transaction that restores them if any
write fails, and `--dry-run` shows the diff without writing.

**Credentials are never written.** Remote MCP entries carry an environment
reference (`Bearer ${GATEWAY_API_KEY}`, `bearer_token_env_var`, `{env:...}`)
or no credentials at all, in which case the agent signs in with OAuth.
`--mcp-auth auto` (default, same as `oauth`) writes no credentials for the
gateway entry: the agent signs in with OAuth, and the gateway marks every
OAuth session as an agent's, so its writes wait for approval. Cline and GitHub
Copilot cannot do this from the file, so the report asks you to authorize the
server in the app. `--mcp-auth api-key` instead references `$GATEWAY_API_KEY`,
which is not marked as an agent's and bypasses approvals; the sync warns.

### Saved routes

Save what goes where once, and a plain `relay sync` follows it:

```bash
relay sync routes add --from claude --to cursor --kind skills,subagents
relay sync routes add --kind hooks,rules,commands --to claude,codex
relay sync routes list
relay sync --dry-run          # the plan, per route
relay sync routes remove 1    # by number, or by kind for default targets
```

A route with `--from` copies skills or subagents from that provider, and
settings (`mcp`, `hooks`, `rules`, `commands`) from that agent's own config,
for example `relay sync routes add --from cursor --to codex --kind rules`.
Only what you wrote there is read; relay's own blocks and entries are skipped,
and copies are lossy between some agents (see the reference). Settings with
no `--from` come from the relay sync directory and have default targets. With saved
routes a plain `relay sync` syncs only what they cover; `--to`, `--from` or
`--kind` run one ordinary sync for that invocation. `relay doctor` checks that
the routes name installed agents. The routes live in the `sync` key of
`~/.config/relay/config.json`; see [docs/reference/sync-routes.md](docs/reference/sync-routes.md).

### MCP servers

```bash
relay mcp install clockify                # gateway catalog service: enables the gateway server in every agent
relay mcp install docs --url https://docs.example.com/mcp --bearer-env DOCS_TOKEN
relay mcp install files --command npx --arg -y --arg @acme/files-mcp --env ROOT='${HOME}'
relay mcp remove docs
relay mcp list                            # read-only view of the catalog's mcp_server resources
```

Catalog services are reached through the gateway's own MCP server (`/api/mcp`),
so installing one by name enables that server and points you at `relay
connect`. Servers with `--url` or `--command` are registered directly.

### Gateway connectors

```bash
relay connectors                          # the gateway's service catalog
relay connectors search invoice           # by name, category, tag, description or tool
relay connectors --category CRM --status  # filter, and show whether your account is linked
relay connect fakturoid                   # link your own account
relay services                            # tools per service, plus connected / not connected
```

See [docs/reference/connectors.md](docs/reference/connectors.md) for the
endpoints involved and what a gateway must allow.

A catalog skill can declare the connectors it needs. `relay skill install`
checks them with the gateway, offers `relay connect` for the missing ones
(`--yes` to connect without asking, `--no-connect` to only report), checks again
and fails while any are unmet unless `--ignore-requirements`. `relay skill info`
shows the requirements and whether you can use the skill, and `relay sync`
warns about local skills whose `SKILL.md` declares `requires.connectors`.

### Skill evals

```bash
relay skill evals sets create pr-triage --name smoke
relay skill evals case add pr-triage smoke --kind trigger --name "asks for a review" \
  --input "Can you review this PR?" --should-trigger
relay skill eval pr-triage --wait                          # score the current version
relay skill compare pr-triage --versions 1.1.0,1.2.0 --wait
relay skill optimize pr-triage --kind description --wait   # leaves a draft
relay skill drafts promote pr-triage sdr_123               # through the publish pipeline
relay skill quality pr-triage
```

`--json` works on every one of them, a failed run exits 20 and
`--fail-on-regression` exits 21 when a score dropped, for CI. See
[docs/reference/skill-evals.md](docs/reference/skill-evals.md).

### Claude Code plugin marketplace

`relay sync --marketplace` (or `--dir <path>`) additionally pulls your
gateway's Claude Code plugin marketplace into
`~/.config/relay/marketplace/<name>/` and prints the managed-settings
fragment to merge. It needs a gateway and is not run by default. Earlier
versions rejected `--to`; it now selects the agents to sync.

### Agents and approvals

A gateway can hold an autonomous agent's writes until a person approves them.
Sign the agent in as its own profile, stored next to your own session:

```bash
relay login --agent                    # once: a session the gateway marks as an agent's
relay sync                             # sets RELAY_AGENT=1 for the agents it configures
RELAY_AGENT=1 relay whoami             # Profile:  agent (RELAY_AGENT=1; writes wait for approval)

# What an agent sees when a write is held (exit status 12):
RELAY_AGENT=1 relay stripe create_refund --arg charge=ch_1 --wait=120

# What you do, with your own session (no RELAY_AGENT):
relay approvals list
relay approvals approve apr_3f9c4e --grant 30   # shows the whole call, asks y/N
```

`relay login` says which kind of session it asked for, because the kind is
fixed when the login starts: `--agent` puts `agent=1` on the authorize request
(or `{"agent":true}` on the device-code request), and the token requests carry
no flag. An agent session reads connections, connectors and the marketplace but
cannot change them: `relay connect`, `relay publish` and the skill eval,
optimize and promote commands print the gateway's reason and tell you to use
your personal profile (unset `RELAY_AGENT`) or the MCP tools.

`relay explain call <service> <tool>` shows what a tool needs: `approval` for
destructive and outward tools, `grant` for writes. Exit statuses, the profiles
and what `relay sync` sets are in
[docs/reference/approvals.md](docs/reference/approvals.md).

**Security note.** `RELAY_AGENT` is a selector, not a security boundary. The
agent marker is chosen when a session is issued; an unmarked `relay login` on
the same machine is still unrestricted, and a process that unsets `RELAY_AGENT`
or reads your keychain acts as you. The agent profile makes the default safe;
it is not a sandbox. The planned protection is gateway-side human presence:
passkey step-up for approvals and high-risk actions, with approvals confirmed
on the approving device. keylatch is an optional integration. As a minimum, `relay approvals approve` and `deny` need an interactive
terminal and show the whole call before they ask; they refuse when stdin is not
a terminal.

## The offline-vs-gateway model

Every `relay` command falls into exactly one of two buckets:

| | LOCAL | GATEWAY |
|---|---|---|
| Touches | Only files on this machine (`~/.claude/`, `~/.claude.json`, `~/.codex/`, `~/.cursor/`, `~/.config/opencode/`, `~/.gemini/`, `~/.codeium/windsurf/`, `~/.cline/`, project config in the current directory, and relay's own manifest) | A configured gateway over HTTP |
| Needs auth | No | Yes — `relay login` or `GATEWAY_API_KEY` |
| Examples | `relay sync`, `relay mcp install`/`remove`, `relay skill migrate`, `relay skill install <path>`, `relay skill list`, `relay skill diff`, `relay skill scan`, `relay skill uninstall`, `relay skill rollback`, `relay agent migrate`, `relay agent list`, `relay agent diff`, `relay agent scan`, `relay agent uninstall`, `relay agent rollback`, `relay providers`, `relay doctor` | `relay skill install <catalog-id>`, `relay skill search`, `relay skill info`, `relay skill eval`/`compare`/`optimize`/`drafts`/`quality`/`evals`, `relay agent install <catalog-id>`, `relay mcp list`, `relay connectors`, `relay connect`, `relay publish`, `relay sync --marketplace`, `relay services`, `relay call`, `relay help-tools`, `relay approvals`, `relay login`/`logout`/`whoami`/`authorize`/`tokens` |

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
| `relay login [--device] [--agent]` | gateway | Authenticate via Google OAuth (browser or device-code flow); `--agent` signs in the agent profile, a session the gateway holds to approvals |
| `relay logout` | gateway | Revoke the current profile's session and remove the stored token |
| `relay whoami [--full]` | gateway | Show the current authenticated identity and which profile (human or agent) answered |
| `relay approvals list\|show\|approve\|deny\|wait` | gateway | List the held calls of agents, decide them with your own session, or wait for one's result (exit 0, 10 denied, 11 expired, 12 pending) |
| `relay explain call <service> <tool> [--json]` | gateway | Report a tool's risk class and decision, and whether an agent needs an approval or a grant |
| `relay authorize <service> --tool <tool>` | gateway | Request gateway tool access and verify grants and linked credentials |
| `relay tokens list` | gateway | Show active session info |
| `relay tokens revoke` | gateway | Revoke the current session token |
| `relay config set-gateway <url>` | local | Persist a gateway URL to `~/.config/relay/config.json` |
| `relay config get-gateway` | local | Print the effective gateway URL |
| `relay config show` | local | Print all resolved config as JSON |
| `relay services [--no-status]` | gateway | List services available on the configured gateway, with whether your account is connected |
| `relay connectors [search <query>] [--category] [--status]` | gateway | Browse the gateway's connector catalog |
| `relay connect <slug>` | gateway | Link your account: API-key prompt, or the OAuth consent flow in your browser |
| `relay help-tools [service]` | gateway | List tools for one or all services |
| `relay call <service> <tool> [--arg k=v] [--wait[=SECONDS]]` | gateway | Call a tool on a gateway service; a call held for approval exits 12, or with `--wait` waits and prints the result |
| `relay sync [--to <agents\|all>] [--scope] [--kind] [--dry-run] [--force]` | local | Write MCP servers, hooks, rules, commands, skills and subagents into the config of every supported coding agent (see [Syncing across coding agents](#syncing-across-coding-agents)) |
| `relay sync --matrix` | local | Print which agent supports which artifact kind at which scope |
| `relay sync --marketplace [--dir]` | gateway | Also pull your gateway's Claude Code plugin marketplace |
| `relay publish <path> [--type] [--watch]` | gateway | Publish a skill/agent/MCP server/prompt/plugin to the catalog |
| `relay publish status <versionId> [--watch]` | gateway | Poll a publish's review status |
| `relay skill publish <path>` | gateway | Alias for `relay publish` scoped to skills |
| `relay agent publish <path>` | gateway | Publish an agent definition |
| `relay mcp publish [--descriptor]` | gateway | Publish an MCP server descriptor |
| `relay mcp list [--json]` | gateway | List catalog `mcp_server` resources (read-only) |
| `relay mcp install <name> [--url\|--command] [--to <agents>]` | local/gateway | Register an MCP server in your coding agents; a bare catalog name enables the gateway server |
| `relay mcp remove <name> [--to <agents>]` | local | Remove an MCP server relay registered |
| `relay skill install <path>` | local | Install a skill from a local directory/file into one or more providers |
| `relay skill install <catalog-id> [--yes\|--no-connect] [--ignore-requirements]` | gateway | Install a skill by catalog id/slug into one or more providers, after checking the connectors it needs |
| `relay skill info <catalog-id\|slug>` | gateway | Show a catalog skill, the connectors it requires and whether you have connected them |
| `relay skill evals sets list\|show\|create\|import\|export\|delete` | gateway | Manage a skill's eval sets |
| `relay skill evals case add\|remove` | gateway | Add or remove a trigger or task case |
| `relay skill eval <skill> [--set] [--version\|--draft] [--repeats] [--wait] [--fail-on-regression]` | gateway | Score a version or draft against an eval set (exit 20 failed run, 21 regression) |
| `relay skill compare <skill> --versions a,b` | gateway | Score several versions or drafts side by side with significance tests |
| `relay skill optimize <skill> --kind description\|instructions [--wait]` | gateway | Rewrite the description or instructions into a scored draft |
| `relay skill drafts list\|show\|promote\|discard` | gateway | Review optimization drafts and publish one through the publish pipeline |
| `relay skill quality <skill>` / `--all` | gateway | Quality score and history by version |
| `relay agent install <path\|catalog-id> [--to <p>...]` | local/gateway | Install a local Claude Markdown agent or a governed catalog bundle into one or more agent providers; supports history and rollback |
| `relay skill search [query]` | gateway | Search the catalog for installable skills |
| `relay skill migrate <name> --from <p> [--to <p>...]` | local | Project an installed skill from one provider to another |
| `relay skill list` | local | List installed skills across providers |
| `relay skill diff <name>` | local | Diff a skill's projection across providers |
| `relay skill scan <name>` / `relay skill score <name>` | local | Inspect a skill's manifest/fidelity |
| `relay skill uninstall <name>` | local | Remove an installed skill |
| `relay skill rollback [manifest-entry-id]` | local | Revert to a prior manifest entry |
| `relay agent migrate <name> --from <p> [--to <p>...]` | local | Project an installed agent from one provider to another (claude, opencode, codex, cursor, gemini-cli) |
| `relay agent list` | local | List installed agents across agent providers |
| `relay agent diff <name>` | local | Diff an agent's projection across providers |
| `relay agent scan <name>` | local | Inspect an agent for dangerous shell patterns/hardcoded secrets |
| `relay agent uninstall <name>` | local | Remove an installed agent |
| `relay agent rollback [manifest-entry-id]` | local | Revert to a prior agent manifest entry |
| `relay providers` | local | List supported providers and their detection status |
| `relay doctor [--json]` | local | Check provider directories, installed skills and agents, synced MCP servers/hooks/rules/commands, recovery state and gateway health |
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
| `GATEWAY_URL` | Gateway base URL (overrides the config file) |
| `GATEWAY_API_KEY` | Non-interactive bearer auth — use in scripts/CI instead of `relay login`. Sent only to the gateway named by `RELAY_API_KEY_ORIGIN`, or else the one saved in the config file |
| `RELAY_API_KEY_ORIGIN` | The one gateway origin `GATEWAY_API_KEY` may be sent to, e.g. `https://gateway.example.com`. Needed in CI when no gateway is saved; a `--gateway-url` or `$GATEWAY_URL` for another origin is refused |
| `RELAY_EMAIL` | Selects which keychain-stored session to use (overrides the last `relay login`'d identity) |
| `RELAY_AGENT` | `1` makes relay use the agent profile's session, which the gateway holds to approvals, and never your own; `relay login` then signs the agent profile in. `relay sync` sets it for the agents it configures |
| `RELAY_TIMEOUT` | Limit for each gateway request, e.g. `90s` or `10m`; `0` means no limit. Defaults: 30s, 150s for tool calls and uploads. `--timeout` overrides it. A request that hits the limit exits with status 4 |

Interactive sessions authenticate via `relay login` (OAuth, tokens stored in
the OS keychain, auto-refreshing). Non-interactive contexts should set
`GATEWAY_URL` + `GATEWAY_API_KEY` instead.

## License

[Apache-2.0](LICENSE)

## Gateway compatibility

Skill search uses `GET /api/catalog/skills/search?query=...`, independent of aggregate MCP. Older gateways need upgrading for search.
Catalog agent installation requires `X-Resource-Type: agent`, catalog identity and version headers, a verified checksum, and scan verdict. Bundles containing resources the flat agent format cannot preserve are rejected. Local agent installation accepts a Claude Markdown file and works without gateway credentials.
Source format provenance remains Claude for downloaded agents so tool and model translation remain correct; catalog id and version separately record catalog origin.

Provider formats were checked against [Codex custom agents](https://learn.chatgpt.com/docs/agent-configuration/subagents), [Cursor subagents](https://cursor.com/docs/subagents), and [Gemini CLI subagents](https://geminicli.com/docs/core/subagents/). The adapters support the fields documented in [agent formats](docs/agent-formats.md) and refuse migrations that cannot preserve execution restrictions.

`relay authorize <service> --tool <tool>` requests access to gateway tools.
Repeat `--scope` for multiple tools and use `--account` when selecting a linked
account. The command polls for approved grants and required credentials, with a
default timeout of ten minutes (`--wait-timeout`, maximum thirty minutes). Tool grants
do not verify the upstream provider's OAuth scopes; connect the required account
in the gateway.

### Diagnostics and format fidelity

`relay doctor [--json]` checks provider directories, current installed hashes and
permissions, pending recovery journals, override validation and gateway/session
health. For every detected coding agent it also checks each synced artifact kind
(MCP servers, agent environment, hooks, rules, commands) at the user and project scope as a
`sync:<scope>:<agent>:<kind>` check: in sync, not synced yet, out of sync,
edited since relay wrote it, or blocked by a config file relay cannot parse. It
verifies the sync sources parse and that environment variables referenced by MCP
entries (`mcp-env:<VAR>`) are set. `--offline` skips network checks. Directory
permission checks inspect mode bits, not ACLs or an actual write probe.

`relay mcp inspect <catalog-id> [--json]` shows source, transport, authentication
mode/scope and declared tool count. Commands, endpoint URLs and credential
reference values are excluded from inspection output.

See [agent formats](docs/agent-formats.md) for supported fields and refusal rules,
and [agent config formats](docs/reference/agent-config-formats.md) for the
files `relay sync` writes for each coding agent.
