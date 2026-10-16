# Plugin bundles

A bundle is skills, commands, subagents, hooks, MCP servers and rules published
once on the gateway. zuano installs it through each coding agent's own plugin
system where the agent has one, and expands what it can into the agent's
configuration where it has not. Packages come from your organisation's private
feeds.

| Command | What it does |
|---|---|
| `zuano plugin install <name>[@version] [--to AGENTS] [--channel stable\|beta] [--allow-hooks] [--dry-run] [--json]` | Verify and install a bundle |
| `zuano plugin update [name] [--to] [--channel] [--allow-hooks] [--dry-run]` | Install newer versions; remove revoked ones |
| `zuano plugin remove <name> [--to]` | Remove a bundle |
| `zuano plugin list [--available] [--channel] [--json]` | What is installed (with update and revocation status), or what the feed offers |
| `zuano plugin verify <name> [--to]` | Re-hash the files zuano wrote and report drift (exit 1) |
| `zuano plugin feeds setup [--to] [--channel]` | Register the feeds in the agents without installing anything |
| `zuano auth header [--json]`, `zuano auth git-credential get\|store\|erase` | The credential helpers the agents and git call |
| `zuano publish --type plugin <dir>` | Publish a bundle from a `plugin.yaml` directory |

`--to` takes `claude`, `codex`, `copilot` (the Copilot CLI), `vscode`, `cursor`,
`gemini-cli`, `opencode`, `cline` and `windsurf`, comma separated. Without it,
every one of those found on this machine except `vscode`, which must be named.

## Verification

Before anything is installed, zuano downloads the package from the feed (with a
short-lived feed token that is sent to the gateway origin only; a package URL on
another host is refused) and checks:

1. the archive's sha256 against the feed;
2. `SIGNATURE.json` at the archive root: schema `plugin-signature/1`, the
   plugin, version and platform it names, and its Ed25519 signature over the
   canonical JSON of the document without `signature`, against the signing keys
   pinned for your gateway (`zuano config trust-key`);
3. every file against the signed list, with no file missing and none extra;
4. the archive itself: no absolute or `..` paths, no links, no duplicates, and
   size limits.

Any failure is exit status 4 and nothing is written. This is checked on the exact
archive, so an agent's own installer (which cannot check signatures) runs only
after zuano has.

## Hooks and local commands

A bundle's hooks, hook scripts and MCP servers that run a local command run on
your machine. zuano lists them (from `hooks/hooks.json`, `.mcp.json`,
`mcp.json` and `gemini-extension.json`, plus the code of OpenCode and Cline
plugins) and installs only with `--allow-hooks`, or after a yes at a terminal.
An update asks again only if the list changed.

## How each agent is reached

Package platforms: Claude Code uses `claude`; Codex, Copilot, VS Code, Cursor
and Windsurf use `agent_plugins`; Gemini CLI `gemini`; OpenCode `opencode_npm`;
Cline `cline`.

| Agent | Install | Remove |
|---|---|---|
| Claude Code | the marketplace is added to `~/.claude/settings.json` (`extraKnownMarketplaces`, url source, `headersHelper` = `zuano auth header --json`), then `claude plugin install <name>@<marketplace>` | `claude plugin uninstall <name>@<marketplace>` |
| Codex | `codex plugin marketplace add <git mirror>`, then `codex plugin install <name>@<marketplace>` | `codex plugin uninstall <name>` |
| Copilot CLI | the same with `copilot` | `copilot plugin uninstall <name>` |
| VS Code | the git mirror is added to `chat.plugins.marketplaces` in the user settings (comments kept); VS Code installs from its plugins view | forgotten only |
| Cursor | the verified package is unpacked to `~/.cursor/plugins/local/<name>` | the files zuano wrote, if unchanged |
| Gemini CLI | `gemini extensions install <git mirror>/git/<name>.git --ref <channel>` | `gemini extensions uninstall <name>` |
| OpenCode | the feed's npm scope and a feed key in `~/.config/opencode/.npmrc`, the package added to `plugin` in `opencode.json[c]`, and the parts OpenCode's package cannot carry expanded | the config entry and the expanded parts |
| Cline | `cline plugin install <verified archive>` when `cline` is on PATH, and the parts it cannot carry expanded; otherwise expanded only | `cline plugin uninstall <name>` and the expanded parts |
| Windsurf | expanded only | the expanded parts |

The git mirror is used with git's credential helper set for the gateway's `/m/`
paths only, through environment variables on the one command, so your git
configuration is not touched (`plugin feeds setup --to vscode` sets it globally,
because VS Code runs git itself). The exact commands above are what zuano runs;
an agent version whose CLI differs needs the matching change.

### Expansion

Where an agent has no plugin system, or its package drops a part, zuano writes
that part into the agent's configuration and records it in the ledger:

- skills go through the same engine as `zuano skill install` (the agent's own
  skill directory and the skill manifest);
- commands and rules are written to the zuano sync directory
  (`~/.config/zuano/sync/commands/<plugin>--<name>.md`, `rules/...`), and MCP
  servers to its `mcp.json` as `<plugin>-<name>`, then those kinds are synced to
  that agent. A local-command MCP server needs `--allow-hooks`.
- subagents and hooks are not expanded.

Because the parts join the sync directory, a later `zuano sync` to other agents
carries them too, and `plugin remove` takes them out again. A file you changed
since zuano wrote it is kept.

## The ledger

`~/.config/zuano/plugins/ledger.json` (mode 0600, replaced atomically) records,
per plugin and agent, the version, platform, method, channel, the files written
with their sha256, expanded skills and MCP servers, the npm package, a digest
of the commands that were approved, and the gateway's install id. `zuano doctor`
reports ledger drift and unreachable feeds.

## Keys and tokens

`zuano auth header` mints a `feeds:read` token (one hour) with your session and
keeps it in the keychain until five minutes before it expires. `plugin feeds
setup --to opencode` mints a long-lived feed key named after this device, because
npm cannot call a helper; it appears under Settings, API keys, where it can be
revoked. Feed tokens cannot call tools or read anything outside `/m/<feed>/`.

## plugin.yaml

```yaml
name: toolkit
version: 1.2.0
description: Team toolkit
components:
  skills:   [{ ref: triage@1.4.0 }]
  commands: [{ file: commands/review.md }]
  rules:    [{ file: rules/style.md }]
  hooks:    [{ event: PreToolUse, matcher: Bash, command: ./hooks/check.sh }]
  mcp:      [{ ref: docs@2.0.1 }, { http: https://mcp.example.com/mcp }]
```

`zuano publish --type plugin <dir>` validates the file (names, pinned `ref`s of
the form `name@version`, files that exist inside the directory, https MCP
addresses), reads the command and rule files, and uploads the bundle. Hooks and
local MCP commands are flagged for admin review on the gateway.
