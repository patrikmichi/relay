# Policy as code

The organisation's risk policy, grants, groups, approval settings, agent rules
and security settings can be kept as YAML files in a Git repository: reviewed as
a pull request, checked with `zuano policy plan` and applied with
`zuano policy apply`, from your machine or from CI. Every change, from the
dashboard or from a file, is a numbered **release** that can be rolled back.

## Commands

| Command | Gateway | Request |
|---|---|---|
| `zuano policy init [--dir D] [--ci github\|gitlab] [--from-live]` | with `--from-live` | `GET /api/policy/document?format=yaml` |
| `zuano policy fmt [--check]` | no | local |
| `zuano policy validate [--offline]` | optional | local schema check, then `POST /api/policy/validate` |
| `zuano policy export [--format yaml\|json]` | yes | `GET /api/policy/document` |
| `zuano policy plan [--format table\|markdown\|json] [--out F] [--fail-on-loosening] [--detailed-exitcode]` | yes | `POST /api/policy/plan` |
| `zuano policy apply [--wait SECONDS] [--resume ID] [--auto-approve]` | yes | `POST /api/policy/apply`, `GET /api/policy/pending/{id}` |
| `zuano policy status`, `diff --live` | yes | `GET /api/policy/status`, `/releases/{id}/diff?against=live` |
| `zuano policy releases [list]`, `show <id>`, `diff <a> <b>` | yes | `GET /api/policy/releases[/{id}[/diff]]` |
| `zuano policy rollback <id> [--yes]`, `freeze [--yes]` | yes | `POST /api/policy/releases/{id}/rollback`, `/api/policy/freeze` |
| `zuano policy explain <service> <tool>` | yes | `GET /api/admin/risk-policy/preview`; also prints the file and line of the deciding rule |

`--dir` (default `zuano`) is the directory of policy files. The shapes are in
[gateway-contracts.md](gateway-contracts.md).

## The files

A policy is a directory of `*.yaml` files merged by top-level section; a section
defined in two files is an error, and links are not followed. `version` and
`org` are required. The JSON Schema is embedded in zuano
(`internal/policyfile/schema/policy-v1.json`), checked offline with file and
line, and written into the file as a `yaml-language-server` header by `init` so
editors offer completion. `make gen-policy-schema GATEWAY_SCHEMA=...` copies in
the gateway's published schema; with `ZUANO_CONTRACT_FIXTURES` set, a test fails
when the two differ. The gateway adds checks the schema cannot express
(references, the safety floor, plan limits, secrets in a policy) and returns them
with file and line too.

`fmt` rewrites each file in the canonical layout, keeping comments; `fmt --check`
exits 1 when a file would change.

## Plan and apply

`plan` sends the files and prints what would change: the document diff with
files and lines, the identities and tools whose mode changes (loosening first),
the classification (`tightening`, `loosening`, `mixed`, `neutral`) and lifecycle
effects. `--format markdown` is a pull request comment, cut to 60,000
characters with a link to the full plan. It changes nothing.

`apply` shows the plan, asks (`--auto-approve` skips only this question, for CI)
and applies it as a release; it fails with exit 41 if the live policy changed
since the plan. When the organisation requires an approval for the change
(always, or when it loosens), apply is held: an administrator reviews the impact
and confirms with a passkey. `--wait` follows it for that many seconds;
otherwise, or when it runs out, apply exits 12 and
`zuano policy apply --resume <id>` completes it later. Applying from a personal
session needs a passkey confirmation (exit 14 when nobody can give it).

`rollback` applies an earlier release again as a new release. In a repository-
managed policy the gateway opens a pull request that brings the repository in
line, or prints the `git revert` to run. `freeze` sets every write, destructive
and outward action to off for agents and needs a passkey confirmation.
`apply`, `rollback` and `freeze` are refused while `ZUANO_AGENT` is set.

## CI

`zuano policy init --ci github` writes `.github/workflows/zuano-policy.yml` and
`--ci gitlab` writes `.gitlab/zuano-policy.yml`; the same files are in
`docs/ci/github-policy.yml` and `docs/ci/gitlab-policy.yml` (include the GitLab
one). They plan on every pull request or merge request and apply on merge, and
they sign in with the job's OIDC token: no secret is stored. Set the variable
`ZUANO_URL` to the gateway's address.

Inside a GitHub Actions or GitLab job without other credentials, the policy
commands exchange the job's token at `POST /api/policy/ci/token` for a
ten-minute token of the repository's workload identity. GitHub's token comes
from the Actions token endpoint; GitLab's from the variable `ZUANO_ID_TOKEN` the
template declares under `id_tokens`. A pull request from a fork gets no token:
`plan` then prints that it was skipped and exits 0; every other command fails
with that reason.

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | done |
| 1 | `fmt --check` found a file to format, or `plan --fail-on-loosening` found a loosening |
| 2 | `plan --detailed-exitcode`: there are changes |
| 10 | refused for an agent session, or an administrator denied a held apply |
| 11 | a held apply expired |
| 12 | a held apply is still waiting |
| 14 | a passkey confirmation is needed and nobody can give it |
| 40 | the files are not a valid policy |
| 41 | the live policy changed since the plan; plan again |
| 42 | the policy is managed in Git; change it by pull request |
| 30-33 | the plan does not allow it; see [billing.md](billing.md) |
