# Skill evals

Score a marketplace skill, compare versions, and let the gateway propose a
better description or set of instructions. The commands wrap the gateway's
skill evals API (`/api/marketplace/skills/{id}/...`); the rules, limits and
response shapes are documented there.

A `<skill>` argument is a catalog id (`res_...`) or a slug. A version is a
semver (`1.2.0`, `v1.2.0`) or a `rev_` id; a draft is an `sdr_` id. An eval set
is its `evs_` id or its exact name; when a skill has only one set, `--set` can
be left out.

Everything here needs a gateway and a login. Starting a run or an optimization
needs the hosted runtime on the gateway (`503` otherwise) and an admin, owner or
the skill's author. An agent session (`zuano login --agent`) can read all of it
and is refused on every command that changes something; see
[approvals.md](approvals.md). Every command takes `--json` (the gateway's body,
indented) and `--gateway-url`.

## Sets and cases

```bash
zuano skill evals sets list weekly-timesheet
zuano skill evals sets create weekly-timesheet --name smoke --version 1.2.0
zuano skill evals sets show weekly-timesheet smoke
zuano skill evals sets export weekly-timesheet smoke --out smoke.json
zuano skill evals sets import other-skill smoke.json
zuano skill evals sets delete weekly-timesheet smoke --yes

zuano skill evals case add weekly-timesheet smoke --kind trigger \
  --name "asks for an invoice" --input "Please invoice Acme for March" --should-trigger
zuano skill evals case add weekly-timesheet smoke --kind task \
  --name "invoice total" --input "What is the total of invoice 42?" \
  --contains "1 250" --regex 'EUR|€' --rubric "States the total and the currency."
zuano skill evals case remove weekly-timesheet smoke evc_123
```

| Command | Request |
|---|---|
| `sets list <skill>` | `GET /evals/sets` |
| `sets show <skill> <set>` | `GET /evals/sets/{setId}` |
| `sets create <skill> --name N [--description] [--version] [--train-percent 10-90]` | `POST /evals/sets` |
| `sets import <skill> <file\|-> [--version]` | `POST /evals/import` |
| `sets export <skill> <set> [--out FILE]` | `GET /evals/sets/{setId}/export` |
| `sets delete <skill> <set> --yes` | `DELETE /evals/sets/{setId}`; also deletes the set's cases and runs, so `--yes` is required |
| `case add <skill> <set> ...` | `POST /evals/sets/{setId}/cases` |
| `case remove <skill> <set> <case-id>` | `DELETE /evals/sets/{setId}/cases/{caseId}` |

`case add` flags: `--kind trigger|task`, `--name`, `--input`; for a trigger case
`--should-trigger` (`=false` for a negative case); for a task case any of
`--contains`, `--not-contains`, `--regex` (repeatable) and `--rubric`,
`--reference`; `--connector` (repeatable) skips the case when a run does not
reach that connector; `--split train|validation` pins the case. `--file FILE`
(or `-`) reads the whole case as JSON, which is how `json_path` and `structured`
assertions are written. zuano checks the flags before sending anything.

`sets export` writes a `zuano.skill-evals/v1` document that `sets import` reads,
so a set can move between skills and organizations.

## Running evals

```bash
zuano skill eval weekly-timesheet                          # start; prints the run id
zuano skill eval weekly-timesheet --wait                   # start and print the scores
zuano skill eval weekly-timesheet --version 1.1.0 --repeats 5 --split validation --wait
zuano skill eval weekly-timesheet --draft sdr_123 --wait
zuano skill eval weekly-timesheet --run evr_123 --wait     # follow a run started earlier
zuano skill eval weekly-timesheet --run evr_123 --cancel
zuano skill compare weekly-timesheet --versions 1.1.0,1.2.0 --wait
zuano skill compare weekly-timesheet --versions 1.1.0,sdr_123 --fail-on-regression
```

Without `--version` or `--draft`, `eval` scores the current published version.
`--kind trigger|task` is needed only when the set holds both kinds. `--repeats`
(1-10, default 3), `--split`, `--model`, `--judge-model` and `--max-tokens`
(which can only lower the budget) go to `POST /evals/runs`.

`--wait` polls `GET /evals/runs/{runId}` every few seconds, printing progress on
stderr, until the run is `succeeded`, `failed` or `cancelled`, or until
`--wait-timeout` (default 20 minutes) passes. The run keeps going on the
gateway when zuano stops waiting, and the error says which command resumes it.
With `--json`, only the final document is printed.

`compare` is `eval` with two or more variants. The first is the baseline; each
other variant gets wins, losses and ties per case and a paired bootstrap test.

A trigger run reports precision, recall and F1 per split; a task run reports the
mean score and the pass rate. Both report tokens, cost, latency, errors (model or
platform failures, left out of the scores) and skipped cases (the case needs a
connector the run did not reach).

### Exit status

| Status | Meaning |
|---|---|
| 0 | the run finished (or, without `--wait`, started) |
| 1 | an error: bad arguments, a refusal by the gateway, a failed request |
| 4 | `--wait-timeout`, or zuano's own request limit, passed |
| 20 | the run or optimization job ended `failed` or `cancelled` |
| 21 | `--fail-on-regression`, and a score dropped |

`--fail-on-regression` implies `--wait`. It counts as a regression when the
gateway recorded `summary.regressions` (the version scores lower on validation
than the previous version on the same set and kind, by more than 0.02), or when a
compared variant is significantly worse than the baseline on validation (on all
cases when the run had no validation split). The gateway records
regressions just after it marks a run `succeeded`, so zuano reads the run once
more before it decides. In CI:

```bash
zuano skill eval weekly-timesheet --wait --fail-on-regression --json > eval.json
```

## Optimization and drafts

```bash
zuano skill optimize weekly-timesheet --kind description --set smoke --wait
zuano skill optimize weekly-timesheet --job opj_123 --wait     # follow a job
zuano skill drafts list weekly-timesheet
zuano skill drafts show weekly-timesheet sdr_123
zuano skill drafts promote weekly-timesheet sdr_123 --version 1.3.0
zuano skill drafts discard weekly-timesheet sdr_123
```

`optimize` starts a job on a set: it scores the version, asks the model for
`--candidates` rewrites of the description (`--kind description`, trigger cases)
or of the SKILL.md (`--kind instructions`, task cases) from the failing **train**
cases only, scores each on train, scores the best once on validation, and keeps
it as a draft only if it beat the version on train. The result lists each
candidate's train and validation gain and whether it looks overfit. Options:
`--set`, `--version`, `--candidates`, `--repeats`, `--model`, `--judge-model`,
`--max-tokens`, `--overfit-threshold`, `--wait`, `--wait-timeout` (default 30
minutes), `--job`, `--cancel`. A `failed` or `cancelled` job exits 20.

A draft never publishes by itself. `drafts promote` publishes it through the
normal publish pipeline, so every gate and review applies and the new version
can land in a state that waits for approval; `--version X.Y.Z` sets the semver
(default: next patch). A draft can also be scored with `eval --draft`.

## Quality

```bash
zuano skill quality weekly-timesheet     # score, latest per kind, history by version
zuano skill quality --all                # every skill of the organization, weakest first
```

The score is 0 to 100: the mean over kinds of the latest validation score (train
when there is no validation score), or `not evaluated` until the skill has run.
`--all` needs an admin or owner.

## Errors

zuano prints the gateway's message. Some it explains further: `NOT_ENABLED`
(the gateway has not enabled the hosted runtime), `SKILL_REQUIREMENTS_UNMET`
(the skill's connectors are not connected: `zuano connect` them first),
`VALIDATION_ERROR` (with the offending fields) and `EVAL_LIMIT` (cases x
variants x repeats is over the gateway's cap; lower `--repeats` or use fewer
variants).
