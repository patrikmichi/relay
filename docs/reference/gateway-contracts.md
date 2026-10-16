# Gateway contracts relay assumes

relay is built against these gateway routes and response shapes. The gateway
does not serve all of them yet; each section says where its shape comes from
(a spec, or a decision of this client where the spec leaves the shape open).
The gateway's contract tests should produce the documents in
`internal/cli/testdata/contracts/`, which relay's own tests read.

Conventions: a field name in `snake_case` follows the approvals and runtime
routes, `camelCase` follows the billing, evals and entitlement types. A field
relay does not read may be added at any time; a field relay reads must not
change its meaning.

## Refusals every command understands

Any gateway response, on any route, may be one of these. relay maps them in
its HTTP client, so no route needs to handle them separately.

| Response | Client behaviour | Exit |
|---|---|---|
| `402` `{"error": CODE, "message", "feature"?, "limit"?, "meter"?, "current"?, "max"?, "current_plan", "required_plan", "upgrade_url", "resets_at"?}` with CODE `plan_required`, `limit_reached` or `verification_required` | prints the message, plans and `upgrade_url` | 30 |
| `402` with CODE `quota_exceeded` or `spend_cap_reached` | same | 31 |
| `503` `{"error": "billing_unavailable", "message"}` with `Retry-After` | prints the message and the wait | 32 |
| `403` `{"error": "org_suspended" \| "seat_suspended", "message"}` | prints the message | 33 |
| `429` `{"error": "quota_exceeded", "message", "metric", "limit", "resets_at"}` | prints metric, limit and reset | 13 |
| `401` `{"error": "step_up_required", "step_up_id", "step_up_url"}` | completes the step-up, see below | 14 when nobody can |
| `401` `{"error": "reauth_required"}` | treated like an expired token: `relay login` | 1 |
| `200`/`403` body or MCP `structuredContent` `{"reason": "policy_off", "message"}` | prints the message | 10 |

Source: billing plan section 5.8 (codes, statuses, JSON, exit 30 to 33),
scalability plan sections 5.9 and 8 (`429` quota, exit 13), risk policy plan
sections 5.2 and 5.14 (`policy_off`, exit 10; `step_up_required`, exit 14).
The billing and scalability plans disagree on the status of `quota_exceeded`
(`402` and exit 31 against `429` and exit 13); relay accepts both and keeps
the exit statuses apart. `limit` is a limit key (text) in a plan refusal and a
number in a quota refusal; relay reads either.

## Login

| Request | Carries |
|---|---|
| `GET /api/cli/authorize` | `principal=cli_personal` or `cli_agent`, with the existing PKCE parameters; no `agent` parameter |
| `POST /api/cli/device/code` | `{"principal": "cli_personal" \| "cli_agent"}`; no `agent` field |
| `POST /api/cli/token`, `POST /api/cli/device/token`, `POST /api/cli/refresh` | no principal: it is fixed at issue |

Source: risk policy plan 5.1 and 5.14. A credential without a principal is
answered `401 {"error": "reauth_required"}`; relay does not refresh such a
session and tells the person to run `relay login`.

## Approvals

| Route | Use |
|---|---|
| `GET /api/approvals?status=S` | unchanged: `{"approvals": [...]}` |
| `GET /api/approvals/{id}` | unchanged: `{"approval": {...}, "args_preview": ...}`; Bearer allowed |
| `POST /api/approvals/{id}/await` | unchanged |
| page `GET /approvals/{id}?decision=approve\|deny` | where a person decides with a passkey; relay opens it and never posts a decision |

relay calls no `approve`, `deny`, `challenge` or `decide` route. The gateway
must reject a Bearer decision (risk policy plan 5.3).

## Step-up

| Route | Use |
|---|---|
| `GET /api/stepup/{id}` | `{"status": "pending" \| "approved" \| "denied" \| "expired"}`; Bearer allowed |
| page `GET {step_up_url}` | on the gateway's origin; the person confirms with a passkey |
| request header `X-Step-Up: {id}` | on the retried request; single use, bound to the request's digest |

Source: risk policy plan 5.4 and 5.14. The status document is this client's
decision; the plan names the route and the three states.

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
runtime routes. relay treats an id that starts with `job_` as a job.

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
