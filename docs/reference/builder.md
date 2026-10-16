# Auth configs, accounts, links and sessions

For apps that give their own users agents. The gateway keeps, per organisation
and project, how each service authenticates (auth configs), whose credential
is connected for it (accounts), how a person connects one (links), and what an
agent may do for one user (sessions). All of these call `/api/v1` with your
login or API key.

Secrets never travel in a flag: credentials are a JSON object on standard input
or in a file (`--from-file`), they are write-only on the gateway, and no command
prints one back. An agent session (`ZUANO_AGENT` set) cannot create a config with
secrets, set or rotate one, import or change account credentials, or mint a
session token (exit 10).

## Auth configs

| Command | |
|---|---|
| `zuano auth-configs list [--service S] [--search Q] [--project prj_...] [--show-disabled] [--limit N] [--cursor C]` | the configs, with scope (organisation or project) and state |
| `zuano auth-configs show <ac_id>` | one config; each secret field is only "set" or "not set" |
| `zuano auth-configs fields <service> [--scheme S] [--required-only]` | what creating one needs, and the redirect URI to register with the provider |
| `zuano auth-configs create --service S --name N --type managed\|custom --scheme S [--scopes A,B] [--credential-source config\|connect_time] [--redirect-host platform\|whitelabel] [--project prj_...] [--share-with-projects] [--default] [--from-file F \| --secrets-stdin]` | create a config |
| `zuano auth-configs update <ac_id> [--name N] [--scopes A,B] [--redirect-host H] [--share-with-projects] [--default]` | change it |
| `zuano auth-configs enable <ac_id>` | turn it back on |
| `zuano auth-configs disable <ac_id> [--suspend-accounts]` | new connections through a disabled config are refused |
| `zuano auth-configs delete <ac_id> [--yes]` | only a config without accounts |
| `zuano auth-configs secrets set <ac_id> [--from-file F]` | replace the named credential fields |
| `zuano auth-configs secrets rotate <ac_id> [--from-file F] [--grace 24h]` | new values; the old ones keep working for the grace window (1m to 168h) |

A `managed` config uses the platform's app and takes no credentials. A
`custom` one carries yours: client id and secret for OAuth, the key for an API
key service (`config` shares one key across accounts; `connect_time` has each
owner supply theirs).

```bash
echo '{"client_id":"...","client_secret":"..."}' | zuano auth-configs create \
  --service github --name "Our app" --type custom --scheme oauth2 --secrets-stdin --default
```

## Accounts

| Command | |
|---|---|
| `zuano accounts list [--user ID] [--service S] [--auth-config ac_...] [--status S] [--alias A] [--cursor C]` | connected accounts |
| `zuano accounts show <ca_id>` | one account |
| `zuano accounts update <ca_id> [--alias A] [--primary]` | rename it or make it the default for its owner and service |
| `zuano accounts delete <ca_id> [--yes]` | revoke at the provider and delete the credentials |
| `zuano accounts credentials <ca_id> [--from-file F]` | push rotated credentials; omitted fields are kept, `null` removes one |
| `zuano accounts import --user ID --service S --auth-config ac_... [--alias A] [--from-file F]` | bring in an API key, a bearer token or an OAuth refresh token (custom OAuth configs only) |
| `zuano accounts import-batch <rows.jsonl>` | up to 1000 imports as a job, one object per line |

Statuses: `initiated`, `active`, `needs_reconnect`, `suspended`, `revoked`,
`expired`.

## Connect links

| Command | |
|---|---|
| `zuano links create --user ID --service S [--auth-config ac_...] [--alias A] [--callback-url U] [--scopes A,B] [--expires-in 15m]` | make a link for a person to connect an account; it works for 1 minute to 1 hour |
| `zuano links show <lnk_id> [--wait DUR]` | `initiated`, `completed`, `failed` or `expired`; `--wait` follows it (exit 17 if nobody used it in time) |

The callback address must match the project's allowlist.

## Sessions

Under `zuano builder`, because `zuano sessions` is your own browser and CLI sessions.

| Command | |
|---|---|
| `zuano builder sessions create --user ID [--config-file F] [--config-id scf_...] [--toolkits A,B] [--disable-toolkits A,B] [--risk-disable C] [--require-approval C] [--auth-config svc=ac_...] [--account svc=ca_...] [--identity service\|delegated_user] [--preset meta\|direct] [--idle-days N] [--memory] [--sandbox] [--metadata k=v]` | create a session; flags override the file |
| `zuano builder sessions list [--user ID] [--limit N] [--cursor C]` | the sessions |
| `zuano builder sessions show <ses_id>` | one session; `--json` has the whole policy |
| `zuano builder sessions update <ses_id> --expected-version N [same policy flags]` | a version that moved is a conflict and nothing is written |
| `zuano builder sessions delete <ses_id> [--yes]` | delete its state, tokens and pending approvals |
| `zuano builder sessions history <ses_id>` | the versions of the policy |
| `zuano builder sessions toolkits <ses_id>` | which services the session may use and whether each is connected |
| `zuano builder sessions token <ses_id> [--expires-in 1h]` | a token for an agent, printed once on standard output (at most 24h) |
| `zuano builder sessions revoke-token <ses_id> <token_id>` | |
| `zuano builder session-configs list` | saved policies a session can use by id |
| `zuano builder session-configs show <scf_id>` | one, as JSON |
| `zuano builder session-configs create --name N [policy flags]` | save a policy |
| `zuano builder session-configs update <scf_id> [--name N] [--propagate] [policy flags]` | a new version; `--propagate` applies it to the sessions that use it |
| `zuano builder session-configs delete <scf_id> [--yes]` | |

A session only narrows the organisation's and project's policy: `policy_widening`
lists what it asked for beyond them. Risk classes are `read`, `write`,
`destructive` and `outward`.

## Exit status

The gateway's builder refusals fall into a few kinds, and the exit status
follows the kind rather than the code:

| Status | Refusals |
|---|---|
| 1 | a request that is wrong or conflicts: `auth_config_required`, `auth_config_invalid`, `auth_config_disabled`, `account_config_mismatch`, `policy_widening`, `request_state_invalid`, `idempotency_key_reused`, `idempotency_in_progress`, `refresh_requires_custom_app`, `callback_not_allowed`, `zdr_conflict`, `config_conflict`, an unknown id |
| 10 | a denial: the key may not do this (`approval_unavailable` and other 403) |
| 12 | an agent session's write is held for approval |
| 13 | rate limited |
| 17 | `links show --wait` ran out |
| 30-33 | the plan, an allowance, billing or a suspension; see [billing.md](billing.md) |
