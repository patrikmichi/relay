# Agent profile and approvals

A gateway can hold what an autonomous agent writes until a person approves it.
The gateway decides what is an agent: a session is one when it was marked at
issue time. This page covers the client side: signing in as an agent,
telling the two sessions apart, deciding and collecting held calls, and what
`zuano sync` sets up. The gateway's rules and endpoints are in its own
approvals documentation.

## The two profiles

| | Human profile | Agent profile |
|---|---|---|
| Sign in | `zuano login` | `zuano login --agent`, or `ZUANO_AGENT=1 zuano login` |
| Login request | `principal=cli_personal` on `GET /api/cli/authorize`, `{"principal":"cli_personal"}` in the `POST /api/cli/device/code` body | `principal=cli_agent`, `{"principal":"cli_agent"}`; the token requests carry no flag |
| Keychain account | `oauth-refresh-token:<gateway>:<email>` | `oauth-refresh-token:agent:<gateway>:<email>` |
| Config file field | `email` | `agentEmail` |
| Used when | `ZUANO_AGENT` is unset | `ZUANO_AGENT` is `1`, `true`, `yes` or `on` |
| Writes | wait for approval unless a time-boxed service grant covers them (reads run) | wait for approval (reads run) |
| `zuano approvals` | list, show, approve, deny (open the approval page), wait | list, show, wait; approve and deny only print the link |

Both can be signed in to the same gateway and account at once. With
`ZUANO_AGENT` set, zuano uses the agent session and nothing else: if there is
none it stops with `no agent session` and never falls back to your own. The
agent identity defaults to your human one, so `zuano login --agent` as the same
person is enough; `ZUANO_EMAIL` still overrides both.

`zuano whoami` prints which profile answered:

```
Profile:  agent (ZUANO_AGENT=1; writes wait for approval)
Email:    dev@example.com
```

`zuano logout` and `zuano tokens revoke` act on the profile in use, so
`ZUANO_AGENT=1 zuano logout` signs out the agent and leaves your own session.
`ZUANO_API_KEY` is used as given in either profile; whether it is held to
approvals depends on how the gateway issued the key.

## What an agent session cannot change

The kind is fixed when the login starts: `zuano login` prints which one it
requested (`Requesting an agent session` or `Requesting a personal session`),
because the browser flow shows nothing of its own. The token requests ignore
any flag, so a login started as one kind cannot be redeemed as the other.

An agent session reads connections, connectors and the marketplace, but the
gateway answers `403` when it changes them: `zuano connect` (credentials),
`zuano publish` and the skill eval, optimize and promote commands. zuano prints
the gateway's reason and suggests the way out: run the command with your
personal profile (unset `ZUANO_AGENT`), or use the MCP tools, where a person
approves the call.

## What the profile does not do

`ZUANO_AGENT` is a selector, not a security boundary: it only picks which
stored session zuano uses. What keeps a process running as you from approving
its own calls is that the CLI cannot decide anything. A decision is a WebAuthn
assertion with user verification, made in a browser session, and the gateway
refuses a decision from any bearer token. `zuano approvals approve` and
`deny` therefore only open the approval page. A personal CLI session is held to
the same approvals as an agent's: only a browser session is unrestricted. The
gateway's organisation policy decides which risk classes are off, need an
approval, or run.

## Held calls

A call the gateway holds is not an error. `zuano call <service> <tool>` and
`zuano <service> <tool>` recognise it in the MCP result (`structuredContent`
with `status: "pending_approval"`) or in a REST `202`, print what is needed to
find it and exit with status 12:

```
Approval required: this call is held until a human decides it.
  approval: apr_3f9c4e
  call:     stripe create_refund
  risk:     destructive
  expires:  2026-10-16T12:15:00.000Z
Personal CLI sessions follow the same approvals as agents; only a browser session runs writes unasked.
Decide it with `zuano approvals approve apr_3f9c4e` (opens the approval page), then collect the result with `zuano approvals wait apr_3f9c4e`, or repeat the call with --wait.
```

The second line is printed for a personal session only. With `--json` the raw
response is printed instead and the status is the same.

A call the organisation's risk policy turned off is not held: the gateway
answers with `reason: "policy_off"` and a sentence naming the policy source and
who can change it (`Blocked by org policy: stripe.create_refund (destructive)
is Off ... Can be changed by: org admins.`). zuano prints that sentence and
exits with status 10, the status of a denial. The reason is read from a JSON
body or from the `structuredContent` of an MCP tool error.

`--wait[=SECONDS]` (a bare `--wait` waits 300 seconds; a duration such as
`90s` also works) waits for the decision, runs the approved call once and
prints its result. With `--json` only the final response is printed.

## Approval pages in the session (URL elicitation)

Before its first call to an MCP endpoint zuano sends `initialize` declaring
`capabilities.elicitation.url`, and sends the `Mcp-Session-Id` the gateway
returns on every later request to that endpoint (a gateway that issues none is
used without one; a session it forgets, answered with `404`, is renewed once).
A gateway that sees the capability may hold a call by answering with the
JSON-RPC error `-32042` instead of the held-call result:

```json
{"error": {"code": -32042, "message": "...",
  "data": {"elicitations": [{"mode": "url", "elicitationId": "eid_1",
                              "url": "https://gateway.example/approvals/apr_1?e=eid_1",
                              "message": "Approve create_refund on stripe (destructive)"}],
           "relay": {"approval_id": "apr_1", "expires_at": "...", "next": "..."}}}}
```

With a person at the keyboard zuano then:

1. prints the message and the link with its host in bold and underline (and on
   a line of its own);
2. asks `Open it in your browser? [Y/n]` and opens it, unless `--no-browser`
   or the link is not a page on the gateway the call went to (it only warns
   about those);
3. polls `GET /api/approvals/{approval_id}` for up to 300 seconds, or the
   `--wait` limit, printing the outcome;
4. when approved, repeats the call once. If that is held again it exits 12.

A denied approval exits 10 and an expired one 11, as for `approvals wait`.
Without a person (an agent session, no terminal, or `--json`) zuano opens
nothing, prints the link and exits 12 straight away, and waits and retries only
when `--wait` is given. The approval id comes from `data.relay.approval_id`,
or from the `/approvals/<id>` path of the link.

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | approved and the call ran |
| 1 | the call failed after approval, the approval is unknown to this session, or an error such as a failed request |
| 17 | a request hit zuano's time limit |
| 10 | denied, or turned off by policy (`reason: "policy_off"`) |
| 11 | expired (nobody decided in 15 minutes) |
| 12 | still pending: the call was held and not waited for, or the timeout ended first |
| 13 | a metered quota is used up (`429` with `error: "quota_exceeded"`) |
| 14 | a passkey step-up is needed and nobody could give it; see [step-up.md](step-up.md) |
| 30 to 33 | the plan does not allow it; see [billing.md](billing.md#exit-statuses) |

Statuses 10 to 12 apply to `zuano approvals wait`, to a held `zuano call` and
to a call run with `--wait`. 13, 14 and 30 to 33 apply to every command that
talks to the gateway. An outcome that was printed adds no error message of its
own.

## zuano approvals

| Command | Request |
|---|---|
| `zuano approvals list [--status S] [--json]` | `GET /api/approvals?status=S`; `S` is `pending` (default), `approved`, `denied`, `executed`, `failed`, `expired` or `all` |
| `zuano approvals show <id> [--json]` | `GET /api/approvals/{id}`: the approval and a preview of its arguments |
| `zuano approvals approve <id> [--no-browser] [--wait] [--wait-timeout T] [--json]` | `GET /api/approvals/{id}`, then opens `<gateway>/approvals/{id}?decision=approve` |
| `zuano approvals deny <id> [--no-browser] [--wait] [--wait-timeout T] [--json]` | `GET /api/approvals/{id}`, then opens `<gateway>/approvals/{id}?decision=deny` |
| `zuano approvals wait <id> [--wait-timeout SECONDS] [--json]` | `POST /api/approvals/{id}/await`, repeated while the status is `pending` |

`list` and `show` with `--json` print the response body unchanged. `wait
--wait-timeout` takes seconds or a duration (`0` asks once; the default is 300
seconds). The root `--timeout` limits a single request.

`approve` and `deny` send no decision. They look the approval up (so a wrong or
already decided id is reported before a browser opens, and the call is
summarised), then open the approval page. There the approver sees the whole
call, the policy that held it and its requester, chooses a grant duration for a
write, and confirms with a passkey. The page is the only place a decision is
made; no zuano command posts one.

- In a terminal zuano opens the default browser. With `--no-browser`, in an
  agent session (`ZUANO_AGENT`) or without a terminal on stdin, it prints the
  link and a QR code instead. Scan the code to decide on a phone with its own
  passkey, or open the link on a desktop whose browser offers the phone
  (cross-device passkey).
- `--wait` polls `GET /api/approvals/{id}` every two seconds until the approval
  is decided and exits 0 (approved or executed), 10 (denied), 11 (expired) or
  12 (still pending when `--wait-timeout` ended).
- `--json` prints one document: `{"approval_id","decision","url","opened","status"}`.
- The approval must exist and be `pending`; zuano refuses a decided one
  before opening anything.

`wait` is for the session that made the call: only that session can collect the
result, so run it with the same `ZUANO_AGENT` setting.

## zuano explain

`zuano explain call <service> <tool>` adds what an agent session needs first:

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

## zuano sync

`zuano sync` makes the coding agents it configures use the agent profile:

- The `env` kind sets `ZUANO_AGENT=1` in the environment an agent gives the
  commands it runs: Claude Code `~/.claude/settings.json` `env`, and Codex
  `~/.codex/config.toml` `[shell_environment_policy.set]` (user scope only).
  Other agents have no documented environment setting and are reported as
  unsupported in `zuano sync --matrix`. A Codex include-pattern allowlist can
  still strip the variable.
- Stdio MCP servers whose command is `zuano` get `ZUANO_AGENT=1` in
  their `env` in each agent's config. The server list saved by `zuano mcp
  install` is not changed.
- The gateway MCP entry authenticates with OAuth, whose sessions the gateway
  always marks as an agent's. `--mcp-auth auto` (the default) and `oauth` use
  it even when `ZUANO_API_KEY` is set; Cline and GitHub Copilot cannot sign
  in from the file, so no credential is written and the report tells you to
  authorize the server in the app. Only `--mcp-auth api-key` references the
  key, which carries no marker and bypasses approvals; the sync warns.
- The sync warns when it sets `ZUANO_AGENT` before any `zuano login --agent`.
  Until then zuano run by an agent reports `no agent session`.

Leave the `env` kind out (`zuano sync --kind mcp,rules`) to keep an agent on
your own session.

## Chat

When your organisation sends approvals to Slack or Teams, `zuano approvals show`
lists where an approval was sent and how far it got. The card in chat only opens
the approval page; the decision is still made there with your passkey.
`zuano approvals routes list|show|test` reads the routing rules, and `zuano chat`
shows and links your chat accounts. See [chat.md](chat.md).

## Environment

| Variable | Purpose |
|---|---|
| `ZUANO_AGENT` | `1` selects the agent profile for this process; `zuano login` then signs it in |

## Session trusts

When a person approves a call they can also trust the same kind of call for the
rest of that session for a while ("only this call", "trust this for the session"
or "all writes on the service for the session"). That is decided on the approval
page with a passkey; there is no command that creates a trust. These list and
end them:

| Command | |
|---|---|
| `zuano approvals trusts list [--org]` | active trusts: the agent, the tool or class of tool, what the trust is limited to, who gave it, when it ends and how often it was used; `--org` lists the whole organisation and needs an administrator (a 403 is exit 10) |
| `zuano approvals trusts revoke <id>` | end one now; the next call of that kind needs an approval again |
| `zuano approvals trusts revoke-all [--org] [--yes]` | end all yours, or all the organisation's |

Ending a trust only tightens things, so an agent session may do it.
