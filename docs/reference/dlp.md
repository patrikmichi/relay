# Data loss prevention

DLP finds secrets and personal data in what goes to tools and what comes back,
and blocks, masks, tokenizes or holds it for approval by rule. `zuano dlp` reads
and replaces the policy, tests a sample, lists findings and records feedback.

## Commands

| Command | Request |
|---|---|
| `zuano dlp policy get [--yaml]` | `GET /api/admin/dlp/policy` |
| `zuano dlp policy apply <file> [--dry-run]` | `PUT /api/admin/dlp/policy[?dryRun=true]` with the file as YAML or JSON |
| `zuano dlp policy versions` | `GET /api/admin/dlp/policy/versions` |
| `zuano dlp test [--service S --tool T] [--direction in\|out] < file` | `POST /api/admin/dlp/test` |
| `zuano dlp findings [--since 24h] [--class 'pii.*'] [--service S]` | `GET /api/admin/dlp/findings` |
| `zuano dlp stats [--since 7d] [--group-by class\|service\|tool\|agent\|rule]` | `GET /api/admin/dlp/stats` |
| `zuano dlp feedback <finding> --false-positive\|--true-positive [--scope value\|tool\|path]` | `POST /api/dlp/findings/{id}/feedback` |
| `zuano dlp hash --detector iban` | `POST /api/admin/dlp/hash` |
| `zuano connectors profile <slug> [--set-residency eu --basis org_verified]` | `GET /api/admin/connectors/data-profiles`, `PUT .../{slug}/data-profile` |

The shapes are in [gateway-contracts.md](gateway-contracts.md).

`policy apply --dry-run` checks the file and prints the difference from the
active policy and what the new rules would have done to recent real traffic
(blocked, held, masked, tokenized, by rule). Without `--dry-run` the file becomes
a new version; in strict mode, or when it loosens anything, that needs a passkey
confirmation (exit 14 when nobody can give it). Problems in the file come back
with file and line and end with exit 40.

`test` and `hash` read standard input, never an argument, so the value stays out
of shell history; `hash` also takes a hidden prompt on a terminal. `test` prints
classes with masked previews; the gateway neither logs nor stores the sample.
Findings never hold the value, only the class, detector and a count.

A call DLP blocks ends with exit 10 and one it holds for approval with exit 12,
like any other refusal or held call.

## Agents

`policy apply` (not `--dry-run`) and `connectors profile --set-residency` change
the organisation's rules and are refused while `ZUANO_AGENT` is set (exit 10).
The rest only reads or tests.
