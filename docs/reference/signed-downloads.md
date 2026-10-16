# Signed downloads

A gateway can sign what it serves so zuano can check a bundle came from that
gateway and was not altered. This page is the contract zuano verifies; the
gateway implements the same one.

## Signed responses

| Route | Signed body |
|---|---|
| `GET /api/catalog/skills/{id}/download` | the tarball |
| `GET /api/catalog/resources/{id}/download` | the tarball (skills and agents) |
| `GET /api/marketplace/plugin/{id}/{rev}` | the JSON response body |

`GET /api/marketplace/manifest` and `GET /api/marketplace/managed-settings` are
generated configuration, not versioned bundles, and are not signed. `zuano sync
--marketplace` prints the managed-settings fragment before it writes it or tells
you to merge it, and says it is unsigned.

Each signed response carries:

| Header | Value |
|---|---|
| `X-Skill-Signature` | base64 ed25519 signature |
| `X-Skill-Signing-Key-Id` | `kid` of the key that signed |
| `X-Skill-Content-Sha256` | hex sha256 of the body |
| `X-Skill-Scan-Verdict` | base64 of the scan-verdict JSON object |
| `X-Skill-Version` | the version signed |
| `X-Skill-Catalog-Id` | the id signed |

## What is signed

The UTF-8 canonical JSON of `{id, sha256, verdict, version}`: keys sorted at
every depth, no whitespace.

```json
{"id":"res_1","sha256":"<hex of the body>","verdict":{"ai_scan":"pass","sast":"pass"},"version":"1.4.2"}
```

- `sha256` is the hex sha256 of the body bytes. zuano computes it itself and
  never takes it from a header.
- `verdict` is the decoded scan-verdict object, not the base64 text of the
  header.
- `id` and `version` are the `X-Skill-Catalog-Id` and `X-Skill-Version` values.
  zuano also requires `id` to be the plugin id for a plugin bundle, and the
  signed version to equal an exact `--version` that was asked for.

## Public keys

`GET /.well-known/zuano-signing-keys`, public, no credentials sent:

```json
{"keys":[{"kid":"2026-10","alg":"ed25519","publicKey":"<base64 raw 32-byte key>"}]}
```

## Trust

- `zuano login` pins the keys the gateway publishes the first time it sees
  them, per gateway origin, in `~/.config/zuano/config.json` (`signingKeys`),
  and prints their fingerprints (`SHA256:` of the raw public key).
- If the published set later differs (a key added, removed or changed), login
  warns and keeps the old pins. Downloads signed with an unpinned key are
  refused until you review the new fingerprints and run `zuano config
  trust-key`, which asks on a terminal (or `--yes`) and is refused when
  `ZUANO_AGENT` is set.
- A gateway that signs nothing (no keys published, or no signature on a
  response) is allowed with a warning. `--require-signature` on `skill
  install`, `agent install` and `sync --marketplace` refuses an unsigned
  download. A signature that is present must always verify.
