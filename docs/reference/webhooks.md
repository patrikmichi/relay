# Webhooks

Inbound endpoints receive events from other services and verify their
signatures. Triggers react to those events. Subscriptions send the gateway's
own events (approvals, runs, files, ...) to your HTTPS URL, signed with the
Standard Webhooks headers (`webhook-id`, `webhook-timestamp`,
`webhook-signature`).

| Command | What it does |
|---|---|
| `zuano webhooks list [--json]` | Endpoints, triggers and subscriptions |
| `zuano webhooks create endpoint --service SLUG [--description]` | Endpoint for a connected service's webhook |
| `zuano webhooks create endpoint --generic --header H [--algorithm sha256] [--encoding hex] [--timestamp-header H]` | Endpoint for a custom source that signs with an HMAC |
| `zuano webhooks create subscription --url https://... --events approval.*,run.finished [--description]` | Outbound subscription |
| `zuano webhooks create trigger --from-file trigger.json` | Trigger from a JSON definition (`-` reads standard input); the gateway validates it |
| `zuano webhooks test <whe_\|whs_>` | Test event to a subscription, synthetic event to an endpoint |
| `zuano webhooks deliveries <whs_> [--status dead]` | Delivery attempts of a subscription |
| `zuano webhooks redeliver <whd_>` | Send a delivery again (same `webhook-id`) |
| `zuano webhooks rotate-secret <whe_\|whs_>` | New secret; the old one keeps working for a grace period |
| `zuano webhooks events <whe_>` | Events an endpoint received |

All accept `--json` (the response body unchanged) and `--gateway-url`.

## Secrets

A secret is shown once, when an endpoint or subscription is created or its
secret is rotated. zuano prints it only to a terminal or with `--json`. When
standard output is a pipe or a file and `--json` is absent, the command fails
*before* sending anything, so no secret is created that nobody sees:

```
$ zuano webhooks create endpoint --service github > out.txt
error: this command prints a secret that is shown only once, and standard output is not a terminal. ...
```

## Approvals

Creating, rotating, testing and redelivering are writes. In an agent session
(`ZUANO_AGENT=1`) the gateway may hold them for approval; zuano then prints the
held call like `zuano call` does and exits 12.

The routes these commands call are listed in
[gateway-contracts.md](gateway-contracts.md#webhooks).
