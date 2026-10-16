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
| `429` `{"error": "quota_exceeded", "message", "metric", "limit", "resets_at"}` | prints metric, limit and reset | 31 |
| `401` `{"error": "step_up_required", "step_up_id", "step_up_url"}` | completes the step-up, see below | 14 when nobody can |
| `401` `{"error": "reauth_required"}` | treated like an expired token: `zuano login` | 1 |
| `200`/`403` body or MCP `structuredContent` `{"reason": "policy_off", "message"}` | prints the message | 10 |

Source: billing plan section 5.8 (codes, statuses, JSON, exit 30 to 33),
scalability plan sections 5.9 and 8 (`429` quota, exit 31), risk policy plan
sections 5.2 and 5.14 (`policy_off`, exit 10; `step_up_required`, exit 14).
A plan quota is `402` `quota_exceeded` (exit 31); a rate limit is `429`
`rate_limited` (exit 13, never a quota). A metered `429` `quota_exceeded` is
also accepted and exits 31. `limit` is a limit key (text) in a plan refusal and a number in a
quota refusal; zuano reads either.

Every response may carry `X-Request-Id: req_<ulid>`. zuano sends its own
`X-Request-Id: cli_<ulid>` on every request as client correlation, and adds the
gateway's id to each refusal and failed-call message as `request id: req_...`.
`--debug` prints the id of every response.

## CLI exit codes

One table for every status a command can end with. A test checks it against
the constants in the code, both ways.

| Code | Name | Notes |
|---|---|---|
| 0 | `ok` | |
| 1 | `error` | unclassified failure; also `spec_fetch_blocked` and `federated_mode_unsupported`, with the code in the JSON |
| 2 | `usage` | bad flags or unreadable input; `policy plan --detailed-exitcode` also returns 2 for "changes present" |
| 4 | `signature_invalid` | any signature or content-hash verification failure; never a timeout |
| 10 | `denied` | `policy_off`, DLP block, `meta_executor_blocked`, `source_not_preferred`, `agent_not_allowed`, `denied_by_policy`, `tool_not_pinned` |
| 11 | `approval_expired` | |
| 12 | `pending_approval` | held call |
| 13 | `rate_limited` | `429` only; never a quota |
| 14 | `step_up_required` | non-interactive session |
| 15 | `plan_stale` | the organisation changed since the plan was made |
| 16 | `invalid_manifest` | `422` `invalid_manifest` |
| 17 | `wait_timeout` | a `--wait` or request time limit passed |
| 20 | `run_failed` | an eval, export or automation run ended failed |
| 21 | `eval_regression` | |
| 22 | `session_too_large` | `413` on capture |
| 23 | `eval_inconclusive` | |
| 24 | `eval_threshold_not_met` | |
| 30 | `plan_required` | also `limit_reached`, `verification_required` |
| 31 | `quota_exceeded` | also `spend_cap_reached`, daily caps |
| 32 | `billing_unavailable` | |
| 33 | `suspended` | `org_suspended`, `seat_suspended`, `ip_not_allowed`, identity suspended |
| 34 | `network_offline` | |
| 40 | `policy_invalid` | also DLP policy documents |
| 41 | `policy_conflict` | |
| 42 | `policy_managed_in_git` | |
| 43 | `tool_definition_changed` | |
| 44 | `upstream_quarantined` | also `registry_server_deleted` |
| 45 | `evidence_bundle_invalid` | `compliance verify` |
| 46 | `evidence_keys_missing` | `compliance verify` |
| 50 | `a2a_chain_refused` | `a2a_loop`, `a2a_depth_exceeded`, `a2a_chain_limit` |
| 51 | `chain_killed` | |
| 52 | `chain_budget_exhausted` | |
| 53 | `agent_card_changed` | |
| 60 | `spec_invalid` | |
| 61 | `spec_unsupported` | |
| 62 | `connector_conflict` | `connector_slug_taken`, `connector_version_conflict` |
| 63 | `quality_gate_failed` | |

SDK builder and catalog refusal codes map onto these by class: `400`, `409` and
`422` end with 1 and keep the code in the JSON, `403` with 10, `402` with 30
or 31.

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

## File store

Auth is the usual bearer; the organisation comes from the credential.
Fixtures: `file-create.json`, `file-meta.json`, `files-list.json`,
`mcp-file-result.json`.

| Route | Request and response |
|---|---|
| `POST /api/files` | `{"name", "content_type", "size", "sha256", "share_with_org"}` answers `{"file_id": "fil_...", "upload": {"url", "method": "PUT", "headers": {}, "expires_at"}}`; over a limit: `402 quota_exceeded` or `413` |
| `PUT {upload.url}` | the object store, not the gateway: zuano sends the bytes, the listed headers and no credential |
| `POST /api/files/{id}/complete` | `{}` answers `{"file": FILE}` |
| `GET /api/files/{id}` | `{"file": FILE}`; `404` for a file the caller may not read |
| `GET /api/files?state=&created_via=&cursor=` | `{"files": [FILE], "next_cursor": null\|text}` |
| `GET /api/files/{id}/download` | `302` to a signed address (zuano does not send its credential there), or `200` with the bytes |
| `DELETE /api/files/{id}` | `200` or `204` |

```json
FILE = {"id": "fil_...", "name", "content_type", "size_bytes", "sha256": null|hex,
        "state": "pending|uploaded|scanning|ready|quarantined|failed|deleted",
        "created_via", "shared_with_org", "expires_at", "created_at",
        "scan": {"verdict": "clean|warn|blocked"}}
```

A tool takes a file argument as the string `file:fil_...`. A tool result may
carry `{"type": "resource_link", "uri", "name", "mimeType", "size"}` content
blocks and `structuredContent.files[]` of `{"file_id", "name", "mime_type",
"size", "sha256", "download_url", "expires_at"}`; zuano downloads by `file_id`
through the route above and ignores `download_url`.

Source: file store plan, sections "Lifecycle", "Surfaces" and "relay CLI
changes". The plan names the routes, the upload target and the result blocks;
the `{"file": ...}` and `{"files": ...}` wrappers and the `snake_case` fields
of FILE are this client's decisions, following the table columns.

## Webhooks

All under `/api/v1/webhooks/` (triggers, webhooks and event streams, including `GET /api/v1/triggers/stream`, live under `/api/v1`), bearer auth, organisation from the credential. A
write by an agent session may answer `202` with the held-call document (exit
12). Fixtures: `webhooks-endpoints.json`, `webhooks-endpoint-create.json`,
`webhooks-triggers.json`, `webhooks-subscriptions.json`,
`webhooks-deliveries.json`, `webhooks-events.json`.

| Route | Request and response |
|---|---|
| `GET endpoints`, `GET triggers`, `GET subscriptions` | `{"endpoints": [ROW]}`, `{"triggers": [ROW]}`, `{"subscriptions": [ROW]}` |
| `POST endpoints` | `{"source_kind": "connector", "service", "description"}` or `{"source_kind": "generic", "signature": {"scheme": "hmac", "algorithm", "header", "encoding", "timestampHeader"?, "timestampFormat"?}, "description"}` answers `{"endpoint": ROW with "url", "secret": "<signing secret>"}` |
| `POST subscriptions` | `{"url", "event_types": [text], "description"}` answers `{"subscription": ROW, "secret"}` |
| `POST triggers` | the definition file as given (source, filter, action, run-as) answers `{"trigger": ROW}` |
| `POST endpoints/{id}/test`, `POST subscriptions/{id}/test` | `{}`; any `2xx` JSON |
| `POST endpoints/{id}/rotate-secret`, `POST subscriptions/{id}/rotate-secret` | `{}` answers `{"secret"}` |
| `GET subscriptions/{id}/deliveries?status=` | `{"deliveries": [DELIVERY], "next_cursor"}` |
| `POST deliveries/{id}/redeliver` | `{}`; any `2xx` JSON |
| `GET endpoints/{id}/events` | `{"events": [EVENT], "next_cursor"}` |

```json
ROW      = {"id": "whe_|wtr_|whs_...", "status"?, "enabled"?, "service"?, "source_kind"?, "url"?,
            "event_types"?: [text], "name"?, "description"?, "action"?: {"kind"}}
DELIVERY = {"id": "whd_...", "subscription_id", "event_id", "event_type", "attempt",
            "status": "pending|succeeded|failed|dead", "response_status": null|int,
            "response_ms", "response_excerpt", "last_error", "next_attempt_at", "created_at"}
EVENT    = {"id": "wev_...", "endpoint_id", "delivery_id", "event_type", "received_at",
            "verify": "ok|fail", "status": "received|processed|partially_failed|failed|dropped_quota"}
```

Source: file store and webhooks plan, "API" and "relay CLI changes (E)". The
plan names the routes and ids; the wrapper keys, the `snake_case` row fields
and the request bodies of the two create routes are this client's decisions.

## Artifact pages

Under `/api/artifacts/`, bearer auth, organisation from the credential. A write
by an agent session may answer `202` with the held-call document (exit 12).
Fixtures: `artifact-upload-create.json`, `artifact-release-ready.json`,
`artifact-release-entry-required.json`, `artifact-sites.json`,
`artifact-site-detail.json`, `artifact-releases.json`,
`artifact-share-proposal.json`. Path rules: `internal/drop/testdata/paths.json`,
to be replaced by the gateway's own vectors file.

| Route | Request and response |
|---|---|
| `POST uploads` | `{"site": {"slug", "title"?, "routing"?: "spa"}, "files": [{"path", "size", "sha256"}], "entry"?, "idempotency_key": "cli_..."}` answers `{"upload_id": "aup_...", "files": [SLOT]}` |
| `PUT {slot.upload.url}` | the object store: the bytes, the listed headers, no credential |
| `POST uploads/{id}/finalize` | `{"activate": bool}` answers `{"release_id", "status": "preparing"}` |
| `GET releases/{id}` | `{"release": RELEASE, "site": SITE}`; `state` is `preparing`, `ready`, `failed` or `deleted` |
| `GET sites[?status=deleted]` | `{"sites": [SITE]}` |
| `GET sites/{id}` | `{"site": SITE, "grants": [GRANT]}` |
| `GET sites/{id}/releases` | `{"releases": [RELEASE]}` |
| `POST sites/{id}/activate` | `{"release_id", "expected_content_revision"}`; `412 revision_conflict`, `409 audience_changed` |
| `PUT sites/{id}/sharing?mode=propose` | `{"grants": [GRANT], "public": {"enabled", "expires_at"?}, "source_download", "expected_policy_revision"}` answers `{"diff": {"gain": [GRANT], "lose": [GRANT], "public": {"before", "after"}}, "policy_revision"}` |
| `PUT sites/{id}/sharing?mode=apply` | the same body with the revision from the proposal; `412` when the sharing changed meanwhile |
| `DELETE sites/{id}`, `POST sites/{id}/restore` | `200` or `204` |

```json
SLOT    = {"path", "file_key", "status": "present"}  or  {"path", "file_key", "upload": {"url", "method": "PUT", "headers": {}, "expires_at"}}
RELEASE = {"id": "arl_...", "site_id", "state", "entry_path", "total_bytes", "file_count", "created_at", "ready_at",
           "preview_url", "warnings": [{"kind", "path", "line"?, "message"}],
           "failure": null | {"code", "message", "candidates": [text], "files": [{"path", "error"}]}}
SITE    = {"id": "ast_...", "slug", "title", "url", "routing", "status", "audience", "active_release_id": null|text,
           "active_release_at", "total_bytes", "content_revision", "policy_revision", "public_read",
           "public_expires_at": null|text, "source_download"}
GRANT   = {"principal_type": "user|group|org", "principal_id", "principal_label", "role": "viewer|editor|owner", "expires_at": null|text}
```

A new user grant in a sharing request names the person by `principal_email`
instead of `principal_id`. Source: artifact pages plan, "Publish flow",
"Sharing" and "relay CLI changes". The plan names the routes, the upload and
sharing bodies and the typed errors; the response wrappers, the `SLOT` shape
(the plan says `{file_key, ...}` per file; zuano also reads `path` to match it
to a file), the proposal diff and the `GRANT`/`SITE` fields are this client's
decisions.

## Plugin feeds

Documents on `/m/{feed}/...` need `Authorization: Bearer <feed token>`; the
other routes take the usual session bearer. Fixtures: none yet (the plugin tests
build signed packages).

| Route | Request and response |
|---|---|
| `GET /api/org/plugin-feeds` | `{"feed": {"slug", "base_url": "https://gateway/m/{slug}", "npm_scope", "git_mirror": bool, "channels": ["stable", "beta"]}}`; `base_url` must be on the gateway's own origin under `/m/` |
| `POST /api/cli/feed-token` | `{}` answers `{"token", "expires_in"}` (`feeds:read`, an hour); `{"kind": "api_key", "name"}` answers `{"token"}`, a long-lived key shown under Settings, API keys. `402 plan_required` when the plan lacks private feeds |
| `GET /m/{feed}/claude/{channel}/marketplace.json`, `GET /m/{feed}/agent-plugins/{channel}/marketplace.json` | `{"name", "owner": {"name", "email"?}, "plugins": [{"name", "version", "description"?, "displayName"?, "source": {"source": "url", "url", "sha256"}}]}`; `source.url` is on the gateway's origin |
| `GET {source.url}` | the package: `.zip`, or `.tgz` for npm (members under `package/`), carrying `SIGNATURE.json` |
| `GET /api/plugins/{name}/packages?version=` | `{"packages": [{"platform", "state", "sha256", "url"}]}` for the platforms the marketplaces do not list (`gemini`, `opencode_npm`, `cline`) |
| `GET /api/plugins/{name}/requirements` | `{"missing": [connector slug]}`; warnings only |
| `POST /api/plugins/installs` | `{"plugin", "version", "agent", "method", "cli_version"}` answers `{"install": {"id"}}` |
| `DELETE /api/plugins/installs/{id}` | `200` or `204` |
| `GET /m/{feed}/git/{channel}.git`, `GET /m/{feed}/git/{plugin}.git` | read-only smart HTTP; git authenticates with `username=zuano`, `password=<feed token>` |
| `GET /m/{feed}/npm/` | the registry for the feed's npm scope |

`SIGNATURE.json` is `{"schema": "plugin-signature/1", "plugin", "version",
"platform", "files": {path: sha256 hex}, "kid", "alg": "ed25519",
"signature": base64}`; the signature covers the canonical JSON of the document
without `signature`, with the keys at `/.well-known/zuano-signing-keys`.

Source: plugin bundles plan, "Builders", "Feeds", "Feed tokens" and "relay CLI
changes". The plan names the feed routes, the signature document and the token
endpoint. The `api/org/plugin-feeds` document, the long-lived key request, the
package list route (the plan's package list is the dashboard's), the
requirements route taking a name, the git path per plugin for Gemini, and the
install report are this client's decisions.

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

## Signed documents

A document zuano acts on without a person reading it first carries an ed25519
signature, checked against the keys pinned for the gateway (the same keys as
signed downloads). The headers are `X-Content-Signature` (base64) and
`X-Content-Signature-Key-Id`; the signature covers the canonical JSON of the
response body (keys sorted at every depth, no whitespace). A body that is
unsigned, signed by an unpinned key or altered ends the command with exit 4
and nothing is used. Fixtures: none yet (the tests sign with a generated key).

## Starter packs

Admin routes need an owner or admin session and refuse the agent profile; a
refusal by the plan is a `402` as described above. Responses marked *signed*
carry the signature headers.

| Route | Request and response |
|---|---|
| `GET /api/packs?category=&source=installed` | `{"packs": [{"slug", "displayName", "version", "category", "source": "platform\|org", "summary", "installed", "fit"}]}` |
| `GET /api/packs/{slug}?version=` | `{"pack": {"slug", "displayName", "version", "summary", "category", "versions": [text], "connectors": [{"slug", "required", "credentialScope", "why"}], "skills": [name], "bundles": [name], "rules": [name], "readme"}}` |
| `POST /api/packs/{slug}/plans` | `{"version"?, "params": {}, "mode": "install\|update"}` answers a *signed* PLAN |
| `POST /api/packs/installs/{id}/plans` | `{"mode": "uninstall\|rollback", "toVersion"?, "disableConnectors"?}` answers a *signed* PLAN |
| `POST /api/packs/plans/{planId}/apply` | `{"planHash", "decisions": {"<seq>": "keep\|take"}}` answers `202` `{"installId", "jobId"}`; `409` `{"error": "plan_stale"}` when the organisation changed since the plan; `401 step_up_required` when the plan loosens a control |
| `GET /api/packs/installs`, `GET /api/packs/installs/{id}` | `{"installs": [INSTALL]}`, `{"install": INSTALL}` |
| `POST /api/packs/validate` | `{"manifest"}` answers `{"errors": [{"path", "message"}]}`, empty when every reference resolves |
| `POST /api/packs`, `POST /api/packs/{slug}/publish` | `{"manifest"}` answers `{"pack": {"slug"}}`; `{"version", "channel"}` publishes it |
| `GET /api/cli/sync/assignments` | the member's ASSIGNMENTS, *signed*, with `ETag: "<revision>"`; `If-None-Match` answers `304` |

```json
PLAN = {"planId", "planHash", "mode", "pack": {"slug", "version", "name"}, "params": {},
        "ops": [{"seq", "kind", "target": {}, "before", "after",
                 "status": "apply|noop|conflict|skipped_plan|skipped_platform|skipped_missing",
                 "loosens", "requires"?: {"feature"?, "limit"?, "requiredPlan"?}, "reason"?}],
        "credentialPrompts": [{"slug", "name", "scope": "org|personal", "status": "connected|missing|expired",
                               "neededBy": [text], "cliCommand", "connectUrl"}],
        "summary": {"apply", "noop", "conflict", "skipped", "loosening"}, "expiresAt"}
INSTALL = {"id", "status", "channel", "installedAt", "updateAvailable"?, "pack": {"slug", "version"},
           "ops": [{"applySeq", "seq", "kind", "target", "status", "reason"?}]}
ASSIGNMENTS = {"revision", "profiles": [{"id", "source": "pack", "name", "pack": {"slug", "version"},
               "rules": [{"name", "description"?, "alwaysApply", "globs"?, "agents": ["all"|agent], "body", "sha256"}],
               "skills": [{"resourceId", "versionId", "name", "version"}],
               "bundles": [{"name", "version", "channel"}]}],
               "connectRequired": [{"slug", "scope", "reason"}]}
```

Operation kinds: `connector.enable`, `connector.prompt`, `group.create`,
`group.map`, `resource.enable`, `resource.assign`, `risk.cell`,
`risk.override`, `approvals.approver_group`, `approvals.grant_max`,
`automation.create`, `sync_profile.upsert`. A plan is applied only with the
`planHash` of the plan the person was shown.

Source: starter packs plan (manifest, planner, apply, member delivery and CLI
sections). The plan names the routes, the plan and assignments documents and
the exit statuses; the `{"pack": ...}`, `{"installs": ...}` wrappers, the
`validate` route, the `version` field of a delivered skill (the CLI installs
by version) and the signature headers are this client's decisions. The plan
asks for exit 20 on `plan_stale`; 20 is taken by a failed eval, so a stale
plan exits 15 with the message to run `zuano pack plan` again.

## Skills from sessions

Routes under `/api/skills/from-session` (Bearer, personal session).

| Route | Request and response |
|---|---|
| `GET /consent` | `{"statementVersion", "text"}`: the words a person accepts before an upload |
| `POST /captures` | one of `{"source": "agent_run", "runId"}`, `{"source": "mcp_session", "sessionRef", "goal"}`, or `{"source": FORMAT, "session": SESSION, "clientRedacted": true, "turnRange": {"from", "to"}}`; each with `"consent": {"statementVersion"}` and optional `"redactTerms"`. Answers `201` `{"captureId", "turns", "toolCalls", "expiresAt", "redaction": {"counts", "placeholders"}}`; `413` too large (exit 22); `422` unparseable with a message (exit 2) |
| `POST /captures/{id}/extract` | `{"mode": "new_skill\|eval_cases", "targetResourceId"?, "setId"?}` answers `{"extractionId", "jobId"}` |
| `GET /extractions/{id}` | `{"extraction": {"id", "status": "queued\|running\|ready\|failed\|superseded", "error"?, "injectionFlag", "proposal": {"name", "description", "instructions", "connectors": [slug], "cases": [], "warnings": []}}}` |
| `POST /extractions/{id}/accept` | `{"proposal", "mode", "targetResourceId"?, "setId"?, "then"?: "evals\|evals_optimize"}` answers `{"resourceId", "versionId", "setId", "catalogUrl"}` |
| `GET /captures`, `GET /captures/{id}`, `DELETE /captures/{id}` | `{"captures": [{"id", "source", "status", "createdAt", "expiresAt", "skill"?: {"id", "name"}}]}`; `{"capture": {...row, "turns", "toolCalls", "redaction": {"counts"}}}` |

`SESSION` is `zuano.session/v1`: `{"format", "source", "agent"?, "title"?,
"startedAt"?, "endedAt"?, "cwdHint"?, "turns": [{"i", "role": "user\|assistant\|tool",
"text", "toolCalls"?: [{"id"?, "name", "service"?, "tool"?, "kind":
"gateway\|mcp_other\|builtin", "argsPreview", "resultPreview"?, "outcome"?}],
"sidechain"?}], "stats", "redaction": {"rulesVersion", "clientRedacted",
"counts", "placeholders"}}`. The CLI sends it already redacted; the gateway
redacts again and its report is the one shown. A turn is at most 8,000
characters, arguments 1,000 and results 1,500, and a session at most 400 turns.

The redaction rules are the file `internal/redact/redaction-rules.json`
(`zuano.redaction/v1`). The gateway's copy must be byte-identical; with
`ZUANO_CONTRACT_FIXTURES` set to the gateway's fixtures, a test fails when
`redaction-rules.json` there differs.

Source: session-to-skill plan (normalized session, privacy, surfaces and CLI
sections). The plan names the capture, extract, accept and list routes and the
session shape; the `consent` route, the response wrappers, `turnRange` and the
extraction document are this client's decisions. The shape of each agent's
session file is read from the formats as currently documented and may need
adjusting against real files from newer agent versions.

## Data loss prevention

| Route | Request and response |
|---|---|
| `GET /api/admin/dlp/policy[?format=yaml]` | `{"policy": {"version", "spec": {"mode", "rules": []}}}`, or the YAML document as it is |
| `PUT /api/admin/dlp/policy[?dryRun=true]` | the document as `application/yaml` or `application/json`; answers `{"version"}`, or with `dryRun` `{"diff": {"changes": [CHANGE]}, "impact": {"days", "wouldBlock", "wouldHold", "wouldMask", "wouldTokenize", "byRule": [{"rule", "count"}]}}`; `422` `{"error": "policy_invalid", "errors": [DIAG]}` (exit 40); `401 step_up_required` |
| `GET /api/admin/dlp/policy/versions` | `{"versions": [{"version", "createdAt", "actor", "mode", "source"}]}` |
| `POST /api/admin/dlp/test` | `{"text" \| "json", "service"?, "tool"?, "direction": "inbound\|outbound"}` answers `{"findings": [{"class", "detector", "confidence", "count", "masked"}], "decision": {"action", "rule"}}`; nothing is logged |
| `GET /api/admin/dlp/findings?from=&class=&service=` | `{"findings": [{"id", "at", "class", "detector", "service", "tool", "outcome", "enforced", "count"}]}`; never a value |
| `GET /api/admin/dlp/stats?from=&groupBy=` | `{"stats": [{"key", "count"}]}` |
| `POST /api/dlp/findings/{id}/feedback` | `{"verdict": "false_positive\|true_positive", "scope": "value\|tool\|path"}` |
| `POST /api/admin/dlp/hash` | `{"detector", "value"}` answers `{"key": "vk1_..."}`; the value is not stored |
| `GET /api/admin/connectors/data-profiles`, `PUT .../{slug}/data-profile` | `{"profiles": [{"service", "category", "residency", "basis"}]}`; `{"residency", "basis"}` |

Source: DLP and compliance packs plan (Part 1 APIs and CLI). The plan names the
routes and commands; the wrappers, `dryRun`, the impact fields and the
`direction` spellings are this client's decisions.

## Compliance packs

| Route | Request and response |
|---|---|
| `GET /api/compliance/packs?from=&to=` | `{"packs": [{"id", "name", "version", "coverage": {"covered", "partial", "gaps", "total"}}]}` |
| `GET /api/compliance/packs/{pack}/controls?from=&to=` | `{"controls": [{"id", "title", "status": "covered\|partial\|gap\|manual", "sources", "records", "gapNote"?}]}` |
| `POST /api/compliance/exports` | `{"pack", "from", "to"}` answers `202` `{"export_id", "job_id"}` |
| `GET /api/compliance/exports`, `.../{id}`, `DELETE .../{id}` | `{"exports": [...]}`, `{"export": {...}}` |
| `GET /api/compliance/exports/{id}/download` | `{"url", "fileName", "sha256"?}`: a short-lived address, https or on the gateway |
| `GET\|POST /api/compliance/schedules`, `DELETE .../{id}` | `{"schedules": [...]}`; `{"pack", "frequency": "weekly\|monthly\|quarterly", "rangeDays", "recipients"}` answers `{"schedule": {"id"}}` |
| `POST /api/compliance/exports/{id}/shares` | `{"to", "expiresInDays"}` answers `{"share": {"url", "expiresAt"}}` |
| `GET\|POST /api/compliance/incidents`, `GET .../{id}` | `{"incidents": [...]}`, `{"incident": {...}}`; create `{"title", "severity", "detectedAt", "description"}` answers `{"incident": {"id"}}` |
| `GET\|POST /api/compliance/dsr`, `POST .../{id}/execute` | `{"requests": [...]}`; create `{"kind", "subject"}` answers `{"request": {"id"}}` |
| `POST /api/compliance/access-reviews`, `GET .../{id}`, `POST .../{id}/apply` | `{"scope", "dueDays"}` answers `{"review": {"id"}}`; `{"review": {...}}` |
| `GET /.well-known/zuano-evidence-keys`, `GET /.well-known/zuano-audit-keys` | `{"keys": [{"kid", "alg": "ed25519", "publicKey"}]}` |

A bundle is a zip holding `manifest.json`, `manifest.sig` (base64 ed25519 over
the bytes of `manifest.json`), the files the manifest lists and
`chain/checkpoints.jsonl`. The manifest has `{"pack", "packVersion",
"mappingVersion", "orgId", "orgSlug", "range": {"from", "to"}, "generatedAt",
"requestedBy", "evidenceKeyId", "files": [{"path", "sha256", "size"}]}`.
`chain/audit_chain*.jsonl` (the plan puts it next to the other evidence
files; the label check forbids that path, so zuano reads it from `chain/`)
holds one event per line with `seq`, `prev_hash`
and `hash` (hex) next to the event's own fields; `hash` is
`sha256(prev_hash bytes || canonical JSON of the line without hash and prev_hash)`.
A checkpoint line is `{"org_id", "date", "last_seq", "head_hash", "kid",
"signature"}` with the signature (base64 ed25519) over the canonical JSON of
`org_id`, `date`, `last_seq` and `head_hash`. `chain/verification.json`
(`{"ok", "to_seq", "recomputed_head"}`) is the gateway's own result and is
checked against the recomputed one.

Source: DLP and compliance packs plan (export bundle, API and CLI sections). The
plan names the routes, the bundle layout and exit codes 45 and 46 for `verify`;
the exact manifest and checkpoint fields, the event hash input and the response
wrappers are this client's reading and must match what the gateway writes.

## Agent inventory and access reviews

| Route | Request and response |
|---|---|
| `POST /api/agents/devices/report` | the device report, personal session only: `{"installId", "cliVersion", "os", "arch", "hostLabel", "agents": [{"id", "version"?, "gatewayMcp", "mcpServers": [{"name", "transport": "http\|stdio", "host"?, "command"?, "firewall"}]}]}`; never arguments, environment values or headers |
| `GET /api/agents?kind=&status=&owner=&label=k=v&stale=true&exposure=&can=&risk=&mode=` | `{"agents": [{"id", "name", "kind", "owner", "status", "lastActivity", "exposure", "labels", "reviewDue"}]}` |
| `GET /api/agents/{id}` | `{"agent": {...row, "credentials": [{"kind", "createdAt", "expiresAt"}], "device": {"hostLabel", "os"}, "access": [{"service", "risk", "mode", "source"}], "exposureFactors": [text]}}` |
| `GET /api/agents/{id}/activity?from=&to=` | `{"activity": {"calls": [{"service", "count"}], "approvals", "refusals"}}` |
| `POST /api/agents/{id}/suspend`, `/revoke` | `{"reason"}`; `POST .../resume` `{}`; `401 step_up_required` for resume and revoke |
| `GET /api/agents/devices`, `GET /api/agents/shadow-mcp` | `{"devices": [{"hostLabel", "os", "cliVersion", "owner", "lastReportAt", "agents": [{"id", "gatewayMcp"}]}]}`, `{"servers": [{"name", "transport", "host"?, "command"?, "devices"}]}` |
| `GET /api/access-reviews`, `GET /api/access-reviews/{id}`, `GET .../{id}/export?format=csv\|json` | `{"reviews": [{"id", "name", "status", "dueAt", "progress": {"decided", "total"}}]}`; `{"review", "items": [{"identityId", "name", "reviewer", "decision"}]}`; the export body as it is |
| `GET /api/cli/whoami` | gains `"identityId"` and `"identityStatus"` |
| login: `POST /api/cli/token`, `POST /api/cli/device/code` | the request gains `install_id` (32 hex characters, an identifier and not a credential) |

A request from a suspended, revoked or expired identity answers `403` with
`{"error": "identity_suspended" | "identity_revoked" | "identity_expired",
"message"}`; zuano exits 33. The gateway's MCP entry written by `zuano sync`
carries `X-Zuano-Install: <install id>` for the agents whose config takes
headers.

Source: agent inventory and policy as code plan (identity model, device report,
surfaces). The plan names the routes and commands; the response wrappers, the
`whoami` fields and the `host`/`command` split of the device report are this
client's decisions.

## Policy as code

| Route | Request and response |
|---|---|
| `GET /api/policy/document?format=yaml\|json` | the live policy as a document, as it is |
| `POST /api/policy/validate` | `{"files": [{"path", "content"}]}` answers `200`, or `422` `{"error": "policy_invalid", "errors": [DIAG]}` |
| `POST /api/policy/plan` | `{"files", "base": "live"}` answers `{"plan": PLAN}` or the `422` above |
| `POST /api/policy/apply` | `{"files", "expectedParentId", "planId"}` answers `200` `{"release": {"id"}}`; `202` `{"pending_apply_id", "url"}` when an approval is required; `409` `{"error": "policy_conflict"}` when the parent release changed (exit 41), `{"error": "policy_managed_in_git"}` (exit 42) |
| `GET /api/policy/pending/{id}` | `{"pending": {"status": "pending\|approved\|applied\|denied\|expired", "releaseId"?}}` |
| `GET /api/policy/status` | `{"status": {"management", "repo", "branch", "activeRelease": {"id", "source", "appliedAt", "commit"}, "drift": {"kind", "message"}\|null, "breakGlass": {"actor", "reason", "expiresAt"}\|null}}` |
| `GET /api/policy/releases`, `GET .../{id}`, `GET .../{id}/diff?against={id}\|live` | `{"releases": [...]}`, `{"release": {"id", "source", "appliedAt", "actor", "commit", "classification", "provenance": {"pr", "approvers"}, "signed"}}`, `{"diff": {"changes": [CHANGE]}}` |
| `POST /api/policy/releases/{id}/rollback` | `{}` answers `{"release": {"id"}, "revertPr"?, "hint"?}`; `401 step_up_required` |
| `POST /api/policy/freeze` | `{}`; `401 step_up_required` |
| `POST /api/policy/ci/token` | `{"provider": "github\|gitlab", "idToken"}` answers `{"token", "expiresIn"}`: a workload token that is sent as the bearer of the other policy routes |
| `GET /api/admin/risk-policy/preview` | the decision also carries `"sourceRef": {"path", "file", "line"}` for a policy kept as files |

```json
DIAG   = {"code"?, "message", "file"?, "line"?}
CHANGE = {"section", "op": "added|removed|changed", "path", "file"?, "line"?}
PLAN   = {"planId", "parentId", "classification": "tightening|loosening|mixed|neutral",
          "changes": [CHANGE], "impact": [{"service", "risk", "from", "to", "identities", "loosening"}],
          "loosening": [text], "lifecycle": [text], "packs": [text], "errors": [DIAG], "url"?}
```

The JSON Schema of the document is `internal/policyfile/schema/policy-v1.json`;
the gateway serves the same file at `/schemas/policy/v1.json`, and a test
compares them when `ZUANO_CONTRACT_FIXTURES` holds the gateway's
`policy-v1.json`. In a CI job, zuano exchanges the job's OIDC token (GitHub's
Actions token, or the GitLab variable `ZUANO_ID_TOKEN`) for the workload token.

Source: agent inventory and policy as code plan (file format, plan, apply, CI
and CLI sections). The plan names the routes, the CLI commands and exit codes
40 to 42; the response wrappers, `planId`, `parentId`, the pending status
values and the `ci/token` request fields are this client's decisions. The
embedded schema is this client's reading of the file format and is replaced by
the gateway's when it publishes one.

## Chat approvals

| Route | Request and response |
|---|---|
| `GET /api/me/chat-identities` | `{"identities": [{"id", "provider": "slack\|teams", "workspace", "name", "dmOptOut"}]}` |
| `GET /api/admin/chat/installations` | `{"installations": [{"id", "provider", "workspace", "status", "detail"?}]}`; `403` for a member |
| `GET /api/admin/chat/reachability` | `{"eligible", "reachable"}`: approvers and how many can be reached in chat |
| `GET /api/chat/link/{provider}/start` | a page: sign in to the chat app, then a passkey confirmation |
| `GET /api/admin/approval-routes[/{id}]` | `{"routes": [ROUTE]}`, `{"route": ROUTE}`; readable by a personal CLI session |
| `POST /api/admin/approval-routes/test` | `{"service", "tool", "risk", "source"?, "requester"?}` answers `{"route": {"name", "detail"}, "recipients": [{"kind", "name", "channel", "status": "mapped\|unmapped\|opted_out"}], "channels": [{"installation", "name", "warning"?}]}`; sends nothing |
| `GET /api/approvals/{id}` | the approval object also carries `"notified": [{"channel": "slack\|teams\|email\|inbox", "target"?, "state": "pending\|delivered\|failed", "at"?}]` |

```json
ROUTE = {"id", "name", "priority", "default"?, "match": {"services", "tools", "risks", "sources", "routeHints"},
         "targets": [{"kind": "dm_requester\|dm_group\|channel", "groupId"?, "installationId"?, "conversationId"?, "name"?}],
         "restrictDeciders", "fourEyes", "detail": "preview\|keys\|digest", "reminders": [number]}
```

Source: chat approvals plan (routes, identity mapping, REST and CLI sections).
The plan names the routes and the CLI commands; the `{"routes": ...}` wrappers,
the shape of `notified`, `reachability`, the identities list and the link start
path are this client's decisions.

## Automations

| Route | Request and response |
|---|---|
| `GET /api/automations?owner=me` | `{"automations": [AUTOMATION]}`; `owner=me` limits the list to the caller's, for `doctor` |
| `GET /api/automations/{id}` | `{"automation": AUTOMATION}` |
| `POST /api/automations` | `{"template": slug, "version"?, "name", "params": {}, "bindings": {service: "org\|personal"}, "runAsEmail"?}` answers `201` `{"automation", "wizardUrl": "/automations/{id}/wizard?step=preapprove"}`; a path on the gateway's own origin |
| `GET /api/automations/templates?category=&service=` | `{"templates": [{"slug", "title", "summary", "category", "version", "ready"}]}` |
| `GET /api/automations/templates/{slug}[/versions/{v}]` | `{"template": {...the list entry, "connectors": [slug], "minimumPlan", "permissions": [{"service", "tool", "risk"}], "preapprovableSteps": [path]}}` |
| `POST /api/automations/templates/validate` | `{"manifest"}` answers `{"errors": [{"path", "message"}], "analysis": {"permissions", "minimumPlan", "preapprovableSteps"}}` |
| `POST /api/automations/{id}/run` | `{"mode": "dry_run\|test\|live", "sample"?, "eventId"?, "input"?}` answers `202` `{"run_id", "job_id"}`; an agent session's run may be held like any call (exit 12) |
| `GET /api/automations/{id}/runs` | `{"runs": [{"id", ...}]}` newest first |
| `GET /api/automations/runs/{run}?after=<seq>` | `{"run": RUN}` with only the steps that changed after `seq` |
| `POST /api/automations/{id}/pause`, `/disable` | `200` |
| `GET /api/automations/{id}/preapprovals` | `{"preapprovals": [{"id", "stepPath", "status": "active\|void\|revoked\|expired", "voidReason"?, "expiresAt", "approver", "callsToday", "maxCallsPerDay"}]}` |
| `GET /api/automations/{id}/upgrade?to=` | `{"diff": {"from", "to", "class": "patch\|minor\|major", "permissionsAdded", "permissionsRemoved", "connectorsAdded", "preapprovalsVoided", "stepsChanged"}}`; an empty diff means already current |
| `POST /api/automations/{id}/upgrade`, `DELETE /api/automations/{id}` | `{"to"}`; `200` |

Pages the CLI opens (no API call): `/automations/{id}/enable`,
`/automations/{id}/preapprove?step=`, `/automations/{id}/upgrade?to=`.

```json
AUTOMATION = {"id": "atm_...", "name", "status": "draft|enabled|paused|disabled", "pausedReason"?,
              "autoUpdate"?, "template": {"slug", "version"}, "lastRun"?: {"id", "status", "startedAt"},
              "reapprovalNeeded"?: int}
RUN = {"id": "atr_...", "status": "queued|running|succeeded|failed|cancelled|skipped", "mode", "error"?, "seq",
       "steps": [{"seq", "path", "kind", "status": "running|succeeded|failed|held|denied|expired|skipped",
                  "error"?, "approvalId"?, "approvalUrl"?: path, "preapprovalId"?}]}
```

A run with a step in `held` is waiting for an approval at its `approvalUrl`.
A `failed` run with a `denied` or `expired` step exits 10 or 11.

Source: automation templates plan (REST routes, CLI commands, exit codes). The
plan names the routes, ids and exit codes; the response wrappers, `seq`
cursor, the `template`/`runAsEmail` request fields, `owner=me` and the browser
page paths are this client's decisions.

## Private networks

`zuano networks` uses the administrator routes; mutations by an agent session
are refused locally with exit 10, and a held write exits 12. A network is named
by `name`, `slug` or `id`; the CLI lists networks and picks the id.

| Request | Carries / answers |
|---|---|
| `GET /api/admin/networks` | `{"networks": [{"id", "name", "slug", "status": "online\|degraded\|offline\|disabled", "description"?, "data_class"?, "agents_online", "bindings", "last_seen_at"}]}` |
| `POST /api/admin/networks` | `{"name", "description"?, "data_class"?}`; `201` `{"network": {...}}`; `402 plan_required` without the add-on |
| `GET /api/admin/networks/{id}`, `DELETE /api/admin/networks/{id}` | `{"network": {...}}`; delete is a soft delete |
| `POST /api/admin/networks/{id}/enrollment-tokens` | `{"mode": "single\|multi", "uses", "ttl_seconds", "require_approval"?}`; `201` `{"token": "nxt_...", "expires_at", "snippets": [{"title", "text"}]}`. The token is returned once and is not an MCP tool |
| `GET /api/admin/networks/{id}/agents` | `{"agents": [{"id", "name", "status": "pending\|active\|revoked", "connection", "version", "platform", "last_seen_at", "key_age_days"}]}` |
| `POST /api/admin/networks/{id}/agents/{agent}/approve`, `/rotate`, `/revoke` | `{}`; `200` |
| `GET /api/admin/networks/{id}/allowlist` | `{"drift", "targets": [{"name", "kind", "methods"?, "path_globs"?, "tool_globs"?, "agents", "bound_to"}], "unserved_bindings": [name]}` |
| `GET /api/admin/networks/{id}/logs?agent=&target=&outcome=&since=` | `{"logs": [{"ts", "agent", "target", "method", "status", "outcome", "duration_ms", "audit_ref"}]}` |
| `POST /api/admin/networks/{id}/bindings` | `{"service" \| "mcp_server", "target", "credentials_mode": "agent\|gateway"}`; `201`. Built-in services are refused with `binding_not_supported` |
| `DELETE /api/admin/networks/{id}/bindings?service=` or `?mcp_server=` | `200` |
| `POST /api/admin/networks/{id}/test` | `{"results": [{"agent", "ok", "rtt_ms"?, "error"?}]}`; an empty list means no agent is online (exit 34) |

A call whose body or MCP result carries `reason` `network_offline` or
`egress_unavailable` exits 34.

Source: private networking plan (8.2 routes, 8.3 commands, 6.5 errors). The
response wrappers, the token and binding request fields and the bindings
`DELETE` query are this client's decisions.

## MCP firewall

Bearer auth. The session document is signed (`X-Content-Signature`) and checked
against the pinned keys; a session that does not verify ends the command
before any tool is served.

| Request | Carries / answers |
|---|---|
| `POST /api/fw/local/session` | `{"slug"?, "fingerprint", "packageSpec"?, "installId", "agentId"?}`; signed `{"upstream": {"id", "slug"}, "status": "active\|pending\|unknown\|quarantined", "adminUrl"?, "settings": {"onDefinitionChange": "block\|warn", "offline": "deny\|allow_read_pinned", "unknown": "allow\|deny", "denyTools": [glob]}, "pins": [{"kind": "tool", "name", "hash", "state": "active", "risk": "read\|write\|destructive\|outward", "definition"}]}`. `quarantined` ends the command with exit 44 |
| `POST /api/fw/local/changes` | `{"upstream", "changes": [{"kind": "tool", "name", "change": "added\|modified\|removed", "oldHash"?, "newHash"?, "definition"?}]}`; `200` |
| `POST /api/fw/local/calls` | `{"upstream", "tool", "sessionRef", "arguments"}` (or `"argumentsDigest"` and `"localFindings"` with `--screen local`); answers `{"decision": "allow\|deny\|pending\|redact", "callId"?, "reason"?, "message"?, "approval_id"?, "url"?, "arguments"?}` |
| `POST /api/fw/local/calls/{callId}/result` | `{"error", "content"}` (or `"digest"` and `"localFindings"`); answers `{"decision": "allow\|redact\|withhold", "content"?, "message"?}`. A result the gateway cannot check is withheld |
| `GET /api/firewall/upstreams`, `GET /api/firewall/upstreams/{slug}` | `{"upstreams": [{"slug", "name", "transport", "status", "toolCount", "pendingChanges"}]}`, `{"upstream": {...}}` |
| `POST /api/firewall/upstreams` | `{"slug", "name", "url"}` or `{"slug", "name", "package"}`; `201` |
| `POST /api/firewall/upstream-requests` | as above plus `"reason"`; `201` |
| `GET /api/firewall/changes?status=&upstream=`, `GET /api/firewall/changes/{id}` | `{"changes": [{"id", "upstream", "name", "change", "risk", "status"}]}`, `{"change": {...}}` |

A remote server is reached at `{gateway}/api/fw/{slug}/mcp` with the install id
in the `X-Zuano-Install` header and no token of its own: the gateway holds the
server's credentials. A refusal reason `tool_definition_changed` exits 43,
`upstream_quarantined` and `registry_server_deleted` exit 44, and
`denied_by_policy`, `tool_not_pinned` and the other reasons in the exit-code
table exit 10.

Pages the CLI opens (no API call): `/firewall/changes/{id}` and
`/api/fw/oauth/start?upstream={slug}`.

Source: MCP firewall plan (routes, settings, decisions, exit codes). The
`/api/firewall/*` wrappers, the session request fields and the page paths are
this client's decisions.
