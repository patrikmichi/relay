# Gateway contracts zuano assumes

zuano is built against these gateway routes and response shapes. The gateway
does not serve all of them yet; each section says where its shape comes from
(a spec, or a decision of this client where the spec leaves the shape open).
The gateway's contract tests should produce the documents in
`internal/cli/testdata/contracts/`, which zuano's own tests read.

## Contract fixtures

zuano's tests read each gateway document from a file. By default that is the
vendored copy in `internal/cli/testdata/contracts/`. Set
`ZUANO_CONTRACT_FIXTURES` to a directory of the gateway's own fixtures
(`gateway/contract-fixtures`) and the tests read every file from there
instead; a file missing from that directory fails the test. The CI job
`gateway-contract` runs the `Contract` tests that way against the gateway's
`main` branch.

The documents the contract tests read: `approval-preview.json`,
`approval-full.json`, `approval-args-gone.json`, `mcp-pending-approval.json`,
`skill-requirements.json`, `eval-run.json`, `eval-set.json` and
`marketplace-manifest.json`. Ids and timestamps are stable placeholders, so a
fixture changes only when a route's shape does.

`zuano docs gen --out DIR` (hidden) writes one Markdown page per command, for
the gateway's docs site to pull in.

Conventions: a field name in `snake_case` follows the approvals and runtime
routes, `camelCase` follows the billing, evals and entitlement types. A field
zuano does not read may be added at any time; a field zuano reads must not
change its meaning.

## Refusals every command understands

Any gateway response, on any route, may be one of these. zuano maps them in
its HTTP client, so no route needs to handle them separately.

| Response | Client behaviour | Exit |
|---|---|---|
| `402` `{"error": CODE, "message", "feature"?, "limit"?, "meter"?, "current"?, "max"?, "current_plan", "required_plan", "upgrade_url", "resets_at"?}` with CODE `plan_required`, `limit_reached` or `verification_required` | prints the message, plans and `upgrade_url` | 30 |
| `402` with CODE `quota_exceeded` or `spend_cap_reached` | same | 31 |
| `503` `{"error": "billing_unavailable", "message"}` with `Retry-After` | prints the message and the wait | 32 |
| `403` `{"error": "org_suspended" \| "seat_suspended", "message"}` | prints the message | 33 |
| `429` `{"error": "rate_limited", "message"}` with `Retry-After` | prints the message and the wait | 13 |
| `429` `{"error": "quota_exceeded", "message", "metric", "limit", "resets_at"}` | prints metric, limit and reset | 13 |
| `401` `{"error": "step_up_required", "step_up_id", "step_up_url"}` | completes the step-up, see below | 14 when nobody can |
| `401` `{"error": "reauth_required"}` | treated like an expired token: `zuano login` | 1 |
| `200`/`403` body or MCP `structuredContent` `{"reason": "policy_off", "message"}` | prints the message | 10 |

Source: billing plan section 5.8 (codes, statuses, JSON, exit 30 to 33),
scalability plan sections 5.9 and 8 (`429` quota, exit 13), risk policy plan
sections 5.2 and 5.14 (`policy_off`, exit 10; `step_up_required`, exit 14).
A plan quota is `402` `quota_exceeded` (exit 31); a rate limit is `429`
`rate_limited` (exit 13). A metered `429` `quota_exceeded` is also accepted and
exits 13. `limit` is a limit key (text) in a plan refusal and a number in a
quota refusal; zuano reads either.

Every response may carry `X-Request-Id: req_<ulid>`. zuano sends its own
`X-Request-Id: cli_<ulid>` on every request as client correlation, and adds the
gateway's id to each refusal and failed-call message as `request id: req_...`.
`--debug` prints the id of every response.

## Login

| Request | Carries |
|---|---|
| `GET /api/cli/authorize` | `principal=cli_personal` or `cli_agent`, with the existing PKCE parameters; no `agent` parameter |
| `POST /api/cli/device/code` | `{"principal": "cli_personal" \| "cli_agent"}`; no `agent` field |
| `POST /api/cli/token`, `POST /api/cli/device/token`, `POST /api/cli/refresh` | no principal: it is fixed at issue |

Device binding (risk policy plan 5.5): the token requests also carry a `DPoP`
proof header (no `ath`) and `dpop_key_type` (`hardware` or `software`, form
field or JSON field). The response's `token_type` is `DPoP` when the session is
bound to the key and `Bearer` otherwise; `400 {"error": "hardware_key_required"}`
refuses a software key. `POST /api/cli/refresh` carries a proof too. Bound
requests use `Authorization: DPoP <token>` and a `DPoP` proof with `htm`,
`htu`, `iat`, `jti`, `ath`. `dpop_key_type` and the refusal code are this
client's decision.

`GET /api/me/passkeys` returns `{"passkeys": [{"id", "name", "created_at",
"last_used_at": null|text, "backed_up"}]}` (fixture `passkeys.json`) and
`GET /api/cli/whoami` may add `"strict_mode": true`; `zuano doctor` reads both.

Source: risk policy plan 5.1 and 5.14. A credential without a principal is
answered `401 {"error": "reauth_required"}`; zuano does not refresh such a
session and tells the person to run `zuano login`.

## Approvals

| Route | Use |
|---|---|
| `GET /api/approvals?status=S` | unchanged: `{"approvals": [...]}` |
| `GET /api/approvals/{id}` | unchanged: `{"approval": {...}, "args_preview": ...}`; Bearer allowed |
| `POST /api/approvals/{id}/await` | unchanged |
| page `GET /approvals/{id}?decision=approve\|deny` | where a person decides with a passkey; zuano opens it and never posts a decision |

zuano calls no `approve`, `deny`, `challenge` or `decide` route. The gateway
must reject a Bearer decision (risk policy plan 5.3).

## MCP sessions and URL elicitation

| Exchange | Detail |
|---|---|
| `POST /api/{service}/mcp`, `/api/mcp` `initialize` | request `params`: `protocolVersion: "2025-11-25"`, `capabilities: {"elicitation": {"url": {}}}`, `clientInfo: {"name": "zuano", "version"}`. The response may carry `Mcp-Session-Id`. zuano then sends `notifications/initialized` and echoes the header on every later request to that path. `404` for a known session id means the session is gone: zuano initializes again, once. Fixture: none (the document is the request) |
| `tools/call` error `-32042` | `data.elicitations[]` with `mode: "url"`, `elicitationId`, `url`, `message`; `data.relay.approval_id` (read; the `/approvals/<id>` path of `url` is the fallback), `expires_at`, `next`. Fixture: `mcp-url-elicitation.json` |
| `GET /api/approvals/{id}` | polled for the decision, as for `approvals wait` |
| `GET /api/org/{org}/connect-links` | `{"claude_ai": {"url", "directory_url"?, "endpoint", "steps": [text]}, "chatgpt": {"endpoint", "auth": text, "admin_url": null\|text, "steps": [text]}}`; org admins only. Fixture: `connect-links.json` |

Source: connectors plan sections 4.9, 4.11, 4.12 and 4.13. The layout of the
connect-links document, and the 300 second default wait, are this client's
decisions; the plan names the endpoint and the two targets.

## Step-up

| Route | Use |
|---|---|
| `GET /api/stepup/{id}` | `{"status": "pending" \| "approved" \| "denied" \| "expired"}`; Bearer allowed |
| page `GET {step_up_url}` | on the gateway's origin; the person confirms with a passkey |
| request header `X-Step-Up: {id}` | on the retried request; single use, bound to the request's digest |

Source: risk policy plan 5.4 and 5.14. The status document is this client's
decision; the plan names the route and the three states.

## Sessions

| Route | Response |
|---|---|
| `GET /api/me/sessions` | `{"sessions": [{"id", "kind": "dashboard\|viewer\|cli_personal\|cli_agent", "auth_method", "created_at", "last_seen_at", "idle_expires_at", "absolute_expires_at", "ip_prefix", "user_agent", "current"}]}`. Fixture: `sessions.json` |
| `DELETE /api/me/sessions/{id}` | `200` any JSON |
| `POST /api/me/sessions/revoke-all` | `200` any JSON; step-up gated |

Source: risk policy plan 5.6, 5.13 and 5.14. The session document follows the
`user_sessions` table; zuano lists CLI sessions beside browser ones, which the
plan asks for, so the gateway must include them.

## Audit

| Route | Response |
|---|---|
| `GET /api/admin/audit/verify?from=&to=` | `{"ok", "from", "to", "events", "checkpoints", "firstBadSeq": null\|int, "reason": null\|text}`. Fixtures: `audit-verify-ok.json`, `audit-verify-broken.json` |

Source: risk policy plan 5.10 and 5.13. The verify document is this client's
decision. Exporting events uses the paged route under Observability below;
zuano no longer calls `/api/admin/audit/export`.

## Observability

In these routes `{org}` is an organisation id or the literal `current`, meaning
the organisation the credential belongs to. All need an organisation admin or
owner; a member gets `403`. Fixture: `trace-lookup.json`.

| Route | Response |
|---|---|
| `GET /api/org/{org}/observability/lookup?id=` | `{"id", "matched": "request\|trace\|approval", "request_id", "trace_id", "spans": [SPAN], "audit": [EVENT], "errors": [ERROR]}`; `404` when the id is unknown or older than the retention |
| `GET /api/org/{org}/observability/audit?from=&to=&user=&service=&tool=&outcome=&limit=&cursor=` | `{"events": [EVENT], "next_cursor": null\|text}`; `limit` is at most 200; events come in a stable order, so a saved `next_cursor` can be used later |
| `GET /api/health` | public; `200` ready, optionally `{"degraded": [check names]}`; `503` not ready; `404` is read as "no health endpoint" |
| `GET /api/version` | public; `{"version", "statusPageUrl"?: text}` |

```json
SPAN  = {"span_id", "parent_span_id": null|text, "kind", "name", "start_ts", "duration_ms",
         "status", "service", "tool", "outcome"}
EVENT = {"ts", "event", "user_email", "service", "tool", "outcome", "risk", "duration_ms",
         "approval_id": null|text, "request_id", "trace_id"}   // other fields may be added
ERROR = {"ts", "level", "route", "message", "exception_type", "fingerprint"}
```

Source: observability plan sections 4.16 and 6 name the routes, filters and
cursor paging. The `current` organisation, the document layouts and paging the
export through the search route (instead of the streamed export route, which
cannot be resumed) are this client's decisions. zuano sends `X-Request-Id:
cli_<ulid>` and reads the gateway's `X-Request-Id`.

## Risk policy preview

`GET /api/admin/risk-policy/preview?service=&tool=&params=<json>&principal=`.
Fixtures: `policy-preview-off.json`, `policy-preview-approval.json`,
`policy-preview-platform.json`.

```json
{"service", "tool", "risk", "principal",
 "decision": {"mode": "off|approval|allowed",
              "source": "platform_block|org_tool_override|org_matrix|default",
              "ruleId": null|text, "predicate": null|text,
              "changeableBy": "platform|org_admin"}}
```

Source: risk policy plan 5.2 (`RiskDecision`) and 5.13 (the preview route). The
`principal` parameter and the wrapper are this client's decision; the plan says
the preview "explains the decision for a principal".

## Billing

`GET /api/billing/status` (Bearer, any member; a member without billing rights
gets `"view": "reduced"`). Fixture: `billing-status.json`,
`billing-status-simulated.json`.

```json
{"orgId", "orgSlug", "orgName", "view": "full|reduced",
 "plan": {"key", "displayName", "source": "default|stripe|admin|simulated",
          "status": "active|trialing|past_due|canceled|none",
          "trialEndsAt"?, "periodStart", "periodEnd", "cancelAtPeriodEnd"},
 "verified", "suspended", "overageEnabled", "spendCapMicros": null|int,
 "currency"?: "EUR", "seats": {"used", "purchased", "max": null|int},
 "billingUrl"}
```

`GET /api/billing/usage?meter=&period=current|previous&by=member|service|tool`
(Bearer). Fixture: `billing-usage.json`, `billing-usage-by-service.json`.

```json
{"period": {"key", "start", "end"}, "by": null|"member"|"service"|"tool",
 "currency": "EUR",
 "meters": [{"meter", "unit", "used", "included": null|int, "percent": null|number,
             "projected", "projectedCostMicros", "overage": "block|bill|none",
             "breakdown": [{"key", "used"}]}]}
```

Source: billing plan sections 5.1 (entitlement types), 3.1 (meter keys) and 9
(what the commands print). The field layout is this client's reading of those
types; `micros` are millionths of the currency unit.

## Jobs and runs

| Route | Response |
|---|---|
| `POST /api/runtime/run`, or a tool that starts background work | `202` `{"run_id", "job_id", "status": "queued"}`, or the same object as an MCP tool result's `structuredContent`. Fixture: `run-accepted.json` |
| `GET /api/jobs?kind=&status=&cursor=` | `{"jobs": [JOB], "nextCursor": null\|text}`. Fixture: `jobs.json` |
| `GET /api/jobs/{id}` | `{"job": JOB}`. Fixture: `job.json` |
| `POST /api/jobs/{id}/cancel`, `POST /api/jobs/{id}/retry` | `{"job": JOB}`; retry is admin-only and for `failed` or `dead` jobs |
| `GET /api/runtime/runs/{id}` | a run document that gains `job_status` (a top-level field, or under `run`); `POST /api/runtime/runs/{id}/cancel` is the existing cooperative cancel |

```json
JOB = {"id": "job_<ulid>", "kind", "status": "queued|running|retrying|succeeded|failed|cancelled|dead",
       "attempts", "progress": {}, "error": null|text,
       "created_at", "started_at": null|text, "finished_at": null|text}
```

Source: scalability plan sections 5.1, 5.3, 6, 7 and 8. The plan names the
routes, the statuses and the table columns; the `{"job": ...}` wrapper and
`snake_case` fields are this client's decision, following the approvals and
runtime routes. zuano treats an id that starts with `job_` as a job.

## Quota usage

`GET /api/org/usage?from=YYYY-MM-DD&to=YYYY-MM-DD` (Bearer). Fixture:
`org-usage.json`.

```json
{"orgId", "plan", "from", "to",
 "metrics": [{"metric", "window": "day|month|gauge", "used", "soft", "hard", "resetsAt": null|text}],
 "daily": [{"day", "metric", "quantity"}]}
```

Source: scalability plan sections 5.9 and 7 ("per-metric daily usage, current
window counters, limits"). The layout is this client's decision.
