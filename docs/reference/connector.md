# zuano-connector

`zuano-connector` is the agent that runs inside your network. It dials out to
the broker over one WebSocket (port 443, `HTTPS_PROXY` honoured), receives
sealed requests from the gateway and runs the ones its local config allows
against HTTP and MCP targets only your network can reach. Nothing connects in.
Administrators manage networks, tokens and bindings with
[`zuano networks`](gateway-contracts.md#private-networks); this page is the
agent itself.

## Install

| Method | Steps |
|---|---|
| Tarball | Download `zuano-connector-<os>-<arch>.tar.gz`, check it with `cosign verify-blob` against `SHA256SUMS.txt`, unpack `zuano-connector` into `/usr/local/bin`. |
| `.deb` / `.rpm` | `apt install ./zuano-connector_<version>_<arch>.deb`. Creates the `zuano-connector` user, `/var/lib/zuano-connector`, and the systemd unit (not started). |
| Docker | `docker run -d --restart unless-stopped -v zuano-connector:/var/lib/zuano-connector -v /etc/zuano-connector:/etc/zuano-connector:ro -e ZUANO_CONNECTOR_ENROLL_TOKEN=nxt_... -e ZUANO_CONNECTOR_URL=https://<gateway> ghcr.io/patrikmichi/zuano-connector:<version>` |
| Kubernetes | `helm install zuano-connector deploy/helm/zuano-connector --set enrollment.url=https://<gateway> --set enrollment.token=nxt_... --set config.network=net_...` with your targets under `config.targets`. Two pods by default, one volume each, a PodDisruptionBudget, anti-affinity and probes. |

Linux and Windows get the same binary; the Windows config is
`%ProgramData%\Zuano\connector\config.yaml`. Verify releases with the
documented `cosign verify` commands: the image, the checksums and the build
provenance are all signed.

## First start

```
sudo zuano-connector enroll --token nxt_... --url https://<gateway> --config /etc/zuano-connector/config.yaml
# edit /etc/zuano-connector/config.yaml: list the targets
zuano-connector check
sudo systemctl enable --now zuano-connector
```

An administrator creates the token (`zuano networks token create <network>`).
Enrollment generates an Ed25519 signing key and an X25519 encryption key
locally; only the public halves leave. If the network requires approval, the
agent connects but receives nothing until an administrator approves it
(`zuano networks approve`).

## Commands

| Command | |
|---|---|
| `zuano-connector enroll --token --url [--name] [--config] [--state-dir]` | enroll; the token and address may come from `ZUANO_CONNECTOR_ENROLL_TOKEN` and `ZUANO_CONNECTOR_URL` |
| `zuano-connector run [--health-addr] [--config] [--state-dir]` | connect and serve; a container with a token in its environment enrolls itself on first start |
| `zuano-connector status [--json]` | state, agent, network, allowlist hash, in-flight count, last error, key age |
| `zuano-connector check [--print] [--hints]` | validate the config offline: schema, DNS, address rules, TLS handshakes, commands, secret references. `--print` shows the config with literal secrets hidden; `--hints` is the only part that contacts the gateway |
| `zuano-connector rotate` | rotate the keys now |
| `zuano-connector pause` | refuse every request with `agent_paused` while staying connected |
| `zuano-connector resume` | serve requests again |
| `zuano-connector update [--check] [--allow-downgrade]` | update this binary after verifying the signed manifest |
| `zuano-connector version` | version, protocol, commit, build date |

Exit status: 0 ok, 1 failure, 2 invalid config or usage (systemd does not
restart on it), 3 the agent was revoked (nor on this). `status`, `pause`,
`resume` and `rotate` talk to the running agent over `<state-dir>/control.sock`,
which only the agent's user can open.

## Config

`/etc/zuano-connector/config.yaml` (or `--config`, or `ZUANO_CONNECTOR_CONFIG`).
The file must not be writable by group or other. Unknown keys are errors. The
JSON Schema is published with the agent (`internal/tunnel/config.schema.json`)
for editor completion. A change is picked up on SIGHUP or when the file
changes; an invalid file leaves the running config in force.

Top level: `version` (1), `network` (the id enrollment printed; a mismatch
exits 2), `name`, `state_dir`, `max_concurrency` (32), `shutdown_grace` (30s),
`allowed_gateway_kids` (pin the signing keys), `spill_threshold` (bytes, 8 MiB)
and `targets`, the list described below.

| Section | Keys |
|---|---|
| `broker` | `url` (pins the broker), `ca_file`, `proxy` |
| `log` | `format` (json, text), `file`, `max_size_mb`, `max_files`, `upload` (off, errors, metadata: what the dashboard receives) |
| `health` | `listen` (serves /healthz, /readyz, /metrics; loopback unless you say otherwise) |
| `update` | `auto` (false), `channel`, `window` ("Sat 02:00-04:00 Europe/Prague"), `method` (binary, package), `url` |

### Targets

A target is the only way the gateway can name something: by `name`. The gateway
never sends a host, address or port.

| Key | Meaning |
|---|---|
| `name`, `kind` | `http`, `mcp_http` or `mcp_stdio` |
| `url` | scheme, host, port and base path (`http` kinds) |
| `cidrs` | every resolved address must be inside these |
| `methods`, `paths` | allowed HTTP methods and path patterns (`**` crosses segments, `*` does not) |
| `timeout` | per request, at most 300s |
| `allow_loopback`, `allow_insecure` | opt in to loopback addresses and to skipping certificate checks |
| `tls` | `ca_file`, `server_name`, `client_cert`, `client_key` (mTLS), `insecure_skip_verify` (needs `allow_insecure`) |
| `follow_same_origin_redirects` | otherwise a 3xx is returned as it is; cross-origin redirects are never followed |
| `strip_request_headers`, `strip_response_headers` | removed before sending and before returning |
| `buffer_unsafe_bodies` | verify the whole request body (up to 8 MiB) before contacting the target |
| `credentials` | held here, never sent to the gateway: `header` with `value`, or `bearer`, or `basic` with `username` and `password`; values may be `${file:/path}` or `${env:NAME}` and are read again on every use |
| `scrub_credentials_in_response` | replace the injected values with `[redacted]` in responses (on by default) |
| `rate` | `per_second` and `burst` |
| `command`, `args`, `env`, `cwd`, `env_inherit`, `idle_timeout`, `run_as_user` | an `mcp_stdio` process: started lazily, restarted with backoff, stopped when idle (15m) |
| `mcp` | `methods` (default initialize, tools/list, tools/call, ping) and `tools` (globs checked on every tools/call) |

## What is enforced where

* The agent opens each request with its own key and checks the gateway's
  signature before it contacts anything. A request that is stale (more than
  60 s from the broker's clock), repeated, for another organisation or network,
  or not sealed to this agent is refused and logged as a security event.
* Method, path (after `..`, duplicate slashes and escapes are resolved; an
  encoded slash is refused), MCP method and tool, address and redirects are all
  checked locally. A refusal is logged as a policy violation.
* The name is resolved once and the connection goes to the checked address, so
  a name that changes its answer cannot move it. Cloud-metadata and link-local
  ranges are refused unless a target lists them, loopback unless allowed.
* Credentials in `credentials` replace any header of the same name the gateway
  sent.
* A response larger than the cap the gateway set is cut with
  `response_too_large`. A large file response can be streamed straight to the
  file store (the gateway supplies a one-time upload address); a file the
  gateway points at is fetched by the agent and its SHA-256 checked first.

## Logs and metrics

One JSON line per exchange: request id, trace id, audit reference, target,
method, path, status, byte counts, durations, outcome, and whether the
credentials were the agent's. Bodies, query strings (only `query_present`) and
header values are never written.

`/metrics` exposes `zuano_connector_exchanges_total{target,outcome}`,
`..._exchange_duration_seconds`, `..._upstream_duration_seconds`,
`..._connected`, `..._reconnects_total`, `..._policy_violations_total`,
`..._security_events_total`, `..._inflight` and `..._build_info`.

## Network requirements

Outbound 443 only: the broker host, the gateway (enrollment, key rotation, key
set), the releases host if updates are on, and the file-store host if large
files are spilled. Pods and hosts need a clock within a minute of the broker's.

## Updates

Off unless both the local `update.auto` and the network's setting allow it, and
never in a container. The manifest at `<update.url>/connector/<channel>/manifest.json`
is signed with an offline key compiled into the binary; the download's size and
SHA-256 are checked, the old binary is kept as `<exe>.previous`, and a new
version that does not connect within two minutes is rolled back.

## Keys

Keys rotate before they expire (a third of their life before); the old key
still opens requests for 24 hours. A revoked agent exits with status 3.
