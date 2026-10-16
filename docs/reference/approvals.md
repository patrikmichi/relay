# Agent profile and approvals

A gateway can hold what an autonomous agent writes until a person approves it.
The gateway decides what is an agent: a session is one when it was marked at
issue time. This page covers the client side: signing in as an agent,
telling the two sessions apart, deciding and collecting held calls, and what
`relay sync` sets up. The gateway's rules and endpoints are in its own
approvals documentation.

## The two profiles

| | Human profile | Agent profile |
|---|---|---|
| Sign in | `relay login` | `relay login --agent`, or `RELAY_AGENT=1 relay login` |
| Token request | no marker | `agent=1` on `POST /api/cli/token`, `agent: true` on the device-code poll |
| Keychain account | `oauth-refresh-token:<gateway>:<email>` | `oauth-refresh-token:agent:<gateway>:<email>` |
| Config file field | `email` | `agentEmail` |
| Used when | `RELAY_AGENT` is unset | `RELAY_AGENT` is `1`, `true`, `yes` or `on` |
| Writes | run | wait for approval (reads run) |
| `relay approvals` | list, show, approve, deny, wait | wait only; approve and deny are refused |

Both can be signed in to the same gateway and account at once. With
`RELAY_AGENT` set, relay uses the agent session and nothing else: if there is
none it stops with `no agent session` and never falls back to your own. The
agent identity defaults to your human one, so `relay login --agent` as the same
person is enough; `RELAY_EMAIL` still overrides both.

`relay whoami` prints which profile answered:

```
Profile:  agent (RELAY_AGENT=1; writes wait for approval)
Email:    dev@example.com
```

`relay logout` and `relay tokens revoke` act on the profile in use, so
`RELAY_AGENT=1 relay logout` signs out the agent and leaves your own session.
`GATEWAY_API_KEY` is used as given in either profile; whether it is held to
approvals depends on how the gateway issued the key.

## What the profile does not do

The marker is chosen when the session is issued. It is not proof of who holds
it. A process that can run `relay login` without `--agent` as the same user on
the same machine gets an unrestricted session, and one that can read the OS
keychain can read your own. The agent profile and `relay sync` below make the
default path safe, but they are not a sandbox. Closing the gap needs the human
session to live where the agent cannot reach it; keylatch RH-7 is the fix.

## Held calls

A call the gateway holds is not an error. `relay call <service> <tool>` and
`relay <service> <tool>` recognise it in the MCP result (`structuredContent`
with `status: "pending_approval"`) or in a REST `202`, print what is needed to
find it and exit with status 12:

```
Approval required: this call is held until a human decides it.
  approval: apr_3f9c4e
  call:     stripe create_refund
  risk:     destructive
  expires:  2026-10-16T12:15:00.000Z
Collect the result with `relay approvals wait apr_3f9c4e`, or repeat the call with --wait.
```

With `--json` the raw response is printed instead and the status is the same.

`--wait[=SECONDS]` (a bare `--wait` waits 300 seconds; a duration such as
`90s` also works) waits for the decision, runs the approved call once and
prints its result. With `--json` only the final response is printed.

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | approved and the call ran |
| 1 | the call failed after approval, the approval is unknown to this session, or an error such as a failed request |
| 4 | a request hit relay's time limit |
| 10 | denied |
| 11 | expired (nobody decided in 15 minutes) |
| 12 | still pending: the call was held and not waited for, or the timeout ended first |

These apply to `relay approvals wait`, to a held `relay call` and to a call run
with `--wait`. An outcome that was printed adds no error message of its own.

## relay approvals

| Command | Request |
|---|---|
| `relay approvals list [--status S] [--json]` | `GET /api/approvals?status=S`; `S` is `pending` (default), `approved`, `denied`, `executed`, `failed`, `expired` or `all` |
| `relay approvals show <id> [--json]` | `GET /api/approvals/{id}`: the approval and a preview of its arguments |
| `relay approvals approve <id> [--grant MINUTES] [--note TEXT] [--json]` | `POST /api/approvals/{id}/approve` |
| `relay approvals deny <id> [--note TEXT] [--json]` | `POST /api/approvals/{id}/deny` |
| `relay approvals wait <id> [--timeout SECONDS] [--json]` | `POST /api/approvals/{id}/await`, repeated while the status is `pending` |

`--json` prints the response body unchanged. `--grant` (1 to 480) applies to a
write approval: it also lets the same agent session write to the service for
that long. `wait --timeout` takes seconds or a duration (`0` asks once; the
default is 300 seconds) and shadows the root `--timeout`, which limits a single
request.

Deciding needs an org owner, admin or listed approver with an unmarked
session. `approve` and `deny` refuse the agent profile before sending anything,
as the gateway does, so run them without `RELAY_AGENT`. `wait` is the other
way round: only the session that made the call can collect it, so run it with
the same `RELAY_AGENT` setting.

## relay explain

`relay explain call <service> <tool>` adds what an agent session needs first:

```
stripe create_refund: destructive -> ask (approval)
stripe update_customer: write -> ask (grant)
stripe list_charges: read -> allow
```

`approval` means a person approves each call (destructive and outward tools),
`grant` means a person approves the call or opens a time-boxed grant for the
service (writes). Reads and tools the catalog does not list name nothing: an
org rule can still hold a call, and the catalog cannot show rules. `--json`
carries the same value in `approval`.

## relay sync

`relay sync` makes the coding agents it configures use the agent profile:

- The `env` kind sets `RELAY_AGENT=1` in the environment an agent gives the
  commands it runs: Claude Code `~/.claude/settings.json` `env`, and Codex
  `~/.codex/config.toml` `[shell_environment_policy.set]` (user scope only).
  Other agents have no documented environment setting and are reported as
  unsupported in `relay sync --matrix`. A Codex include-pattern allowlist can
  still strip the variable.
- Stdio MCP servers whose command is `relay` or `gw` get `RELAY_AGENT=1` in
  their `env` in each agent's config. The server list saved by `relay mcp
  install` is not changed.
- The gateway MCP entry authenticates with OAuth, whose sessions the gateway
  always marks as an agent's. With `GATEWAY_API_KEY` set, the default
  `--mcp-auth auto` writes a reference to that key instead, which carries no
  marker; the sync warns, and `--mcp-auth oauth` avoids it.
- The sync warns when it sets `RELAY_AGENT` before any `relay login --agent`.
  Until then relay run by an agent reports `no agent session`.

Leave the `env` kind out (`relay sync --kind mcp,rules`) to keep an agent on
your own session.

## Environment

| Variable | Purpose |
|---|---|
| `RELAY_AGENT` | `1` selects the agent profile for this process; `relay login` then signs it in |
