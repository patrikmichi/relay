# Starter packs

A starter pack is a versioned, signed set of connectors, skills, plugin bundles,
rules for coding agents, a risk and approval preset and automation templates
that sets an organisation up for one kind of work. Installing is plan, preview,
apply; members receive the pack's rules, skills and bundles on their next
`zuano sync`.

## Commands

| Command | Request |
|---|---|
| `zuano pack list [--category C] [--installed] [--json]` | `GET /api/packs` |
| `zuano pack show <pack>[@version] [--json]` | `GET /api/packs/{slug}` |
| `zuano pack plan <pack>[@version] [--param k=v] [--params-file F] [--json]` | `POST /api/packs/{slug}/plans` |
| `zuano pack install <pack>[@version] [--param k=v] [--keep-conflicts\|--take-conflicts\|--decide SEQ=keep\|take] [--yes] [--no-connect] [--json]` | plan, then `POST /api/packs/plans/{id}/apply` |
| `zuano pack update <pack> [--to-version V]` | plan with `mode: update`, then apply |
| `zuano pack uninstall <pack> [--disable-connectors]` | plan with `mode: uninstall`, then apply |
| `zuano pack rollback <pack> --to V` | plan with `mode: rollback`, then apply |
| `zuano pack status [<pack>] [--json]` | `GET /api/packs/installs` |
| `zuano pack init <dir>`, `validate <dir>`, `publish <dir> [--channel stable\|beta]` | authoring |

The request and response shapes are in [gateway-contracts.md](gateway-contracts.md).

## Plan, preview, apply

The gateway signs every plan. zuano checks the signature against the keys
pinned for the gateway before it prints anything; a plan that is unsigned,
altered or signed by another key exits with status 4 and nothing is applied.

The preview groups the plan by area: connectors, groups, skills and bundles,
risk and approvals, automations and member rules. Each change shows its value
now and after, a `[loosens]` mark when it makes a control less strict, and for
a change the plan does not allow the plan that would. A conflict is a value an
administrator changed since the pack set it; choose `keep` or `take` for it
with `--keep-conflicts`, `--take-conflicts` or `--decide SEQ=keep|take`.

`install`, `update`, `uninstall` and `rollback` ask before applying (`--yes`
skips the question and is required without a terminal). The plan is applied
only if the organisation is unchanged since it was made; otherwise nothing is
applied and zuano asks you to run the plan again. A plan that loosens a control
needs a passkey confirmation, which is decided in the browser or on your phone;
zuano only opens the page.

After applying, zuano lists the connections the pack needs and, at a terminal,
offers to make each organisation-wide one with `zuano connect`. Credentials are
never created or deleted by a pack.

Packs are for people. With `ZUANO_AGENT` set, `install`, `update`, `uninstall`,
`rollback` and `publish` exit 10; an agent uses the gateway's pack tools, which
ask a person to approve.

## Member delivery

`zuano sync` fetches the member's assignments from the gateway when one is
configured and you are signed in. The list is signed and checked first.

- Rules are written like the rules from the sync directory, named
  `zuano-pack-<pack>-<rule>`, and limited to the agents a rule names. A block
  you edited is reported as a conflict and kept; a rule that is no longer
  assigned is removed.
- Skills and plugin bundles that are new or changed are installed. Hooks and
  local servers in a bundle still need `--allow-hooks` or a confirmation, so a
  sync without a terminal reports such a bundle instead of installing it.
- A skill or bundle that is no longer assigned is reported with the command
  that removes it; sync does not delete it.
- Connections the member still needs are listed with `zuano connect <slug>`.

`--no-packs` skips all of it; without a gateway nothing is fetched. The last
delivered revision is kept in `~/.config/zuano/sync/assignments.json`, so an
unchanged revision costs a `304`. `zuano doctor` has a `packs` row that says
whether a newer revision is waiting.

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | done |
| 1 | failed, or the plan changed since it was shown |
| 4 | a signature did not verify |
| 10 | refused for an agent session |
| 13 | rate limited |
| 14 | a passkey confirmation is needed and nobody can give it |
| 30-33 | the plan does not allow it; see [billing.md](billing.md) |

## Authoring

`zuano pack init <dir>` writes a `pack.yaml` to start from. `validate` checks
the file (format, names, versions, placeholders, rule limits, the automation
actions a pack may use) and, with a gateway, whether every referenced skill,
bundle and connector exists. `publish` uploads it as a private pack of your
organisation; a pack that loosens a control waits for an administrator's review.
