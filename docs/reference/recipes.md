# Recipes

A recipe is a playbook for using a few tools well: the steps in order, which
are optional or fall back to another, the pitfalls and what to do instead, and
how well it has worked. The gateway serves the best matching ones next to tool
search results. They come from the platform, from your organisation, or are
mined from its usage.

| Command | |
|---|---|
| `zuano recipes list [--service S] [--source platform\|mined\|org] [--status S] [--query Q] [--limit N] [--cursor C]` | id, title, services, steps, pitfalls, success rate, source, version |
| `zuano recipes show <id\|slug> [--version N]` | the recipe in full, with its signature checked |
| `zuano recipes versions <id>` | the versions of a recipe |
| `zuano recipes promote <id>` | make it into a draft skill; prints the next commands |
| `zuano recipes settings [--contribute on\|off] [--serve all\|org_only\|off] [--mine on\|off]` | show or change how the organisation takes part |

`--json` prints what the gateway sent, and `--gateway-url` names the gateway.

## What `show` prints

The title and a header line (services, steps, pitfalls, source, version,
status); the difficulty with its reason; the primary and related tools; the
numbered steps, with `optional` and `fallback` steps carrying their condition,
and for you the tool's risk, whether it needs your approval or is covered by a
grant, and whether a retry is safe (`idempotent`, `auto key` for a call the
gateway keys itself, `not idempotent`); the pitfalls grouped by tool with their
error codes and the fix; the evidence (success rate over the last 30 days, its
95 % lower bound, how many runs, the last eval); and the signature line.

Output has no colour, so `NO_COLOR` needs nothing. Text in a recipe may come
from outside your organisation, so control, bidirectional-override and
zero-width characters are removed from everything printed.

## Signatures

The platform signs a platform recipe with its recipe key, and an organisation
signs its own when it activates one. `show` checks the signature without asking
the gateway to vouch for it:

1. the content the signature covers hashes to the recipe's `contentHash`;
2. the signature is by a key the gateway publishes at `/api/v1/recipes/keys`
   (kept for 24 hours and fetched again when a recipe names a key it does not
   have, as after a rotation);
3. the recipe shown is that content: its title, use case, services, difficulty,
   tools, steps (order, kind, condition, tool, instruction) and pitfalls.

The risk, approval and idempotency notes are worked out for you after signing
and are not covered. A recipe that fails any check is printed with
`signature: NOT VERIFIED` and the reason, and the command exits 4. Do not follow
it.

## Settings

Without flags, `settings` prints them. `--contribute` shares anonymised
tool-usage patterns to improve recipes for everyone; `--serve` chooses which
recipes tool search serves (`all`, only your organisation's, or none); `--mine`
turns on mining your own usage for private recipes. Changing them is an
administrator's and is audited; it is refused in an agent session (exit 10).

## Exit status

| Status | Meaning |
|---|---|
| 4 | the recipe's signature does not verify |
| 10 | refused for an agent session, or not allowed |
| 12 | an agent session's write is held for approval |
| 13 | rate limited |
| 30-33 | the plan does not allow it; see [billing.md](billing.md) |
