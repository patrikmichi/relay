# Skills from sessions

`zuano skill from-session` drafts a skill and eval cases from a conversation you
had with a coding agent or a chat app. The conversation is read and redacted on
your machine; only the redacted copy is uploaded, the gateway redacts it again,
and nothing is published until you review and submit the draft.

## Commands

| Command | Request |
|---|---|
| `zuano skill from-session [<file>\|<crn_run>\|mcp-session:<ref>\|-]` | `GET /api/skills/from-session/consent`, `POST .../captures`, `POST .../captures/{id}/extract` |
| `... --wait [--accept [--then evals\|evals-optimize]]` | `GET .../extractions/{id}`, `POST .../extractions/{id}/accept` |
| `zuano skill from-session list`, `show <capture>`, `discard <capture>` | `GET .../captures`, `GET .../captures/{id}`, `DELETE .../captures/{id}` |

Flags: `--agent auto\|claude\|codex\|gemini\|opencode\|cline\|claude-ai\|chatgpt\|markdown\|text`,
`--pick`, `--last`, `--conversation ID`, `--from-turn N`, `--to-turn M`,
`--redact-term S` (repeatable), `--redact-file PATH`, `--show-redacted`,
`--into SKILL`, `--set SET`, `--goal TEXT`, `--consent`, `--wait`, `--accept`,
`--then`, `--open`, `--json`.

## What leaves your machine

1. The source is read here: a file (a symlink or a device is refused), standard
   input, or for OpenCode the output of `opencode export <id>` (zuano runs it
   with the id as one argument, never through a shell).
2. It is normalized to one session format: turns, tool calls with short
   previews of their arguments and results, and counts. Reasoning blocks, images,
   system prompts and tool definitions are dropped; long texts are cut.
3. You may keep a range of turns (`--from-turn`, `--to-turn`); the default is the
   last 400.
4. Secrets and personal data are removed here, by the rules in
   `internal/redact/redaction-rules.json`: provider keys and tokens, private
   keys, bearer headers, values assigned to names like password or token and
   long random strings next to them, URL credentials and token parameters, email
   addresses, phone numbers, card numbers, IBANs, national ids, public IP
   addresses, home directory names, and any `--redact-term`. The same address is
   the same placeholder everywhere in a session. Argument values under a key
   that looks like a secret are replaced whatever they contain.
5. `--show-redacted` prints exactly what would be uploaded and uploads nothing,
   not even a request to the gateway.
6. Uploading asks you to accept the consent text the gateway states (without a
   terminal, `--consent`). The raw transcript is never uploaded.

A run id (`crn_...`) or `mcp-session:<ref>` names something the gateway already
holds, so the gateway reads and redacts it itself.

## Formats

| `--agent` | Read from |
|---|---|
| `claude` | `~/.claude/projects/<project>/<session>.jsonl` |
| `codex` | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` |
| `gemini` | `~/.gemini/tmp/<project>/chats/session-*.json`, saved checkpoints |
| `opencode` | `opencode export <session>` |
| `cline` | the editor's global storage, `saoudrizwan.claude-dev/tasks/<id>/api_conversation_history.json` |
| `claude-ai`, `chatgpt` | `conversations.json` of the data export; several conversations need `--conversation` or a choice |
| `markdown`, `text` | a pasted or exported chat; role markers `User:`, `Human:`, `You:`, `Assistant:`, `Claude:`, `ChatGPT:`, `AI:`, `## User`, `**User**` |

`--pick` (the default on a terminal) and `--last` look only inside each agent's
own data directory and never follow a link out of it. `zuano doctor` lists which
of those directories exist, without reading any session.

## Personal skills

A skill you add yourself is personal by default if you are a member, and
organisational if you are an administrator. A personal skill is visible to you
alone (administrators see only its name, size and date), passes the secret and
code checks but needs no review, and counts against your own limit rather than
the organisation's.

| Command | |
|---|---|
| `zuano skill publish <dir> --scope personal\|org` | publish a skill; without `--scope` the gateway picks by role and the answer shows which |
| `zuano skill list --personal [--json]` | your personal skills on the gateway, and where each stands in the organisation |
| `zuano skill promote <skill-id>` | send a copy through the organisation's publish pipeline; the personal skill stays until the organisation's version is approved |
| `zuano sync` | installs your personal skills for your agents, with the starter packs' (only your devices receive them); `--no-packs` skips both |

`--scope` is for skills; an agent, an MCP server or a plugin is always
organisational. A plan without private publishing refuses `promote` (exit 30),
and the personal limit is `limit_reached` (exit 30).

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | done |
| 2 | the source could not be read or parsed, the range selects nothing, or consent is missing without a terminal |
| 10 | refused for an agent session |
| 12 | `--wait` ran out before the draft was ready |
| 22 | the session is too large for the plan: trim it with `--from-turn` and `--to-turn` |
| 30-33 | the plan does not allow it; see [billing.md](billing.md) |

An agent session (`ZUANO_AGENT` set) cannot upload a local transcript or show an
uploaded one; it uses the gateway's `skill_from_session` tool, which asks a
person to review first.
