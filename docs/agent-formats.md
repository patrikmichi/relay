# Agent format compatibility

Verified September 9, 2026 against [Codex custom agents](https://learn.chatgpt.com/docs/agent-configuration/subagents), [Cursor subagents](https://cursor.com/docs/subagents), and [Gemini CLI subagents](https://geminicli.com/docs/core/subagents/).

All five providers support name, description and instruction body. Codex uses
TOML and stores the body in `developer_instructions`; Cursor and Gemini use YAML
frontmatter in Markdown. Only each provider's own agent directory is writable.
Codex TOML parsing and serialization use go-toml/v2, including nested tables,
dotted keys, multiline strings and escaping; unsupported fields are reported.

Codex and Cursor do not have a supported Relay tools-allowlist mapping. Agents
with tools restrictions cannot migrate into them. Gemini's tools field is an
array; same-provider nonempty allowlists are preserved. Cross-provider Gemini
tool restrictions, explicit empty allowlists, and all unbound execution settings
(including `readonly`, `mcpServers`, sandbox/approval settings, nested TOML tables)
produce a non-overridable security refusal. This applies even to same-provider
migration for unbound settings. These adapters support the reviewed common
subset, not every vendor feature. Provider runtimes have not been launched by
the automated fixtures; fixtures exercise serialization, discovery and writes.

Models are preserved within a provider. The Claude/opencode alias table is
verified against [Anthropic model IDs](https://platform.claude.com/docs/en/models/overview)
on September 9, 2026: opus → anthropic/claude-opus-5, sonnet →
anthropic/claude-sonnet-5, haiku → anthropic/claude-haiku-4-5-20251001.
Unknown cross-provider models are omitted and reported as dropped; the target
uses its default. `--strict` refuses such drops. No foreign model ID is guessed.
