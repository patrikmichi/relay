# Agent-to-agent

Agents call other agents through the gateway. It checks who may call whom,
records every delegation as a chain, applies approvals to every effect in the
chain, and can stop the whole chain. `zuano a2a` is the command-line side.

| Command | |
|---|---|
| `zuano a2a agents list [--kind K] [--external]` | the agents you may call, with the state of each card |
| `zuano a2a agents show <slug>` | an agent, its endpoint and the skills its pinned card describes |
| `zuano a2a agents register --card-url <url> [--auth none\|stored\|per-user] [--internal]` | administrators: register an agent from its card; prints the review link |
| `zuano a2a agents changes <slug>` | changes to a card that wait for review |
| `zuano a2a agents review <slug>` | opens the page where a card or a change is approved with a passkey |
| `zuano a2a card <slug> [--live]` | the pinned card (the one an administrator approved), or the card the agent publishes now |
| `zuano a2a call <slug> --skill <id> --message <text\|@file> [--file param=@path] [--context id] [--wait s] [--stream]` | send a message through the gateway |
| `zuano a2a chains list [--mine] [--status S] [--agent A]` | chains, newest first |
| `zuano a2a chains show <id>` | a chain as a tree: hops, effects, budget and the audit check |
| `zuano a2a chains kill <id> --reason <text>` | stop a chain |
| `zuano a2a graph [--format dot\|json] [--from F] [--to T] [--window 30d]` | the agent graph, for Graphviz or as JSON |
| `zuano a2a explain --from <identity> --to <agent> --skill <id>` | which rule decides, with its file and line |

## Pinned cards

An agent's card is pinned when an administrator approves it. Callers and models
see only the pinned text, never what the agent publishes now; a change waits in
`agents changes` and is approved in the browser. A card from an outside party
can contain anything, so everything printed from one has control,
bidirectional-override and zero-width characters removed.

## Calling

`a2a call` sends a message and prints the task. `--wait N` waits up to N
seconds for the task to finish and exits 17 if it has not; `--stream` follows
it as it runs. A hop or an effect further down the chain that needs an approval
holds the task: the command prints the approval page link and exits 12. Decide
it in the browser, then run the call again or collect results with
`zuano approvals wait`. `--file param=@path` uploads the file and passes it by
reference.

`ZUANO_AGENT=1` sessions are refused on `register` (exit 10) and on `review`
(exit 14); they may stop a chain.

## Exit status

| Code | Meaning |
|---|---|
| 10 | denied: the edge, DLP, an unpinned agent or an approval decision |
| 11 | an approval expired |
| 12 | held for an approval |
| 13 | rate limited |
| 17 | `--wait` ended first |
| 31 | a plan quota or spend cap |
| 33 | an identity in the chain is suspended or revoked |
| 50 | loop, depth or chain limit |
| 51 | the chain was killed |
| 52 | the chain's budget is used up |
| 53 | the agent's card changed and is quarantined |

The full list is in [gateway-contracts.md](gateway-contracts.md#cli-exit-codes).
