# Sessions, audit and policy

Read-mostly views of the gateway's security state. Changing a policy, enrolling
a passkey or enabling strict mode is done in the dashboard, where a passkey
confirms it.

## zuano sessions

| Command | Request |
|---|---|
| `zuano sessions list [--json]` | `GET /api/me/sessions`: your browser and CLI sessions; the one this CLI uses is marked |
| `zuano sessions revoke <id>` | `DELETE /api/me/sessions/{id}` |
| `zuano sessions revoke-all` | `POST /api/me/sessions/revoke-all`: every session, CLI refresh token and OAuth family, this one included |

`revoke-all` needs a passkey confirmation. The gateway asks for it with a
step-up (see [step-up.md](step-up.md)); zuano opens the page, waits and retries,
or exits 14 without a person at the keyboard.

## zuano audit

Both commands need an organisation admin.

| Command | Request |
|---|---|
| `zuano audit verify [--from D] [--to D] [--json]` | `GET /api/admin/audit/verify?from=&to=` |
| `zuano audit export [--from D] [--to D] [--format jsonl\|csv] [--user EMAIL] [--service] [--tool] [--outcome] [-o FILE] [--resume] [--org ID]` | `GET /api/org/{org}/observability/audit?...&cursor=` |

`verify` asks the gateway to recompute the hash chain and check the signed
daily checkpoints. It exits 0 when the log verifies and 1 when it does not,
naming the first broken event. `export` pages through the events and writes
them to stdout, or to `-o FILE`, which must not exist yet and is created with
mode 0600. A file export keeps its position in `FILE.cursor`; if it stops,
the same command with `--resume` continues and appends. CSV cells that start
with `=`, `+`, `-` or `@` are prefixed with a quote so a spreadsheet does not
run them. See [observability.md](observability.md).

## zuano policy

`zuano policy explain <service> <tool> [--arg k=v] [--arg-json k=<json>] [--principal KIND] [--json]`
calls `GET /api/admin/risk-policy/preview?service=&tool=&params=<json>&principal=`
and prints the decision the organisation's risk matrix, the tool's overrides and
the platform blocks give that call:

```
stripe.create_refund (destructive) is Off for cli_personal
  from the built-in default
  Can be changed by: org admins
```

The principal defaults to the session's: `cli_agent` in an agent session,
`cli_personal` otherwise. `--arg` and `--arg-json` give the arguments an
override's threshold depends on.

By default destructive and outward calls are off for everyone and writes need
an approval for every CLI session; an admin changes a cell in the dashboard.
The command is read-only.
