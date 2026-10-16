# Background runs

Agent and prompt runs, code runs, skill evals and optimisations, webhook
deliveries and host builds run on the gateway as durable jobs. A job survives
a crash or a deploy, keeps a status, and can be cancelled or, once it failed
every retry, started again.

Job status: `queued`, `running`, `retrying`, then one of `succeeded`,
`failed`, `cancelled` or `dead` (it failed every retry and was
dead-lettered). `queued` and `retrying` are not the end: every `--wait` keeps
polling through them, and `dead` ends it as a failure.

## Calls that start a job

A tool that starts background work answers before it finishes. zuano
recognises two shapes of the answer, both with a `job_id` or a `run_id` and a
status of `queued`, `running` or `retrying`:

- HTTP `202` with `{"run_id", "job_id", "status"}`;
- an MCP tool result whose `structuredContent` has that shape.

`zuano call <service> <tool>` prints what started and how to follow it, and
exits 0:

```
Started on the gateway: job_01J9ZK3 (queued).
Follow it with `zuano runs show job_01J9ZK3`, or repeat the call with --wait.
```

With `--wait[=SECONDS]` it polls `GET /api/jobs/{id}` (a run id with no job id
polls `GET /api/runtime/runs/{id}`, which carries `job_status`) at one, two,
three, then every five seconds, and prints the finished job. The exit status is
0 for `succeeded`, 1 for `failed`, `dead` or `cancelled`, and 12 when the time
ended with the job still going. `--json` prints only the final document.

## zuano runs

| Command | Request |
|---|---|
| `zuano runs list [--kind K] [--status S] [--cursor C] [--json]` | `GET /api/jobs?kind=&status=&cursor=` |
| `zuano runs show <id> [--json]` | `GET /api/jobs/{id}`, or `GET /api/runtime/runs/{id}` for an id that does not start with `job_` |
| `zuano runs wait <id> [--wait-timeout T] [--json]` | the same route, polled until finished; exit status as above |
| `zuano runs cancel <id>` | `POST /api/jobs/{id}/cancel`, or `POST /api/runtime/runs/{id}/cancel` for a run id |
| `zuano runs retry <job-id>` | `POST /api/jobs/{id}/retry` (organisation admins; failed or dead jobs only) |

Members see their own jobs; admins see all of the organisation's, metadata
only. `zuano usage` shows the quotas that bound how many jobs may start; a
start refused by a quota exits 13 (see [billing.md](billing.md#exit-statuses)).

## Skill evals

`zuano skill eval --wait` and `zuano skill optimize --wait` follow `queued` and
`retrying` and stop at `succeeded`, `failed`, `cancelled` or `dead`; the last
three exit with the eval failure status.
