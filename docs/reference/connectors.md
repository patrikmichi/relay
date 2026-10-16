# Connectors

`relay connectors`, `relay connect` and the status column of `relay services`
talk to the gateway's service catalog and to the account-linking endpoints.

## Commands

| Command | What it does |
|---|---|
| `relay connectors` | Lists every service the gateway offers: slug, name, category, how it authenticates, tool count, whether your account has access |
| `relay connectors search <query>` | Same list filtered by name, id, category, tag, description or tool name. Every word must match; id and name hits rank first |
| `--category <name>` | Case-insensitive category filter. `--categories` lists the categories with counts |
| `--status` | Adds a STATUS column (one probe per listed connector) |
| `--json` | Raw rows for scripts |
| `relay connect <slug>` | Links your account for one connector |
| `relay services` | Tool counts per service, followed by `connected`, `not connected` or `not connected (admin)` for services that take credentials. `--no-status` skips the probes |

Status labels: `connected` (credentials resolve for you), `not connected`
(`relay connect` them), `not connected (admin)` (an administrator manages the
shared credentials, nothing for you to do), `no access` (the service is not
enabled for your account) and `-` (the service needs no credentials). A probe
that fails shows `unknown`.

## Endpoints

| Endpoint | Auth | Used for |
|---|---|---|
| `GET /api/integrations` | CLI token | The catalog: id, name, `authType`, category, tags, description, tools, `accessible` |
| `GET /api/connections?service=<slug>` | CLI token (planned) | `{"connected": boolean}`; the preferred status source because it also covers declarative OAuth connectors. relay falls back to the authorize probe below on 401, 403 or 404, or when the reply has no `connected` field, and stops asking for the rest of the run |
| `GET /api/cli/services/authorize?service=<id>&scope=<tool>` | CLI token | `accountLinked` and `authorizationPath` for the status column and for waiting on a flow. It needs a tool name, so relay passes the service's first tool |
| `GET /api/connections` | browser session | Per-connector credential fields and OAuth state |
| `POST /api/connections/keys` | browser session | Store API-key credentials |
| `GET /api/connectors/oauth/start?service=<slug>` | browser session | Starts the OAuth consent redirect |
| `GET /api/connectors/oauth/status?service=<slug>` | browser session | `needs_app`, `needs_connect`, `connected`, `reconnect` |
| `GET /api/connections/google/authorize` | browser session | Google Workspace consent |

"Browser session" endpoints check the gateway's session cookie, not the CLI
token. `relay connect` therefore works in two modes and picks one itself:

- **Bearer-capable gateway** (`GET /api/connections` answers the CLI token):
  API-key connectors prompt for each credential field without echo and POST
  them. OAuth connectors open the `connectUrl` the gateway advertises and
  poll the status endpoint.
- **Session-only gateway** (the case today): relay opens the gateway's
  `/settings/connections` page (or `/api/connections/google/authorize` for
  Google Workspace, or the OAuth start URL with `--oauth`) in your browser
  and polls `/api/cli/services/authorize` until `accountLinked` turns true,
  up to `--wait` (default 5 minutes). Sign in to the gateway in that browser
  first.

While waiting, relay asks the status endpoint first, so declarative OAuth
connectors complete as soon as the gateway reports `connected: true`. Against a
gateway without that endpoint the authorize probe may never report
`accountLinked` for an OAuth credential, and relay times out even though the
flow succeeded; confirm in the gateway's connection page. The same lookup
drives the `already connected` check and the status columns.

`relay connect` never writes credentials to disk. `--field KEY=VALUE` and
`RELAY_CONNECT_<KEY>` supply values non-interactively; without a terminal and
without values a required field is an error rather than a hang.

## Skills that need connectors

A catalog skill can declare the connectors it works with in its manifest
(`requires.connectors`, each with a `slug` and optionally `tools` and `scopes`).
`relay skill install` and `relay skill info` read them from
`GET /api/marketplace/skills/{id}/requirements`, which answers for the calling
user: each requirement with `connected`, plus `satisfied` and `missing`.

```
relay skill info weekly-timesheet
relay skill install weekly-timesheet --to claude
```

`install` lists the missing connectors, then for each one:

| Option | Behavior |
|---|---|
| (default) | Asks `Connect <slug> now? [Y/n]` and runs `relay connect <slug>`. Without a terminal it does not ask, so a pipeline never waits |
| `--yes` | Connects every missing connector without asking |
| `--no-connect` | Only reports; nothing is connected |
| `--ignore-requirements` | Installs even if some stay unmet, with a warning |

It then asks the gateway again. If a connector is still missing the install
fails before anything is written, naming the connectors and the two ways out.
`--dry-run` reports the missing connectors and connects nothing. A gateway
without the route, or one that fails to answer, only produces a warning: the
check never blocks an install it could not make.

`relay skill info <id|slug>` prints the version, whether and where the skill
is installed on this machine, the requirements with `connected` or `missing`,
and a ready or not-ready line. `--json` prints the same as one document.

`relay sync` copies skills between agents without contacting the gateway, so
it cannot know which connectors are linked. It reads each source skill's
`SKILL.md` frontmatter and, for one that declares `requires.connectors`, prints
a warning naming the skill and its connectors. Nothing is blocked or connected.

A connection change from an agent session (`relay login --agent`) is refused by
the gateway. relay prints its reason and tells you to run the command with your
personal profile (unset `RELAY_AGENT`) or to use the MCP tools; see
[approvals.md](approvals.md).

## Gateway MCP server

`relay sync` and `relay mcp install <catalog-service>` register the gateway's
aggregate MCP endpoint, `<gateway>/api/mcp`, as the server named `relay` in
each agent. Every catalog service, including custom MCP servers published to
the catalog, is reachable through it. With `GATEWAY_API_KEY` set when you run
the sync, entries reference the variable; without it they carry no
credentials and the agent authenticates with the gateway's MCP OAuth
endpoints. `relay mcp remove relay` stops syncing it.
