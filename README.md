# Zuano

Zuano is published by Loopmatic. `zuano` is a CLI for managing AI agent skills (Claude, Codex, OpenCode,
Cursor, Cline, Gemini CLI, Windsurf — see [Skill provider
support](#skill-provider-support)) and agent definitions (Claude, opencode,
Codex, Cursor, Gemini CLI — see [Agent provider
support](#agent-provider-support)) across providers, and, when you're
connected to a gateway, for pulling skills/agents/MCP servers from a shared
catalog. `zuano sync` keeps MCP servers, hooks, rules, commands, skills and
subagents consistent across Claude Code, Codex, Cursor, OpenCode, Gemini CLI,
Windsurf, Cline and GitHub Copilot (see [Syncing across coding
agents](#syncing-across-coding-agents)). Every command works fully offline
against your local machine; a handful of catalog commands need a gateway to
reach. `zuano providers` is the live, authoritative list of loaded providers
— it never goes stale as new providers are added.

## Install

> **v0.1.0 is withdrawn.** Its binaries had a private gateway hostname
> compiled in and its tag had no source. Use v0.3.0 or later, the first
> release published as Zuano; see the
> [v0.2.0 release notes](docs/releases/v0.2.0.md) for why v0.1.0 was withdrawn
> and the [v0.3.0 release notes](docs/releases/v0.3.0.md) for the rename.

**Homebrew (macOS/Linux):**

```bash
brew install patrikmichi/tap/zuano
```

**Scoop (Windows):**

```powershell
scoop bucket add patrikmichi https://github.com/patrikmichi/scoop-bucket
scoop install patrikmichi/zuano
```

**Go install** (requires Go 1.26 or newer):

```bash
go install github.com/patrikmichi/zuano/cmd/zuano@latest
```

**Direct download:** archives for macOS, Linux and Windows (amd64, arm64) are
on the [releases page](https://github.com/patrikmichi/zuano/releases).
`SHA256SUMS.txt` is signed with Sigstore cosign, and every archive has an
SBOM and a GitHub build provenance attestation
(`gh attestation verify <archive> --repo patrikmichi/zuano`).

Every install is a single static `zuano` binary with no runtime
dependencies and no built-in gateway: point it at one with
`zuano config set-gateway <url>` or `ZUANO_URL`.

## Quickstart

```bash
# Authenticate against a gateway for catalog access and service calls.
zuano login

# Fully offline: migrate a skill you already have installed for one
# provider so it's also available to another.
zuano skill migrate my-skill --from claude --to codex

# Requires a gateway: install a skill by catalog id/slug. If it needs
# connectors you have not linked, zuano offers `zuano connect` for each.
zuano skill install pr-triage --to claude
zuano skill info pr-triage
```

```bash
# Fully offline: migrate an installed agent from one provider to another.
zuano agent migrate reviewer --from claude --to opencode

# Requires a gateway: install an agent by catalog id/slug.
zuano agent install res_abc123 --to claude
```

```bash
# Fully offline: write your rules, commands, hooks and MCP servers into every
# coding agent installed on this machine. Preview first.
zuano sync --dry-run
zuano sync

# Register an MCP server in all of them.
zuano mcp install docs --url https://docs.example.com/mcp --bearer-env DOCS_TOKEN

# Requires a gateway: browse the connector catalog and link your account.
zuano connectors search invoice
zuano connect fakturoid
```

## Skill provider support

`zuano skill migrate/install/list/diff/scan/score/uninstall/rollback` work
offline against 7 providers, every one vendor-verified against its own
docs (run `zuano providers` for the live, detected list — this table is a
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

`zuano agent migrate/list/diff/scan/uninstall/rollback` work offline against
5 providers; `zuano agent install <path|catalog-id>` (a local Claude
Markdown agent works offline; a catalog id needs the gateway — same
fan-out/`--to`/manifest/rollback model as `zuano skill install`) projects
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

Skill management (`zuano skill ...`) supports 7 providers — see [Skill
provider support](#skill-provider-support) above.

## Syncing across coding agents

`zuano sync` writes MCP servers, hooks, rules, commands, skills and subagents
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

Besides these, the `env` kind sets `ZUANO_AGENT=1` in the environment of
Claude Code and Codex (user scope) and in stdio MCP servers that run zuano, so
the zuano an agent starts uses the agent profile. See
[Agents and approvals](#agents-and-approvals); `--kind mcp,rules` leaves it out.

`zuano sync --matrix` prints the live matrix with the reason for every gap.
Anything an agent has no format for is reported as `unsupported` in the sync
report; nothing is skipped silently. Per-agent paths, keys and caveats are in
[docs/reference/agent-config-formats.md](docs/reference/agent-config-formats.md).

```bash
zuano sync --dry-run                     # diff of every file that would change
zuano sync                               # all detected agents, user scope
zuano sync --to claude,codex --scope all # named agents, user + current project
zuano sync --kind mcp,rules --to all     # only some kinds, every supported agent
```

**Safety.** A sync changes what your agents run, so it is gated:

- It is refused while `ZUANO_AGENT` is set (`--yes` does not override that).
- A write that adds or changes a hook or a stdio MCP command shows the full
  diff and asks on a terminal. Without a terminal it refuses; `--yes` skips the
  question after you reviewed `--dry-run`. A project's `.zuano/` is untrusted
  input, which is why hooks from it are shown, not just applied.
- Project paths are never followed through symlinks. A user config that is a
  symlink is followed only while it stays inside your home directory, and the
  report says so. A file that changed after the plan was made is not
  overwritten.
- The gateway entry uses the gateway saved in the config file, never
  `$ZUANO_URL`. Another gateway needs an explicit `--gateway-url` and a
  confirmation.
- `ZUANO_API_KEY` is sent only to `ZUANO_API_KEY_ORIGIN`, or else the saved
  gateway; a flag or variable that points elsewhere is refused (a loopback
  development gateway excepted).
- Skill, agent and plugin downloads are verified against the gateway's pinned
  signing keys; see
  [docs/reference/signed-downloads.md](docs/reference/signed-downloads.md).
  `--require-signature` refuses unsigned ones and `zuano config trust-key`
  reviews changed keys.

**What gets synced.** MCP servers come from the gateway (when one is
configured) plus servers added with `zuano mcp install`. Hooks, rules and
commands come from files you keep in one place:

```
~/.config/zuano/sync/            (project scope: <project>/.zuano/)
├── rules/<name>.md              frontmatter: description, globs, always
├── commands/<name>.md           frontmatter: description, argument-hint; body uses $ARGUMENTS
├── hooks.json                   {"hooks": [{"event": "pre-tool-use", "matcher": "Bash", "command": "..."}]}
└── mcp.json                     written by zuano mcp install / remove
```

Skills and subagents are copied from `--from` (default `claude`) to the other
agents through the same engine as `zuano skill migrate` and
`zuano agent migrate`, so its fidelity reports and refusals apply.

**Your entries are never clobbered.** Shared files such as `~/.claude.json`,
`~/.claude/settings.json`, `~/.codex/config.toml`, `.cursor/mcp.json`,
`opencode.json` and `~/.gemini/settings.json` are merged, not rewritten: key
order and formatting are kept, and zuano changes only what it wrote.
What zuano wrote is recorded in a hash ledger (`~/.config/zuano/sync-ledger.json`):
the entry, hook, block or file as written. Comment markers only show where a
TOML or Markdown block starts and ends. A later sync updates what still matches
the ledger and removes the ones you dropped from the source. An entry with
the same name that you created, or one you edited after zuano wrote it, is
reported as a `conflict` and left alone; `--force` replaces it. A config file
zuano cannot parse is skipped and reported. MCP server files with comments or
trailing commas (`.vscode/mcp.json`, `opencode.jsonc`) are edited in place and
keep them; hook files with comments are skipped.
All files in a run are written in one transaction that restores them if any
write fails, and `--dry-run` shows the diff without writing.

**Credentials are never written.** Remote MCP entries carry an environment
reference (`Bearer ${ZUANO_API_KEY}`, `bearer_token_env_var`, `{env:...}`)
or no credentials at all, in which case the agent signs in with OAuth.
`--mcp-auth auto` (default, same as `oauth`) writes no credentials for the
gateway entry: the agent signs in with OAuth, and the gateway marks every
OAuth session as an agent's, so its writes wait for approval. Cline and GitHub
Copilot cannot do this from the file, so the report asks you to authorize the
server in the app. `--mcp-auth api-key` instead references `$ZUANO_API_KEY`,
which is not marked as an agent's and bypasses approvals; the sync warns.

### Saved routes

Save what goes where once, and a plain `zuano sync` follows it:

```bash
zuano sync routes add --from claude --to cursor --kind skills,subagents
zuano sync routes add --kind hooks,rules,commands --to claude,codex
zuano sync routes list
zuano sync --dry-run          # the plan, per route
zuano sync routes remove 1    # by number, or by kind for default targets
```

A route with `--from` copies skills or subagents from that provider, and
settings (`mcp`, `hooks`, `rules`, `commands`) from that agent's own config,
for example `zuano sync routes add --from cursor --to codex --kind rules`.
Only what you wrote there is read; zuano's own blocks and entries are skipped,
and copies are lossy between some agents (see the reference). Settings with
no `--from` come from the zuano sync directory and have default targets. With saved
routes a plain `zuano sync` syncs only what they cover; `--to`, `--from` or
`--kind` run one ordinary sync for that invocation. `zuano doctor` checks that
the routes name installed agents. The routes live in the `sync` key of
`~/.config/zuano/config.json`; see [docs/reference/sync-routes.md](docs/reference/sync-routes.md).

### MCP servers

```bash
zuano mcp install clockify                # gateway catalog service: enables the gateway server in every agent
zuano mcp install docs --url https://docs.example.com/mcp --bearer-env DOCS_TOKEN
zuano mcp install files --command npx --arg -y --arg @acme/files-mcp --env ROOT='${HOME}'
zuano mcp remove docs
zuano mcp list                            # read-only view of the catalog's mcp_server resources
```

Catalog services are reached through the gateway's own MCP server (`/api/mcp`),
so installing one by name enables that server and points you at `zuano
connect`. Servers with `--url` or `--command` are registered directly.

### Gateway connectors

```bash
zuano connectors                          # the gateway's service catalog
zuano connectors search invoice           # by name, category, tag, description or tool
zuano connectors --category CRM --status  # filter, and show whether your account is linked
zuano connect fakturoid                   # link your own account
zuano services                            # tools per service, plus connected / not connected
```

See [docs/reference/connectors.md](docs/reference/connectors.md) for the
endpoints involved and what a gateway must allow.

A catalog skill can declare the connectors it needs. `zuano skill install`
checks them with the gateway, offers `zuano connect` for the missing ones
(`--yes` to connect without asking, `--no-connect` to only report), checks again
and fails while any are unmet unless `--ignore-requirements`. `zuano skill info`
shows the requirements and whether you can use the skill, and `zuano sync`
warns about local skills whose `SKILL.md` declares `requires.connectors`.

### Skill evals

```bash
zuano skill evals sets create pr-triage --name smoke
zuano skill evals case add pr-triage smoke --kind trigger --name "asks for a review" \
  --input "Can you review this PR?" --should-trigger
zuano skill eval pr-triage --wait                          # score the current version
zuano skill compare pr-triage --versions 1.1.0,1.2.0 --wait
zuano skill optimize pr-triage --kind description --wait   # leaves a draft
zuano skill drafts promote pr-triage sdr_123               # through the publish pipeline
zuano skill quality pr-triage
```

`--json` works on every one of them, a failed run exits 20 and
`--fail-on-regression` exits 21 when a score dropped, for CI. See
[docs/reference/skill-evals.md](docs/reference/skill-evals.md).

### Claude Code plugin marketplace

`zuano sync --marketplace` (or `--dir <path>`) additionally pulls your
gateway's Claude Code plugin marketplace into
`~/.config/zuano/marketplace/<name>/` and prints the managed-settings
fragment to merge. It needs a gateway and is not run by default. Earlier
versions rejected `--to`; it now selects the agents to sync.

### Agents and approvals

A gateway can hold an autonomous agent's writes until a person approves them.
Sign the agent in as its own profile, stored next to your own session:

```bash
zuano login --agent                    # once: a session the gateway marks as an agent's
zuano sync                             # sets ZUANO_AGENT=1 for the agents it configures
ZUANO_AGENT=1 zuano whoami             # Profile:  agent (ZUANO_AGENT=1; writes wait for approval)

# What an agent sees when a write is held (exit status 12):
ZUANO_AGENT=1 zuano stripe create_refund --arg charge=ch_1 --wait=120

# What you do:
zuano approvals list
zuano approvals approve apr_3f9c4e              # opens the approval page; confirm with your passkey
zuano approvals approve apr_3f9c4e --no-browser # prints the link and a QR code for your phone
```

`zuano login` says which kind of session it asked for, because the kind is
fixed when the login starts: the authorize request carries
`principal=cli_personal`, or `principal=cli_agent` with `--agent` (the same
value goes in the device-code request body), and the token requests carry no
flag. A session the gateway no longer accepts (for instance one issued before
every credential carried a principal) is reported as such and needs a new
`zuano login`. An agent session reads connections, connectors and the marketplace but
cannot change them: `zuano connect`, `zuano publish` and the skill eval,
optimize and promote commands print the gateway's reason and tell you to use
your personal profile (unset `ZUANO_AGENT`) or the MCP tools.

`zuano explain call <service> <tool>` shows what a tool needs: `approval` for
destructive and outward tools, `grant` for writes. Exit statuses, the profiles
and what `zuano sync` sets are in
[docs/reference/approvals.md](docs/reference/approvals.md).

**Security note.** `ZUANO_AGENT` is a selector, not a security boundary. What
stops a process running as you from approving its own calls is that nothing in
the CLI decides: an approval is a passkey confirmation in a browser, so
`zuano approvals approve` and `deny` only open the approval page (or print a
link and a QR code for your phone). A personal CLI session is held to the same
approvals as an agent's; only a browser session is unrestricted.

## The offline-vs-gateway model

Every `zuano` command falls into exactly one of two buckets:

| | LOCAL | GATEWAY |
|---|---|---|
| Touches | Only files on this machine (`~/.claude/`, `~/.claude.json`, `~/.codex/`, `~/.cursor/`, `~/.config/opencode/`, `~/.gemini/`, `~/.codeium/windsurf/`, `~/.cline/`, project config in the current directory, and zuano's own manifest) | A configured gateway over HTTP |
| Needs auth | No | Yes — `zuano login` or `ZUANO_API_KEY` |
| Examples | `zuano sync`, `zuano mcp install`/`remove`, `zuano skill migrate`, `zuano skill install <path>`, `zuano skill list`, `zuano skill diff`, `zuano skill scan`, `zuano skill uninstall`, `zuano skill rollback`, `zuano agent migrate`, `zuano agent list`, `zuano agent diff`, `zuano agent scan`, `zuano agent uninstall`, `zuano agent rollback`, `zuano providers`, `zuano doctor` | `zuano skill install <catalog-id>`, `zuano skill search`, `zuano skill info`, `zuano skill eval`/`compare`/`optimize`/`drafts`/`quality`/`evals`, `zuano agent install <catalog-id>`, `zuano mcp list`, `zuano connectors`, `zuano connect`, `zuano publish`, `zuano sync --marketplace`, `zuano services`, `zuano call`, `zuano help-tools`, `zuano approvals`, `zuano billing`, `zuano usage`, `zuano runs`, `zuano sessions`, `zuano audit`, `zuano policy`, `zuano login`/`logout`/`whoami`/`authorize`/`tokens` |

**Fail-closed guidance.** If a GATEWAY command can't resolve a gateway URL —
none configured, `--offline` passed, or you're not authenticated — it refuses
to run rather than guessing, and prints:

```
no gateway configured. Run `zuano config set-gateway <url>` then `zuano login`
to install from the catalog. Local `zuano skill install <path>` and
`zuano skill migrate` work offline.
```

`--offline` is a root flag (`zuano --offline <command>`) that forces this
fail-closed behavior even if a gateway is otherwise configured — useful for
scripts/CI that must never make a network call.

## Command reference

Commands are grouped by what they are for. "Kind" says whether a command needs
a gateway.

### Account and configuration

| Command | Kind | Description |
|---|---|---|
| `zuano login [--device] [--agent]` | gateway | Authenticate via Google OAuth (browser or device-code flow); `--agent` signs in the agent profile, a session the gateway holds to approvals |
| `zuano logout` | gateway | Revoke the current profile's session and remove the stored token; exits non-zero if the gateway did not revoke it |
| `zuano whoami [--full]` | gateway | Show the current authenticated identity and which profile (human or agent) answered |
| `zuano tokens list` | gateway | Show active session info |
| `zuano tokens revoke` | gateway | Revoke the current session token |
| `zuano config set-gateway <url>` | local | Persist a gateway URL to `~/.config/zuano/config.json` |
| `zuano config get-gateway` | local | Print the effective gateway URL |
| `zuano config show` | local | Print all resolved config as JSON |
| `zuano config trust-key` | gateway | Pin the gateway's signing keys, used to verify signed downloads |

### Gateway services and calls

| Command | Kind | Description |
|---|---|---|
| `zuano services [--no-status]` | gateway | List services available on the configured gateway, with whether your account is connected |
| `zuano connectors [search <query>] [--category] [--status]` | gateway | Browse the gateway's connector catalog |
| `zuano connect <slug>` | gateway | Link your account: API-key prompt, or the OAuth consent flow in your browser |
| `zuano authorize <service> --tool <tool>` | gateway | Request gateway tool access and verify grants and linked credentials |
| `zuano help-tools [service]` | gateway | List tools for one or all services |
| `zuano call <service> <tool> [--arg k=v] [--arg-json k=<json>] [--wait[=SECONDS]]` | gateway | Call a tool on a gateway service; a call held for approval exits 12, or with `--wait` waits and prints the result; an approval page the gateway asks you to visit is opened (`--no-browser` prints it) and the call retried once approved |
| `zuano files upload\|download\|list\|rm` | gateway | Move files through the gateway's file store by id instead of inside a request body; `zuano call --file param=@path` uploads and passes one, `--save-files DIR` saves a result's files ([docs/reference/files.md](docs/reference/files.md)) |
| `zuano explain call <service> <tool> [--json]` | gateway | Report a tool's risk class and decision, and whether an agent needs an approval or a grant |
| `zuano approvals list\|show\|approve\|deny\|wait` | gateway | List held calls, open the page where one is approved or denied with a passkey (`--no-browser` prints a link and QR code), or wait for one's result (exit 0, 10 denied, 11 expired, 12 pending) |
| `zuano billing status\|usage\|open` | gateway | Show the plan, seats and spend cap, metered usage against allowances (`--meter`, `--period`, `--by`), or open the billing page; a request the plan refuses exits 30 to 33 with the upgrade link ([docs/reference/billing.md](docs/reference/billing.md)) |
| `zuano usage [--from] [--to]` | gateway | Show each quota metric against its soft and hard limit; a used-up quota exits 13 |
| `zuano sessions list\|revoke\|revoke-all` | gateway | List your browser and CLI sessions on the gateway, revoke one, or sign out everywhere (a passkey step-up); see [docs/reference/security-commands.md](docs/reference/security-commands.md) |
| `zuano audit verify\|export` | gateway | Verify the audit log's hash chain and checkpoints, or export events as JSON lines or CSV, resumable with `--resume` (org admins; [docs/reference/observability.md](docs/reference/observability.md)) |
| `zuano trace <request_id\|trace_id\|approval_id>` | gateway | Show the spans, audit entries and errors the gateway recorded for a request (org admins) |
| `zuano policy explain <service> <tool> [--arg k=v]` | gateway | Show whether the organisation's risk policy turns a call off, needs an approval, or lets it run, and who can change that |
| `zuano runs list\|show\|wait\|cancel\|retry` | gateway | Follow the gateway's background jobs (agent runs, evals, deliveries); a call that starts one prints its id, and `--wait` follows it ([docs/reference/runs.md](docs/reference/runs.md)) |

### Syncing coding agents

| Command | Kind | Description |
|---|---|---|
| `zuano sync [--to <agents\|all>] [--scope] [--kind] [--dry-run] [--force]` | local | Write MCP servers, hooks, rules, commands, skills and subagents into the config of every supported coding agent (see [Syncing across coding agents](#syncing-across-coding-agents)) |
| `zuano sync routes list\|add\|remove` | local | Save what a plain `zuano sync` copies where (see [Saved routes](#saved-routes)) |
| `zuano sync --matrix` | local | Print which agent supports which artifact kind at which scope |
| `zuano sync --marketplace [--dir]` | gateway | Also pull your gateway's Claude Code plugin marketplace |
| `zuano mcp install <name> [--url\|--command] [--to <agents>]` | local/gateway | Register an MCP server in your coding agents; a bare catalog name enables the gateway server |
| `zuano mcp remove <name> [--to <agents>]` | local | Remove an MCP server zuano registered |
| `zuano mcp link claude-ai\|chatgpt [--directory]` | gateway | Print the prefilled link (Claude) or the endpoint and steps (ChatGPT) to add the gateway as a connector (org admins) |

### Catalog: publish, search, inspect

| Command | Kind | Description |
|---|---|---|
| `zuano publish <path> [--type] [--watch]` | gateway | Publish a skill/agent/MCP server/prompt/plugin to the catalog |
| `zuano publish status <versionId> [--watch]` | gateway | Poll a publish's review status |
| `zuano skill publish <path>` | gateway | Alias for `zuano publish` scoped to skills |
| `zuano agent publish <path>` | gateway | Publish an agent definition |
| `zuano mcp publish [--descriptor]` | gateway | Publish an MCP server descriptor |
| `zuano mcp list [--json]` | gateway | List catalog `mcp_server` resources (read-only) |
| `zuano mcp inspect <catalog-id> [--json]` | gateway | Show the permitted metadata of one catalog MCP server (read-only) |
| `zuano skill search [query]` | gateway | Search the catalog for installable skills |
| `zuano skill info <catalog-id\|slug> [--json]` | gateway | Show a catalog skill, the connectors it requires and whether you have connected them; `--json` prints one document, the text goes to stderr |

### Skills

| Command | Kind | Description |
|---|---|---|
| `zuano skill install <path>` | local | Install a skill from a local directory/file into one or more providers |
| `zuano skill install <catalog-id> [--yes\|--no-connect] [--ignore-requirements]` | gateway | Install a skill by catalog id/slug into one or more providers, after checking the connectors it needs |
| `zuano skill migrate <name> --from <p> [--to <p>...]` | local | Project an installed skill from one provider to another |
| `zuano skill list [--json]` | local | List installed skills across providers |
| `zuano skill diff <name> [--json]` | local | Diff a skill's projection across providers |
| `zuano skill scan <name> [--json]` / `zuano skill score <name>` | local | Inspect a skill's manifest/fidelity; `--json` prints one document, as for `skill install` and `skill migrate` |
| `zuano skill uninstall <name>` | local | Remove an installed skill |
| `zuano skill rollback [manifest-entry-id]` | local | Revert to a prior manifest entry |
| `zuano skill evals sets list\|show\|create\|import\|export\|delete` | gateway | Manage a skill's eval sets |
| `zuano skill evals case add\|remove` | gateway | Add or remove a trigger or task case |
| `zuano skill eval <skill> [--set] [--version\|--draft] [--repeats] [--wait] [--fail-on-regression]` | gateway | Score a version or draft against an eval set (exit 20 failed run or nothing verified, 21 regression) |
| `zuano skill compare <skill> --versions a,b` | gateway | Score several versions or drafts side by side with significance tests |
| `zuano skill optimize <skill> --kind description\|instructions [--wait]` | gateway | Rewrite the description or instructions into a scored draft |
| `zuano skill drafts list\|show\|promote\|discard` | gateway | Review optimization drafts and publish one through the publish pipeline |
| `zuano skill quality <skill>` / `--all` | gateway | Quality score and history by version |

### Agents

| Command | Kind | Description |
|---|---|---|
| `zuano agent install <path\|catalog-id> [--to <p>...]` | local/gateway | Install a local Claude Markdown agent or a governed catalog bundle into one or more agent providers; supports history and rollback |
| `zuano agent migrate <name> --from <p> [--to <p>...]` | local | Project an installed agent from one provider to another (claude, opencode, codex, cursor, gemini-cli) |
| `zuano agent list` | local | List installed agents across agent providers |
| `zuano agent diff <name>` | local | Diff an agent's projection across providers |
| `zuano agent scan <name>` | local | Inspect an agent for dangerous shell patterns/hardcoded secrets |
| `zuano agent uninstall <name>` | local | Remove an installed agent |
| `zuano agent rollback [manifest-entry-id]` | local | Revert to a prior agent manifest entry |

### Diagnostics and recovery

| Command | Kind | Description |
|---|---|---|
| `zuano providers` | local | List supported providers and their detection status |
| `zuano doctor [--json]` | local | Check provider directories, installed skills and agents, synced MCP servers/hooks/rules/commands and saved routes, recovery state and gateway health |
| `zuano history` | local | List recorded write transactions and their state |
| `zuano recover [id]` | local | Finish or undo an interrupted write: a journal still writing is rolled back, one recording its ledger update is rolled forward |
| `--offline` (root flag) | — | Force every gateway command in this invocation to fail closed |
| `--timeout` (root flag) | — | Limit for each gateway request (see `ZUANO_TIMEOUT`) |
| `--version` | — | Print the CLI version, commit, and build date |

Run `zuano <command> --help` for full flag documentation on any command.

## Configuration & environment

Persistent config lives at `~/.config/zuano/config.json`. Manage it with
`zuano config set-gateway` / `get-gateway` / `show`, or override per-invocation
with environment variables — env always wins over the config file:

| Variable | Purpose |
|---|---|
| `ZUANO_URL` | Gateway base URL (overrides the config file) |
| `ZUANO_API_KEY` | Non-interactive bearer auth — use in scripts/CI instead of `zuano login`. Sent only to the gateway named by `ZUANO_API_KEY_ORIGIN`, or else the one saved in the config file |
| `ZUANO_API_KEY_ORIGIN` | The one gateway origin `ZUANO_API_KEY` may be sent to, e.g. `https://app.zuano.ai`. Needed in CI when no gateway is saved; a `--gateway-url` or `$ZUANO_URL` for another origin is refused |
| `ZUANO_EMAIL` | Selects which keychain-stored session to use (overrides the last `zuano login`'d identity) |
| `ZUANO_AGENT` | `1` makes zuano use the agent profile's session, which the gateway holds to approvals, and never your own; `zuano login` then signs the agent profile in. `zuano sync` sets it for the agents it configures |
| `ZUANO_TIMEOUT` | Limit for each gateway request, e.g. `90s` or `10m`; `0` means no limit. Defaults: 30s, 150s for tool calls and uploads. `--timeout` overrides it. A request that hits the limit exits with status 4 |

Interactive sessions authenticate via `zuano login` (OAuth, tokens stored in
the OS keychain, auto-refreshing). Non-interactive contexts should set
`ZUANO_URL` + `ZUANO_API_KEY` instead.

## License

[Apache-2.0](LICENSE)

## Gateway compatibility

Skill search uses `GET /api/catalog/skills/search?query=...`, independent of aggregate MCP. Older gateways need upgrading for search.
Catalog agent installation requires `X-Resource-Type: agent`, catalog identity and version headers, a verified checksum, and scan verdict. Bundles containing resources the flat agent format cannot preserve are rejected. Local agent installation accepts a Claude Markdown file and works without gateway credentials.
Source format provenance remains Claude for downloaded agents so tool and model translation remain correct; catalog id and version separately record catalog origin.

Provider formats were checked against [Codex custom agents](https://learn.chatgpt.com/docs/agent-configuration/subagents), [Cursor subagents](https://cursor.com/docs/subagents), and [Gemini CLI subagents](https://geminicli.com/docs/core/subagents/). The adapters support the fields documented in [agent formats](docs/agent-formats.md) and refuse migrations that cannot preserve execution restrictions.

`zuano authorize <service> --tool <tool>` requests access to gateway tools.
Repeat `--scope` for multiple tools and use `--account` when selecting a linked
account. The command polls for approved grants and required credentials, with a
default timeout of ten minutes (`--wait-timeout`, maximum thirty minutes). Tool grants
do not verify the upstream provider's OAuth scopes; connect the required account
in the gateway.

### Diagnostics and format fidelity

`zuano doctor [--json]` checks provider directories, current installed hashes and
permissions, pending recovery journals, override validation and gateway/session
health. For every detected coding agent it also checks each synced artifact kind
(MCP servers, agent environment, hooks, rules, commands) at the user and project scope as a
`sync:<scope>:<agent>:<kind>` check: in sync, not synced yet, out of sync,
edited since zuano wrote it, or blocked by a config file zuano cannot parse. It
verifies the sync sources parse and that environment variables referenced by MCP
entries (`mcp-env:<VAR>`) are set. `--offline` skips network checks. Directory
permission checks inspect mode bits, not ACLs or an actual write probe.

`zuano mcp inspect <catalog-id> [--json]` shows source, transport, authentication
mode/scope and declared tool count. Commands, endpoint URLs and credential
reference values are excluded from inspection output.

See [agent formats](docs/agent-formats.md) for supported fields and refusal rules,
and [agent config formats](docs/reference/agent-config-formats.md) for the
files `zuano sync` writes for each coding agent.
