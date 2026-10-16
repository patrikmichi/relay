# Approvals in Slack and Teams

Your organisation can send approval requests to Slack or Microsoft Teams. A card
there never decides anything and carries no token: its **Review** button opens
the approval page, where you confirm with your passkey, in a browser or on your
phone. zuano shows what is set up and where an approval was sent; it never
decides and never edits the routes.

## Commands

| Command | Request |
|---|---|
| `zuano chat status [--json]` | `GET /api/me/chat-identities`, and for administrators `GET /api/admin/chat/installations` and `/reachability` |
| `zuano chat link slack\|teams [--no-browser]` | none: opens `/api/chat/link/{provider}/start` |
| `zuano approvals show <id>` | `GET /api/approvals/{id}`; prints where it was sent (`notified`) and how far it got |
| `zuano approvals routes list\|show <id>` | `GET /api/admin/approval-routes[/{id}]` |
| `zuano approvals routes test --service S --tool T --risk R [--source agent\|automation\|cli\|run_code] [--requester EMAIL]` | `POST /api/admin/approval-routes/test` |

`status` lists the chat apps (workspace, state), how many approvers can be
reached in chat, and your own links; a member who is not an administrator sees
only their links. `link` opens the page that proves you own both accounts: you
sign in to the chat app, then confirm with your passkey. `routes test` shows
which route a call would take and who would be asked, and sends nothing.

`zuano doctor` has an `approvals-chat` row such as "approvals reachable in: Slack
(linked), Microsoft Teams (not linked)".

## Agents

An agent session may read (`status`, `routes list|show|test`, `approvals show`).
`link` is confirmed by a person and is refused with exit 14.

The shapes are in [gateway-contracts.md](gateway-contracts.md).
