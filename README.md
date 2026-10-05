# relay

`relay` is a CLI for managing AI agent skills (Claude, Codex, OpenCode,
Cursor, Cline, Gemini CLI, Windsurf — see [Skill provider
support](#skill-provider-support)) and agent definitions (Claude, opencode,
Codex, Cursor, Gemini CLI — see [Agent provider
support](#agent-provider-support)) across providers, and, when you're
connected to a gateway, for pulling skills/agents/MCP servers from a shared
catalog. Every command works fully offline against your local machine; a
handful of catalog commands need a gateway to reach. `relay providers` is
the live, authoritative list of loaded providers — it never goes stale as
new providers are added.

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

**Go install:**

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

# Requires a gateway: install a skill by catalog id/slug.
relay skill install pr-triage --to claude
```

```bash
# Fully offline: migrate an installed agent from one provider to another.
relay agent migrate reviewer --from claude --to opencode

# Requires a gateway: install an agent by catalog id/slug.
relay agent install res_abc123 --to claude
```

> `relay mcp list` reads the catalog's MCP-server resources (read-only).
> `relay mcp install`/`relay mcp remove` — writing an MCP-server registration
> into a per-provider client config — are deliberately NOT part of this CLI;
> see [MCP server support](#mcp-server-support) for why.

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

## MCP server support

`relay mcp list` is read-only: it prints the gateway catalog's `mcp_server`
resources (id, slug, name, registration source, current published version).
That's the full extent of MCP-server management this CLI ships.

`relay mcp install`/`relay mcp remove` are **deliberately NOT implemented.**
Unlike a skill or agent (both project onto a single markdown-shaped file per
provider), registering an MCP server means writing into a fourth, genuinely
different artifact kind — a per-provider client config file
(`~/.claude.json`, `~/.codex/config.toml`, `.cursor/mcp.json`, ...) — each
with its own schema and its own IR, so it's out of scope for this CLI;
`relay mcp list` gives you the id/slug to register a server manually today.

`relay sync` is **also NOT** a portable way to distribute MCP servers,
skills, or agents to non-Claude-Code providers — see the next paragraph.

## `relay sync` is Claude-Code-only — no `--to <provider>`

`relay sync` materializes a Claude-Code-specific plugin **marketplace**
(`marketplace.json` + plugin bundles + a managed-settings fragment) — a
distribution format with no analogue in codex/cursor/gemini-cli/opencode.
`relay sync --to <provider>` is rejected outright rather than silently
no-op'ing or guessing at an unsupported projection:

```
$ relay sync --to cursor
relay sync --to cursor is not supported: sync materializes a
Claude-Code-specific plugin marketplace with no per-provider equivalent —
use `relay skill install <catalog-id> --to cursor` and
`relay agent install <catalog-id> --to cursor` instead
```

The portable path to the other providers is per-artifact catalog install:
`relay skill install <catalog-id> --to <provider>` and
`relay agent install <catalog-id> --to <provider>`.

## The offline-vs-gateway model

Every `relay` command falls into exactly one of two buckets:

| | LOCAL | GATEWAY |
|---|---|---|
| Touches | Only files on this machine (`~/.claude/skills/`, `~/.claude/agents/`, `~/.codex/`, `~/.cursor/`, `~/.config/opencode/`, `~/.gemini/`, and relay's own manifest) | A configured gateway over HTTP |
| Needs auth | No | Yes — `relay login` or `GATEWAY_API_KEY` |
| Examples | `relay skill migrate`, `relay skill install <path>`, `relay skill list`, `relay skill diff`, `relay skill scan`, `relay skill uninstall`, `relay skill rollback`, `relay agent migrate`, `relay agent list`, `relay agent diff`, `relay agent scan`, `relay agent uninstall`, `relay agent rollback`, `relay providers` | `relay skill install <catalog-id>`, `relay skill search`, `relay agent install <catalog-id>`, `relay mcp list`, `relay publish`, `relay sync`, `relay services`, `relay call`, `relay help-tools`, `relay login`/`logout`/`whoami`/`authorize`/`tokens` |

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
| `relay authorize <service> --scope <tool>` | gateway | Request gateway tool access and verify grants and linked credentials |
| `relay tokens list` | gateway | Show active session info |
| `relay tokens revoke` | gateway | Revoke the current session token |
| `relay config set-gateway <url>` | local | Persist a gateway URL to `~/.config/relay/config.json` |
| `relay config get-gateway` | local | Print the effective gateway URL |
| `relay config show` | local | Print all resolved config as JSON |
| `relay services` | gateway | List services available on the configured gateway |
| `relay help-tools [service]` | gateway | List tools for one or all services |
| `relay call <service> <tool> [--arg k=v]` | gateway | Call a tool on a gateway service |
| `relay sync [--dir] [--dry-run]` | gateway | Pull your marketplace manifest into a local Claude Code plugin directory (Claude-Code-only — `--to <provider>` errors naming the portable alternative) |
| `relay publish <path> [--type] [--watch]` | gateway | Publish a skill/agent/MCP server/prompt/plugin to the catalog |
| `relay publish status <versionId> [--watch]` | gateway | Poll a publish's review status |
| `relay skill publish <path>` | gateway | Alias for `relay publish` scoped to skills |
| `relay agent publish <path>` | gateway | Publish an agent definition |
| `relay mcp publish [--descriptor]` | gateway | Publish an MCP server descriptor |
| `relay mcp list [--json]` | gateway | List catalog `mcp_server` resources (read-only; `install`/`remove` are not implemented) |
| `relay skill install <path>` | local | Install a skill from a local directory/file into one or more providers |
| `relay skill install <catalog-id>` | gateway | Install a skill by catalog id/slug into one or more providers |
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
| `GATEWAY_API_KEY` | Non-interactive bearer auth — use in scripts/CI instead of `relay login` |
| `RELAY_EMAIL` | Selects which keychain-stored session to use (overrides the last `relay login`'d identity) |
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

`relay authorize <service> --scope <tool>` requests access to gateway tools.
Repeat `--scope` for multiple tools and use `--account` when selecting a linked
account. The command polls for approved grants and required credentials, with a
default timeout of ten minutes (`--timeout`, maximum thirty minutes). Tool grants
do not verify the upstream provider's OAuth scopes; connect the required account
in the gateway.

### Diagnostics and format fidelity

`relay doctor [--json]` checks provider directories, current installed hashes and
permissions, pending recovery journals, override validation and gateway/session
health. `--offline` skips network checks. Directory permission checks inspect
mode bits, not ACLs or an actual write probe.

`relay mcp inspect <catalog-id> [--json]` shows source, transport, authentication
mode/scope and declared tool count. Commands, endpoint URLs and credential
reference values are excluded from inspection output.

See [agent formats](docs/agent-formats.md) for supported fields and refusal rules.
