# Evaluations

`zuano eval` runs a dataset of cases against a target on one or more models,
grades the answers, and fails a build when quality drops. A run happens on this
machine with `--offline`, using your own provider keys, or on the gateway.

| Command | |
|---|---|
| `zuano eval init [--target skill\|prompt\|agent\|mcp\|app] [--dir D] [--force]` | write `zuano.evals.yaml`, a sample dataset and a `.gitignore` line |
| `zuano eval run [--offline] [--suite id] [--target kind:slug] [--models a,b] [--repeats n] [--ci] [--out dir] [--no-wait] [--no-cache] [--timeout d] [--config file]` | run a suite and apply its gates |
| `zuano eval compare <run-a> [<run-b>] [--against baseline:<ref>] [--fail-on-regression] [--pairwise --judge provider/model [--rubric text] [--max-pairs n]]` | paired comparison of two runs |
| `zuano eval report [results.json\|run-id] [--format md\|html\|json] [--baseline ref] [--out file]` | render results |
| `zuano eval baseline set <ref> [--run file] [--cloud --suite id --run-id id]` | record a baseline |
| `zuano eval baseline show <ref> [--cloud --suite id]` | show a baseline |
| `zuano eval datasets import <file\|dir> --from jsonl\|csv\|promptfoo\|openai\|skill-creator\|claude-plugin [--out file] [--criteria file] [--column field=header]` | convert a file into cases |
| `zuano eval datasets push <cases.jsonl> (--name N \| --dataset id) [--sensitivity level]` | upload a dataset version |
| `zuano eval datasets pull <dataset-id> [--version n] [--out file]` | download a dataset version |
| `zuano eval optimize (--suite id \| --target kind:slug) --slot S [--candidates n] [--repeats n] [--models list] [--overfit x] [--test-split] [--apply] [--sink S] [--no-wait]` | propose better text on the gateway |
| `zuano eval review [--suite id] [--no-browser]` | open the human review queue |
| `zuano eval leaderboard (--suite id \| --target kind:slug) [--split S] [--tag T]` | rank models and versions |

They replace `zuano skill evals`, `eval`, `compare`, `optimize`, `drafts` and
`quality`: use `--target skill:<slug>`. Every command that talks to the gateway
takes `--gateway-url` and `--json`.

## Offline runs

```bash
export ANTHROPIC_API_KEY=...           # or OPENAI_API_KEY, GEMINI_API_KEY,
zuano eval init --target skill         # or OPENAI_COMPATIBLE_BASE_URL (and _API_KEY)
zuano eval run --offline
```

A run writes `results.json`, `report.md` and `report.html` to `--out`
(default `.zuano/evals/results`, which `init` adds to `.gitignore`). Keys are
read from the environment and sent only to their provider. Providers are called
over HTTP directly; `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL` and `GEMINI_BASE_URL`
point a provider at a proxy.

Runs on the gateway use the organisation's keys or platform credits. Offline
runs work without an account and are never refused for an agent session.

## Config file

`zuano.evals.yaml` is validated against an embedded JSON Schema. Paths resolve
from the file's directory.

```yaml
version: 1
prices:                              # USD per million tokens, for the estimated cost of your own keys
  claude-sonnet-5.5: { input: 3, output: 15 }
suites:
  - id: pr-triage-skill
    target: { kind: skill, path: skills/pr-triage/SKILL.md, catalog: skills/catalog.json }
    dataset: { path: evals/pr-triage.jsonl }       # or { cloud: evd_..., version: 7 }
    models: [anthropic/claude-sonnet-5.5, openai/gpt-6-mini]
    repeats: 3
    seed: 1
    split: { train: 70, test: 0 }                   # the rest is validation
    graders:
      assertions: []                                # applied to every task case
      judge: { model: google/gemini-3-pro, aggregate: mean, threshold: 0.6 }
    gates:
      min_pass_rate: 0.85
      no_regression_vs: main                        # baseline ref
      fail_on_inconclusive: false
```

Suite keys: `id`, `name`, `target`, `dataset`, `models`, `repeats`, `seed`,
`concurrency`, `temperature`, `max_tokens`, `timeout_seconds`, `split`,
`graders`, `gates`. Target keys: `kind`, `path`, `catalog`, `tools`, `url`,
`secret_env`, `system`, `template`, `slug`, `tool_names`.

### Targets

| Kind | Under test | Trigger cases | Task cases |
|---|---|---|---|
| `skill` | the `SKILL.md` description and instructions | which option a router picks among `catalog` plus this skill | a model loop with the skill's instructions |
| `prompt` | `system` and `template` (`{{variables}}`) | | one or more turns; `{{missing}}` is an error |
| `agent_config` | a subagent file; its `tools` list limits the tools | delegation among `catalog` plus this subagent | a model loop with the file's body; calls to tools outside the list are refused |
| `mcp_server` | a `tools/list` snapshot | which tool the model calls | a model loop over the tools |
| `app` | an HTTP hook | | the hook answers |
| `agent_endpoint` | gateway only | | |

Tools in task cases are answered from the case's `fixtures`; a call no fixture
matches is recorded and answered with `fixture_missing`, never run. A live MCP
server over stdio or HTTP is not queried: export `tools/list` to a file.

An `app` hook receives `POST url` with `{case_id, input, messages, variables, model}`
and a `Zuano-Signature: t=<unix>,v1=<hex>` header, the HMAC-SHA256 of
`<t>.<body>` under the secret in the environment variable named by
`secret_env`. It answers `{output, structured?, tool_calls?, usage?}`. A 5xx or
timeout is retried once; it counts as an error of the target, not a failed case.

## Cases

One JSON object per line, or a CSV with a header. Format `zuano.evals/v2`:

```json
{"kind": "task", "key": "total", "name": "states the total", "input": "What is the total of invoice 42?",
 "fixtures": [{"tool": "get_invoice", "match": {"id": "42"}, "response": {"total": "1 250 EUR"}}],
 "expected": {"assertions": [{"type": "icontains", "value": "1 250"}], "rubric": "States the currency", "reference": "1 250 EUR"},
 "split": "validation", "tags": ["billing"], "weight": 1}
{"kind": "trigger", "name": "merge", "input": "Merge a.pdf and b.pdf", "should_trigger": true}
{"kind": "trigger", "name": "pick", "input": "Show invoices", "expected_choice": "list_invoices"}
```

`kind` is `trigger`, `task`, `regression`, `adversarial` or `safety`. `input` is
text or `{"messages": [...], "variables": {...}}`. A case without `key` is
identified by a digest of what it asks. Cases without a `split` are assigned
one from a hash of the key and the seed, so a case keeps its split.

### Assertions

`contains`, `icontains`, `not_contains`, `not_icontains`, `equals`, `regex`
(RE2), `json_path` (`path` with `equals` or `subset`), `json_schema`
(JSON Schema 2020-12), `tool_sequence` (an ordered subsequence of calls with
argument subsets), `tool_count_lte`, `cost_lte`, `latency_lte`, `choice_equals`,
`refusal`, `no_pii`, `no_secret` (optionally limited to `classes`),
`no_injection_compliance` (`forbidden_tools`, `canary`). A `rubric` is graded by
the suite's judge; several judges aggregate by mean, majority or minimum, and a
warning appears when a judge is also a model under test.

Code graders and the real coding-agent harnesses need the gateway's sandbox and
run only there.

## Statistics

Per model and split: the mean score and its sample standard deviation, the pass
rate with a 95 % Wilson interval, pass@k and pass^k over the repeats, trigger
precision, recall and F1, and the cases whose repeats disagree more than 30 %
(flaky). A comparison pairs results by case and repeat: the mean change with a
bootstrap interval (seeded, so it repeats), wins, losses and ties (a score
difference under 0.01 is a tie), and McNemar's exact test on pass and fail.
P-values are corrected across all compared models with Holm-Bonferroni; only a
corrected p-value under 0.05 is "significant". A comparison that is not
significant and has fewer pairs than needed to see a 5-point change says how
many it would take (inconclusive). Errors of the provider or the target are
counted but not held against the model.

The grader and statistics results are checked against conformance vectors in
`internal/evals/testdata/conformance`; `make gen-evals-fixtures` copies the
gateway's vectors over them and records their hashes, and
`make gen-evals-schema` does the same for the config schema.

## Gates and exit status

| Code | Meaning |
|---|---|
| 0 | passed |
| 20 | no model completed a case, or the gateway run failed |
| 21 | a regression against the baseline, significant after correction |
| 23 | inconclusive and `fail_on_inconclusive` is set |
| 24 | `min_pass_rate` not met |
| 13 | rate limited |
| 30 | the plan lacks a feature or a limit is reached |
| 31 | an allowance or a spend cap is used up |
| 17 | `--timeout` ended before the gateway run did |

When several apply, 20 outranks 21, 21 outranks 24 and 24 outranks 23. A
`no_regression_vs` ref with no baseline yet is reported and skipped, so the
first run on a branch can create it. Baselines are files under
`.zuano/evals/baselines/<ref>.json`, meant to be committed; on the gateway
a baseline moves with `baseline set --cloud`, which protected refs reserve for
administrators.

## In CI

`--ci` records the commit, repository and pull request from GitHub or GitLab
variables on a gateway run, appends the Markdown table to `GITHUB_STEP_SUMMARY`
and prints a `::error::` annotation for every failed gate. The Markdown is the
text of the pull request comment.

## Agent sessions

`run` and `optimize` on the gateway spend the evaluation budget. With
`ZUANO_AGENT=1` they ask the gateway whether the organisation allows it
(`evals.agents_may_run`, off by default) and exit 10 without spending when it
does not or cannot be read. Everything else is read-only or local.

## Importing

`zuano eval datasets import` converts a file and prints what it could not:
nothing is converted into something weaker.

| `--from` | Reads | Notes |
|---|---|---|
| `jsonl` | cases | validated and normalised |
| `csv` | one case per row | `--column field=header`; a `should_trigger` or `expected_choice` column makes a trigger case |
| `promptfoo` | inline `tests` | `contains`, `icontains`, `equals`, `regex` (RE2 only), `contains-all`, `contains-any`, `is-json`, `llm-rubric`, `cost`, `latency` and the `not-` forms that exist here; `javascript`, `python`, similarity and the rest are listed. The prompts are listed so you can use one as the target's `template` |
| `openai` | OpenAI Evals items (JSON lines) | with `--criteria`, `string_check` `eq`, `like` and `ilike` are mapped; `text_similarity`, `label_model`, `score_model` and `ne` are listed. Without it each ideal answer becomes the reference of a rubric |
| `skill-creator` | a skill-creator `evals.json` | `expectations` become a rubric, `expected_output` the reference; input files are listed as not attached |
| `claude-plugin` | the `evals/` directory of a Claude Code plugin | `prompt.md` is the input; `llm` graders become the rubric, `regex` on the final message a `regex` (flag `i` kept), `tool_used` a `tool_sequence` (or a forbidden tool when `min` and `max` are 0), `tool_order` a `tool_sequence`. Graders on the trace, on files, on a reference transcript, with input matching or call counts, and `case.yaml` contexts are listed. Weights and `allowed_tools` are not carried over |

OpenAI has announced that its Evals product shuts down on 2026-11-30; export
your datasets before then.
