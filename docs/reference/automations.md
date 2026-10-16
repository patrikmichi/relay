# Automations

An automation is a template (a trigger and steps) installed for the
organisation with its parameters and connections. `zuano automation` installs
templates as drafts, runs and follows them, and publishes your own. Turning an
automation on, and every standing permission that gives it, is decided in the
browser or on your phone with your passkey; zuano only opens the page.

## Commands

| Command | Request |
|---|---|
| `zuano automation list [--gallery] [--category C] [--service S] [--json]` | `GET /api/automations`, or with `--gallery` `GET /api/automations/templates` |
| `zuano automation show <atm_id\|template[@version]> [--json]` | `GET /api/automations/{id}` and `/preapprovals`, or `GET /api/automations/templates/{slug}[/versions/{v}]` |
| `zuano automation install <template>[@version] [--name N] [--param k=v] [--params-file F] [--bind service=org\|personal] [--run-as EMAIL] [--no-browser]` | `POST /api/automations`, then opens the returned page |
| `zuano automation draft [goal] [--manifest F] [--name N] [--param k=v] [--params-file F] [--bind service=org\|personal] [--automation-id atm_...] [--no-browser]` | `POST /api/automations/draft`, then opens the returned page; `422` `invalid_manifest` prints the issues and exits 16 |
| `zuano automation run <atm_id> [--dry-run\|--test --yes] [--sample N\|--event wev_...\|--input F] [--wait[=DUR]]` | `POST /api/automations/{id}/run`, then `GET /api/automations/runs/{run}` |
| `zuano automation logs <atm_id\|atr_id> [--follow] [--step PATH] [--json]` | `GET /api/automations/{id}/runs`, `GET /api/automations/runs/{run}?after=` |
| `zuano automation enable <atm_id>`, `preapprove <atm_id> [--step PATH]` | none: open `/automations/{id}/enable` and `/preapprove` |
| `zuano automation pause\|disable <atm_id>` | `POST /api/automations/{id}/pause\|disable` |
| `zuano automation upgrade <atm_id> [--to V] [--yes]` | `GET` then `POST /api/automations/{id}/upgrade` |
| `zuano automation uninstall <atm_id> [--yes]` | `DELETE /api/automations/{id}` |
| `zuano automation validate <dir>`, `publish <dir> [--channel stable\|beta]` | `POST /api/automations/templates/validate`, `POST /api/marketplace/publish` with `type=automation` |

The shapes are in [gateway-contracts.md](gateway-contracts.md). An id that
starts with `atm_` is an installed automation, `atr_` is a run; anything else
is a template slug.

## Dry run, test and live

`--dry-run` reads for real and shows, for each step that would send, write or
delete, what it would do and whether it would run, run under a pre-approval,
wait for approval or be refused, without doing it. `--test` runs it for real
with a sample, a recent event or an input file, under the normal approvals, and
asks first (`--yes` skips the question). A live run is a run of an enabled
automation. `--wait` follows the run (plain `--wait` is five minutes).

## Pre-approvals

A pre-approval lets one outward step, such as a message to a Slack channel, run
without an approval each time. It is pinned to a target, capped per day, lasts
at most 30 days and is signed with a passkey. Any change to the manifest, its
parameters, the run-as member or the connection voids it, and the step then
asks for approval each run until it is signed again. `show` lists each
pre-approval and the command that signs a voided one again, `upgrade` lists the
ones an upgrade voids, and `zuano doctor` reports automations that are paused
and steps that need re-approval.

## Agents

An agent session (`ZUANO_AGENT` set) may `list`, `show`, `logs`, `install` or
`draft` a draft and `run`; `enable`, `preapprove`, `uninstall` and an upgrade to a major
version are refused with exit 14 and the gateway refuses them too.

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | the run succeeded, or the change was made |
| 1 | failed, or the run was cancelled or skipped |
| 17 | a gateway request hit the time limit |
| 10 | a step was denied, or the command is refused for an agent session (publish) |
| 11 | a step's approval expired |
| 12 | the run waits for an approval (the link is printed), or `--wait` ran out |
| 13 | rate limited |
| 14 | a person must do this in the browser with a passkey |
| 16 | `draft`: the manifest has issues, each printed on its own line |
| 20 | the run failed |
| 30-33 | the plan does not allow it; see [billing.md](billing.md) |

## Drafting from a goal or a manifest

`draft` saves a private automation that is not from a template: describe it in
words and the gateway writes the manifest (with the organisation's model key, or
its platform model where the plan has one), or pass a manifest you or an agent
wrote. Either way the gateway checks it and returns the analysis (steps,
permissions, risk, connections still to make, minimum plan) and the page where a
person tests and enables it. The draft has no grants and no pre-approvals; a
manifest that claims a lower risk than the tool's registered class gets the
registered class. An invalid manifest is a `422` with every issue, printed one
per line, exit 16, and nothing is saved.

## Authoring

`validate` checks `automation.json` locally (manifest version, names, trigger
and step kinds, unique step ids, no secret-like parameter names) and then asks
the gateway to analyse it: the permissions with their risk, the minimum plan and
the steps that can be pre-approved. `publish` uploads it as a private template;
a template that pre-approves an outward step always waits for an
administrator's review.
