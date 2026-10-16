# MCP firewall

The firewall sits between your coding agents and MCP servers. An agent sees
only the tool definitions your organisation approved. A tool that is added or
whose definition changes stays hidden until it is reviewed. Each call is
checked before it reaches the server and each result before it reaches the
agent.

## Routing servers through it

```
zuano sync --firewall            # remembered for later syncs
zuano sync --firewall=off        # servers go back as installed
zuano mcp install files --command npx --arg -y --arg @acme/files-mcp --firewall
zuano firewall import-local files    # take over a server you wrote into an agent
```

A local (stdio) server starts as `zuano mcp wrap --upstream <name> -- <command>`.
A remote server is reached through the gateway at `/api/fw/<name>/mcp`; the
gateway holds its credentials, so `--bearer-env` tokens are not sent. Sign the
gateway in to it with `zuano firewall connect <name>`.

`--move-secrets` (`--env-to-keychain` on `mcp install` and `import-local`)
stores literal environment values whose name looks like a key, token, secret or
password in the system keychain; the wrapper reads them back with
`--env-from-keychain`. The wrapper's child never sees `ZUANO_*` variables.

Servers stay in zuano's own list as installed, so turning the firewall off
needs no stored originals. Servers an agent's config holds that zuano did not
write are untouched until `firewall import-local` hands them over. Wrapping,
importing and turning the firewall on or off are refused while `ZUANO_AGENT` is
set. `zuano doctor` reports whether servers are routed.

## Offline

`zuano mcp wrap --offline` (also used when `--offline` is set on zuano itself)
needs no account. Tools wait in `zuano mcp changes` until you approve them with
`zuano mcp pins approve <server> <tool>` (or `--all`); approvals take effect in
a running wrapper. `~/.config/zuano/firewall.yaml` holds a per-server subset of
the organisation policy:

```yaml
firewall:
  upstreams:
    files:
      on_definition_change: block   # or warn: keep serving the approved definition
      inbound: withhold             # or warn
      tools:
        deny: ["delete_*"]
        classify: {sync: read}
```

Pins are in `~/.config/zuano/mcp-pins/`, the hash-chained audit log in
`~/.config/zuano/mcp-audit/<server>.jsonl`. The log holds names, decisions and
digests, never arguments or results.

## Screening

Definitions are checked for hidden instructions, references to credentials and
paths, invisible characters, cross-tool references, encoded blobs, very long
descriptions and parameters that ask for unrelated data. Results are checked
for instructions aimed at the model. With a gateway, `--screen gateway`
(default) sends arguments and results to it; `--screen local` sends only
digests and local findings. A result that cannot be checked is withheld.

## Commands

| Command | |
|---|---|
| `zuano mcp wrap [--upstream s] [--offline] [--screen local\|gateway] [--env-from-keychain A,B] -- cmd args` | run a server behind the firewall |
| `zuano mcp pins list\|approve\|reject`, `zuano mcp changes` | offline pins; `list --check` exits 43 when a tool changed |
| `zuano firewall upstreams list\|show\|add\|request` | servers the gateway knows |
| `zuano firewall changes list\|show\|decide` | `decide` opens the page where a person approves or rejects with a passkey |
| `zuano firewall connect <slug>` | opens the page that signs the gateway in to a remote server |
| `zuano firewall import-local <name>` | bring a server from an agent's config under the firewall |

## Exit status

| Code | |
|---|---|
| 10 | a call was denied |
| 12 | a call is held for approval |
| 43 | a server changed a tool it was approved with |
| 44 | the server is quarantined |

The full list is in [gateway-contracts.md](gateway-contracts.md#cli-exit-codes).
