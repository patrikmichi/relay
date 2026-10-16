# Strict mode: device-bound sessions

An organisation can require short-lived CLI tokens bound to the device that
logged in (DPoP, RFC 9449). A stolen access or refresh token is then useless
without the device key. The gateway decides which organisations are strict;
relay only offers the key and proves possession.

## Login

`relay login` (browser or `--device`) makes a P-256 device key before it asks
for tokens and sends a DPoP proof with the token request, plus
`dpop_key_type=hardware|software` saying where the private key lives
(form field of `POST /api/cli/token`, JSON field of the device-token poll).

- A strict organisation answers `token_type: "DPoP"`. relay stores the key
  reference with the session (`dpop_key` in the keychain entry) and says
  whether the key is hardware-backed.
- Any other organisation answers `token_type: "Bearer"` and relay deletes the
  key again.
- An organisation that requires a hardware key answers `400
  hardware_key_required` to a software key, and relay explains that the machine
  needs a TPM 2.0. Nothing is left behind.

## The key

| Where | When |
|---|---|
| TPM 2.0 | Linux through `/dev/tpmrm0` (your user needs access, usually the `tss` group); Windows through TBS. The key is a primary key under the owner hierarchy, derived from the key id, so the private half never leaves the TPM and nothing is stored. |
| File | otherwise: a PKCS#8 key in `~/.config/relay/dpop/<id>.pem`, mode 0600 in a 0700 directory. relay refuses a key file others can read. A login that ends bound to a file key prints a warning. |

The TPM backend is written to the TPM 2.0 specification and tested against a
fake that checks the command layout; it has not run on hardware. If it fails,
relay falls back to a file key unless the organisation requires hardware.
macOS has no hardware backend yet (Secure Enclave needs a signed build); a Mac
uses the file key, and `hardware_key_required` there means the organisation
cannot be used from a Mac until that exists.

## Requests

For a bound session every request carries `Authorization: DPoP <token>` and a
`DPoP` header: a proof with `htm` (method), `htu` (URL without query), `iat`,
a random `jti` and `ath` (hash of the access token). The refresh request
carries a proof without `ath`. The gateway verifies them and keeps `jti` for
replay protection; relay never reuses one.

Strict organisations issue 10-minute access tokens. relay refreshes ahead of
expiry (30 seconds), so a command does not fail on an expired token.

A session whose key file is gone cannot be used: relay says so and asks for a
new `relay login`. `relay logout` and `relay tokens revoke` delete the key with
the session; a new login deletes the key of the session it replaces.

## Not built

- The macOS keychain access list limited to the signed `relay` binary
  (`SecAccessCreate`) needs cgo against the Security framework and a Mac to
  build and test it; relay stores sessions through the system keychain
  library, without that restriction. Linux Secret Service and Windows
  Credential Manager have no per-application isolation.
- A Secure Enclave key for the same reason.
- keylatch is an optional integration and is not used here.

## relay doctor

`relay doctor` reports `device-key` (the session's key loads and where it
lives, or that the organisation is strict and the session is not bound),
`passkeys` (how many are enrolled; informational) and `stray-session` (an
email-only keychain entry no gateway session can use; `relay login` removes
it).
