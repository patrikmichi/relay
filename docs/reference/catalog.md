# Catalog and custom connectors

`zuano catalog` shows each service once, with every source that offers it.
`zuano connectors import` and its companions turn an API description into a
connector of your own.

## Catalog

| Command | |
|---|---|
| `zuano catalog search <query> [--source native\|custom\|federated\|registry] [--cursor C]` | find a service; the sources that offer it are listed |
| `zuano catalog show <service>` | one service with its sources, tool counts and the preferred source |
| `zuano catalog federated list` | another vendor's catalogs the organisation has added |
| `zuano catalog federated show <id>` | one, with its apps |
| `zuano catalog federated add <vendor> (--credential-stdin \| --from-file F) [--per-user]` | add a vendor's catalog; the gateway verifies the credential |
| `zuano catalog federated enable <id> <app>...` | provision apps and discover their tools |
| `zuano catalog federated review <id> [--no-browser]` | open the page where the tools and their risk are accepted |
| `zuano catalog registry search <query> [--source S] [--cursor C]` | search the MCP registry |
| `zuano catalog registry show <name>` | the versions of a server, how each runs (package or remote) |
| `zuano catalog registry install <name>[@version] [--remote \| --package] [--wrap]` | install it on the gateway, or with `--wrap` print how to run it behind the firewall on this machine |

Sources: **native** (built in), **custom** (your organisation's), **federated**
(a vendor's catalog, such as Composio or Zapier, reached through the gateway) and
**registry** (the MCP registry). Bringing in tools nobody here reviewed is why
each federated app and registry server is pinned and classified by risk, and
accepted by a person in the browser with a passkey.

Credentials are JSON on standard input or in a file, never a flag, and are not
shown again. An agent session cannot add a federated catalog, review one or
install from the registry (exit 10).

### Running a registry server here

`registry install <name> --wrap` installs nothing on the gateway. It prints

```
zuano mcp wrap --upstream files -- npx -y @acme/files-mcp@1.2.0
zuano mcp install files --command npx --arg -y --arg @acme/files-mcp@1.2.0 --firewall
```

so the server runs behind the firewall, with its tools pinned. npm packages run
with `npx -y`, PyPI packages with `uvx` and OCI images with `docker run -i --rm`.
A registry is open to anyone, so a package name or version with characters a
command line should not carry is refused, not printed.

## Custom connectors

| Command | |
|---|---|
| `zuano connectors import <file\|url> (--list \| --ops a,b \| --suggest) [--format F] [--slug S] [--risk tool=class:reason] [--auth-config ac_...] [--publish]` | make a draft connector from an API description |
| `zuano connectors list` | your organisation's custom connectors |
| `zuano connectors show <slug>` | one, with its tools and their risk |
| `zuano connectors versions <slug>` | its versions |
| `zuano connectors publish <slug> [--version N]` | publish the open draft |
| `zuano connectors disable <slug> [--yes]` | stop offering its tools |
| `zuano connectors export <slug> [--out F]` | the connector as JSON; secrets are never in it |

`import` accepts OpenAPI 3, Swagger 2, a Postman collection, a GraphQL schema
(SDL or introspection) and a Google discovery document; `--format auto` finds
which. WSDL, RAML, API Blueprint, AsyncAPI and other OpenAPI versions are not
imported (exit 61); a file that is none of these is invalid (exit 60). A URL is
fetched by the gateway. Each operation gets a risk class (`read`, `write`,
`destructive`, `outward`) that a person reviews; `--risk` overrides one and
needs a reason. `--list` shows the operations and stops; `--ops` takes the ids
you name; `--suggest` takes the gateway's suggestion.

Nothing is published until `--publish` or `connectors publish`, which a person
does: an agent session is refused (exit 10, `agent_not_allowed`), and a strict
organisation asks for a passkey.

## Exit status

| Status | Meaning |
|---|---|
| 10 | denied, including an agent session on `add`, `install` and `publish` |
| 12 | an agent session's write is held for approval |
| 13 | rate limited |
| 14 | a passkey is needed |
| 30-33 | the plan does not allow it; see [billing.md](billing.md) |
| 60 | the API description is not valid |
| 61 | its format is not supported |
| 62 | the connector's slug or version is taken |
| 63 | the draft fails the quality checks |
