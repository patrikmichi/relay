# Step-up confirmations

Some gateway actions need a person to confirm them with a passkey before they
run, even though the CLI session is signed in: connecting an account,
publishing or promoting, changing a policy, and a destructive call from a
non-browser credential. Organisation admin actions and, in a strict
organisation, more of them, ask the same way. The gateway decides which; relay
only completes the confirmation.

## The flow

1. The request is refused with `401` and a body naming the confirmation:

   ```json
   {"error":"step_up_required","step_up_id":"su_8f2c","step_up_url":"https://gateway.example/stepup/su_8f2c"}
   ```

2. relay opens `step_up_url` in the browser. With no browser, it prints the
   link and a QR code to stderr so a phone with the person's passkey can open
   it. The page shows the action and its details and asks for the passkey.
3. relay polls `GET /api/stepup/{step_up_id}` every two seconds, for at most
   five minutes. The response is `{"status":"pending|approved|denied|expired"}`.
4. On `approved` relay sends the original request again, unchanged, with
   `X-Step-Up: <step_up_id>`. The gateway accepts the id once, for that
   request's digest only. A second `step_up_required` is returned as an
   error; relay never loops.

Only the request that was refused is retried. A `denied` or `expired`
confirmation, and a timeout, end the command with exit status 1 and nothing
done.

## Without a person at the keyboard

When `RELAY_AGENT` is set or stdin is not a terminal, relay does not open
anything and does not poll. The command ends with exit status **14** and
prints the link for a human to open:

```
this action needs a passkey confirmation from a person: open https://gateway.example/stepup/su_8f2c
```

Run the command again after the person has confirmed; a confirmation is bound
to the action it was issued for and is used up by the retry, so the second
run asks again unless the gateway's freshness window covers it.

## The link

relay opens the link only when it is a page on the gateway that refused the
request: same scheme and host, no user information. Any other link is refused
with an error and nothing is opened or polled, because the page asks for a
passkey and a look-alike page must not be trusted by a link a response
supplied.

## Exit status

| Status | Meaning |
|---|---|
| 14 | a step-up confirmation is needed and nobody could give it (agent session, no terminal) |

Every command inherits this: the mapping is in the gateway client, not in the
commands. The other exit statuses are in
[approvals.md](approvals.md#exit-statuses) and [billing.md](billing.md#exit-statuses).
