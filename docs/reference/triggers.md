# Triggers

A trigger type is an event a connector can send, such as `github.issue_opened`.
A trigger is an instance of a type with its config, bound to a destination. The
gateway registers the hook with the provider using the connected account; where
it cannot, it gives the steps to do by hand.

| Command | |
|---|---|
| `zuano triggers types [--service S] [--kind webhook\|poll] [--search Q]` | the catalog, with whether delivery is realtime or polling (and how often) |
| `zuano triggers info <type>` | description, role needed, config fields, event data fields and a sample |
| `zuano triggers create <type> [--config k=v] [--config-file F] [--account ca_...] [--user ID] (--automation atm_... \| --webhook URL \| --agent-prompt TEXT [--agent res_...] \| --deliver) [--description T]` | create a trigger |
| `zuano triggers list [--user IDS] [--account IDS] [--type TYPES] [--auth-config IDS] [--trigger-id IDS] [--show-disabled] [--cursor C]` | list triggers; ids are comma-separated |
| `zuano triggers retry <wtr_id>` | register the hook at the provider again after a failure |
| `zuano triggers delete <wtr_id> [--yes]` | delete a trigger and remove its hook |
| `zuano triggers stream [--trigger ID] [--type T] [--service S] [--account ID] [--user ID] [--out F]` | print events as JSON lines |
| `zuano dev triggers listen --forward URL [--trigger-id ID] [--type T] [--service S] [--account ID] [--user-id ID] [--out F] [--replay F] [--allow-remote]` | send events to a local server, signed like a delivery |
| `zuano generate --services S1,S2 [--lang ts\|py] [--out F] [--no-triggers] [--no-tools] [--force]` | write types for trigger payloads and tool inputs |

All of them take `--gateway-url`; the ones that print data take `--json`.

## Destinations

`--automation` starts a draft automation (a person still turns it on),
`--webhook` posts each event to a URL as a signed delivery, `--agent-prompt`
starts an agent run, and `--deliver` hands events to the project's webhook
subscriptions (the only one a project key may use). Exactly one is required.
A trigger that sends data out (a webhook or an agent prompt) made from an agent
session is held for an approval first (exit 12).

If the provider refuses the registration the trigger stays off and the command
fails; `retry` tries again. When the provider has no API, the printed steps are
the registration: add the webhook by hand.

## The event stream

`stream` follows `GET /api/v1/triggers/stream` with your API key (a browser
session is not enough) and prints each event envelope on a line:

```json
{"id":"evt_...","type":"zuano.trigger.event","version":1,"timestamp":"...","metadata":{"trigger_id":"wtr_...","trigger_type":"github.issue_opened","connected_account_id":"ca_...","user_id":"...","project_id":null,"trace_id":"..."},"data":{...}}
```

If the connection drops, or the gateway ends a response early as it does
before its time limit, the command reconnects with `Last-Event-ID` and the
gateway replays what was missed within 24 hours. `type` also takes `prefix.*`.
Too many streams on one key (3) is a `429` and the command waits and retries.
The stream is unsigned and best effort; it is for building, not for
production. Streaming costs nothing.

## Local development

`dev triggers listen --forward http://localhost:8000/hook` sends every event of
the stream to your server with the Standard Webhooks headers `webhook-id`,
`webhook-timestamp` and `webhook-signature`, so the code that verifies and
parses deliveries runs for real. It signs with `$ZUANO_WEBHOOK_SECRET`
(`whsec_` and base64); without it, it generates a secret and prints it once,
and you give that to your server. `--out` keeps the events in a JSONL file and
`--replay` sends such a file again with the same ids, without the stream. A
server that is down or answers an error is reported for that event and the
listener goes on. Events carry your data, so a URL that is not on this machine
needs `--allow-remote`.

## Generated types

`generate` writes one file for the services you name: `TriggerPayloads` (each
trigger type to its event data) and `ToolInputs` (each tool to its arguments),
as TypeScript `export type`s or Python `TypedDict`s (Python 3.11 or newer). The
language comes from the project: `package.json` or `tsconfig.json` for
TypeScript, `pyproject.toml`, `requirements.txt` or `setup.py` for Python.
Without `--out` the file is `zuano.gen.ts` or `zuano_gen.py`; a file it did not
write is never replaced without `--force`. Tool outputs are not described by
the gateway, so only inputs are typed.
