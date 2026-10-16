# Billing, usage and plan limits

zuano reads the organisation's plan and usage from the gateway and reports a
refusal by the plan the same way on every command. Changing the plan, paying
and the customer portal are done in the gateway's billing page: the CLI holds
no billing credential and cannot spend money.

## Commands

| Command | Request |
|---|---|
| `zuano billing status [--json]` | `GET /api/billing/status` |
| `zuano billing usage [--meter KEY] [--period current\|previous] [--by member\|service\|tool] [--json]` | `GET /api/billing/usage?meter=&period=&by=` |
| `zuano billing open [--no-browser]` | none: opens `<gateway>/settings/billing` |
| `zuano usage [--from DAY] [--to DAY] [--json]` | `GET /api/org/usage?from=&to=` |

All of them accept the personal and the agent session (they only read), and
`--json` prints the response body unchanged. The request and response shapes
are in [gateway-contracts.md](gateway-contracts.md).

`status` prints the organisation, the plan with where it comes from (a
subscription, the default plan, assigned by an administrator, or simulated by a
platform administrator), the subscription state and trial end, the billing
period, whether it cancels at the end of the period, seats used, purchased and
allowed, whether the organisation is verified, whether overage is on and the
spend cap. A member without billing rights gets the gateway's reduced view and
zuano says so.

`billing usage` prints one row per meter: used, included (or `unlimited`),
percent of the allowance, the projection to the end of the period and its cost
(only for a meter billed beyond its allowance). `--by` adds rows under each
meter for the member, service or tool that used it. `usage` is the quota view:
each metric with its soft limit (a warning) and hard limit (a block) and when
the window resets.

`billing open` opens the page in the default browser. Without a terminal, in an
agent session or with `--no-browser` it prints the link only.

## Exit statuses

Any gateway response that carries the billing error shape ends the command
with one of these, whatever the command (call, skill eval and optimize,
publish, connect, webhooks, files, plugins). zuano prints the gateway's
sentence, the plan you are on and the plan that has the feature, when the
quota resets, and the upgrade link.

| Status | Gateway error codes | HTTP |
|---|---|---|
| 30 | `plan_required`, `limit_reached`, `verification_required` | 402 |
| 31 | `quota_exceeded`, `spend_cap_reached` | 402 |
| 32 | `billing_unavailable` (the message says when to retry) | 503 |
| 33 | `org_suspended`, `seat_suspended` | 403 |
| 13 | `quota_exceeded` on a metered quota | 429 |

Statuses 13 and 14 (step-up) are described in [approvals.md](approvals.md#exit-statuses)
and [step-up.md](step-up.md). `zuano call --wait` treats 402 as final: it does
not retry.

```
$ zuano call sandbox run_code
run_code needs the Pro plan (plan_required, plan free, needs pro)
Upgrade: https://app.zuano.ai/settings/billing/plans?highlight=sandbox.run_code&org=acme
$ echo $?
30
```

A response with another status or another error code is not treated as a
billing refusal and reaches the command unchanged, so a plain rate limit
(`429` with another error) is still an ordinary failure.
