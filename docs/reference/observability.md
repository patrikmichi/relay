# Request ids, trace and audit export

Every request zuano sends carries `X-Request-Id: cli_<ulid>`, which the gateway
records as the client's correlation id. Every response may carry the
gateway's own `X-Request-Id: req_<ulid>`. When a call fails, the message ends
with `request id: req_...`; `--debug` prints the id of every response, so a
successful call can be traced too.

```
$ zuano --debug call stripe list_charges
zuano: POST /api/stripe/mcp -> 200, request id req_01J9ZW8K...
```

## zuano trace

`zuano trace <request_id|trace_id|approval_id> [--org ID] [--json]` looks one
id up and prints what the gateway linked to it: the spans as a tree, the audit
entries and any errors. It needs an organisation admin or owner; anyone else
gets a message saying so. `--org` defaults to the organisation the credential
belongs to.

```
Request: req_01J9ZZ...
Trace:   4bf92f3577b34da6a3ce929d0e0e4736

Spans (2)
  tool_call stripe.create_refund [tool_call]  pending_approval  412ms
    approval [approval]  pending  3ms

Audit (1)
  2026-10-16T12:00:00.400Z  tool_call  stripe.create_refund  outcome=pending_approval  risk=destructive  user=dev@example.com  approval=apr_1

Errors (0)
```

## zuano audit export

`zuano audit export --from 2026-10-01 --to 2026-10-16 --format csv -o october.csv`
downloads the audit events for the range. Filters: `--user EMAIL`,
`--service`, `--tool`, `--outcome`. The format is `jsonl` (one event per line,
every field the gateway sends) or `csv` (a fixed set of columns).

The gateway serves events a page at a time (200 per page) with an opaque
cursor. With `-o`, zuano appends each page to the file and records the cursor
in `october.csv.cursor`. If the export stops, rerun the same command with
`--resume`: it checks the dates and filters match, continues from the saved
cursor and appends. A page that was written but whose cursor was not saved
yet may appear twice. The cursor file is removed when the export finishes.
Without `-o` the events go to stdout and there is nothing to resume.

CSV cells beginning with `=`, `+`, `-`, `@`, a tab or a carriage return are
prefixed with `'`, so a spreadsheet shows them as text instead of running
them.

## zuano doctor

`zuano doctor` adds two checks that need no login. `health` reads
`GET /api/health`: ready, ready but degraded (the failing non-critical
checks are named), or not ready (HTTP 503, which fails the doctor). `status-page`
prints the `statusPageUrl` the gateway publishes in `GET /api/version`, and
ignores anything that is not an http or https URL.

The gateway routes these commands call are listed in
[gateway-contracts.md](gateway-contracts.md#observability).
