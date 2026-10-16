# Agent inventory and access reviews

The gateway keeps an inventory of every identity that can call tools: MCP
clients, CLI sessions, API keys, triggers, CI workflows and the coding agents on
your machines. `zuano agents` reads it and stops an identity; `zuano reviews`
lists access review campaigns and exports their evidence.

## Commands

| Command | Request |
|---|---|
| `zuano agents list [--kind K] [--status S] [--owner EMAIL] [--label k=v] [--stale] [--exposure low\|medium\|high] [--can svc[.tool] [--risk R] [--mode M]] [--json]` | `GET /api/agents` |
| `zuano agents show <id>`, `activity <id> [--from DAY] [--to DAY]` | `GET /api/agents/{id}`, `/activity` |
| `zuano agents suspend <id> --reason T`, `resume <id>`, `revoke <id> --reason T [--yes]` | `POST /api/agents/{id}/suspend\|resume\|revoke` |
| `zuano agents devices`, `shadow-mcp` | `GET /api/agents/devices`, `/shadow-mcp` |
| `zuano agents quarantine apply [--dry-run]` | `GET /api/agents/shadow-mcp/quarantine`, then `POST .../actions` |
| `zuano agents quarantine list [--json]` | none: what is quarantined on this machine |
| `zuano agents quarantine restore (<id> \| --all)` | none; reports the restore |
| `zuano reviews list\|show <id>`, `export <id> [--format csv\|json] [--out F]`, `decide <id>` | `GET /api/access-reviews[/{id}[/export]]`; `decide` opens `/reviews?campaign={id}` |
| `zuano whoami` | `GET /api/cli/whoami`; also prints `Identity: <id> (<status>)` |

The shapes are in [gateway-contracts.md](gateway-contracts.md).

`suspend`, `resume` and `revoke` change what an identity may do. They are for a
person: refused while `ZUANO_AGENT` is set (exit 10). `resume` and `revoke` need
a passkey confirmation (exit 14 when nobody can give it); `revoke` also asks
first (`--yes` skips it) because it ends the identity's credentials, cancels its
pending approvals and stops its running work. A request from a suspended,
revoked or expired identity ends with exit 33. Deciding a review item is a
passkey action in the browser: `reviews decide` only opens the page (exit 14 for
an agent session).

## Quarantining shadow MCP servers

An MCP server somebody added to an agent on their own, that is neither the
organisation's connector nor routed through the firewall, shows up in
`agents shadow-mcp`. By default that is only a report. An administrator can
choose, org-wide and with a passkey, to **quarantine** them: `zuano sync` (unless
`--no-quarantine`) then takes each listed server out of the agent's config.

- Only servers the person wrote are touched, never one zuano manages, and only
  while the entry still matches the hash it was reported with; a changed entry
  has to be reported and judged again.
- Nothing is deleted silently: a copy of the config file as it was and the
  server itself are kept in a private directory (`~/.config/zuano/quarantine`),
  and `agents quarantine restore` puts a server back. While the organisation
  still lists it, the next sync quarantines it again.
- The gateway is told which agent, server, config path (with your home
  directory shown as `~`) and entry hash were touched, for the audit log.
- Claude Code, Cursor, Gemini CLI, OpenCode, Windsurf, Cline, GitHub Copilot
  (JSON configs) and Codex (TOML tables) are covered. A config that is not
  valid JSON is left alone and reported as an error.
- It is a person's act: refused while `ZUANO_AGENT` is set (exit 10), and an
  agent session's `sync` never quarantines.

## This machine

`zuano` keeps an install id in `~/.config/zuano/install.json`: 128 random bits
that tell the gateway which machine a session came from. It is not a credential
and grants nothing; it is sent when you sign in and in the device report.

After a `zuano sync` that wrote files, and when you are signed in as yourself,
zuano sends the gateway a **device report**: the CLI version, the operating
system, a host label, the coding agents installed here, whether each has the
gateway's MCP server, and for each MCP server its name, transport, the host of a
remote URL or the program name of a local command, and whether it goes through
the firewall. It never sends arguments, environment values or headers, and an
agent session never sends it. `--host-label NAME` sets the label (an empty
value sends none) and is remembered; by default it is the host name.

The gateway's MCP entry written into Claude Code, Cursor, Codex, Gemini CLI,
OpenCode and Copilot also carries the header `X-Zuano-Install: <install id>`,
which the gateway uses for display only. Agents whose config cannot carry
headers simply lack it.

`zuano doctor` has an `install` row with the install id and the time of the
last device report.
