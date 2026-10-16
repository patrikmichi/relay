# Compliance packs and evidence bundles

A compliance pack maps a framework's controls (SOC 2, ISO 27001, GDPR, EU AI
Act, NIS2 and DORA) to evidence the gateway holds. An export is a signed bundle
for a date range that an auditor can check without trusting the gateway. A pack
shows what the gateway can evidence and where a person must supply it; it is not
legal advice and does not certify compliance.

## Commands

| Command | Request |
|---|---|
| `zuano compliance packs [--from D --to D]` | `GET /api/compliance/packs` |
| `zuano compliance controls --pack P [--from D --to D] [--gaps]` | `GET /api/compliance/packs/{pack}/controls` |
| `zuano compliance export --pack P --from D --to D [--wait [--wait-timeout T] [--out F]]` | `POST /api/compliance/exports`, the job, then `GET .../{id}/download` |
| `zuano compliance exports list\|show <id>\|delete <id>` | `/api/compliance/exports[/{id}]` |
| `zuano compliance schedules list\|create\|delete <id>` | `/api/compliance/schedules[/{id}]` |
| `zuano compliance share <export> --to EMAIL [--expires 14d]` | `POST /api/compliance/exports/{id}/shares` |
| `zuano compliance verify <bundle.zip> [--keys F]` | none for a keys file; otherwise `GET /.well-known/zuano-evidence-keys` and `-audit-keys` |
| `zuano compliance incidents list\|show <id>\|create` | `/api/compliance/incidents` |
| `zuano compliance dsr list\|create\|execute <id>` | `/api/compliance/dsr` |
| `zuano compliance access-review start\|status <id>\|apply <id>` | `/api/compliance/access-reviews` |

Dates are `YYYY-MM-DD`; a range includes `--from` and stops before `--to`. The
shapes are in [gateway-contracts.md](gateway-contracts.md).

`export --wait` follows the job and, with `--out`, downloads the bundle from its
short-lived address (https, or on the gateway; no credential is sent, and a
bundle that does not match the hash the gateway gave is discarded). Exit 0 ready,
4 still running when `--wait-timeout` ended, 20 the export failed, 30-33 the plan
does not allow it.

`share` prints a link for the auditor, who needs a code sent to the email you
name; it stops working after `--expires`.

## Verifying a bundle

`verify` needs no login. It checks:

1. the manifest's signature with the evidence key it names;
2. that every file matches the hash and size the signed manifest lists, and that
   the archive holds nothing the manifest does not list;
3. the audit hash chain, event by event: an event's hash is
   `sha256(prev_hash || canonical JSON of the event without hash and prev_hash)`,
   each event continues from the one before, and the first continues from the
   start checkpoint;
4. each signed daily checkpoint with the audit key it names, and that the chain
   recomputes to its head at its sequence number.

Events after the last signed checkpoint are reported as a warning. Exit 0 when
valid; 40 when not, naming each file or event that failed; 41 when the public
keys could not be obtained. `--keys` takes a file you got from the operator by
another route, the stricter way: `{"keys": [...]}` or `{"evidence": [...],
"audit": [...]}`.

## Agents

Everything that changes something (export, schedules, share, delete, incident,
request, access review) is refused while `ZUANO_AGENT` is set (exit 10); the
read commands and `verify` are not. `dsr execute` (export or erase a person's
data) and `access-review apply` (remove the access the reviewers revoked) ask
first and need a passkey confirmation.
