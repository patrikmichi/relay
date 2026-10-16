# Sites (zuano drop)

A site is a file or folder served on its own address on the gateway's isolated
user-content domain, with immutable releases. Publishing uploads a release and
makes it live; rollback makes an earlier release live again. A site is visible
to the people, groups or organisation you share it with, and to anyone only if
you make it public.

| Command | What it does |
|---|---|
| `zuano drop publish <path> [--site S] [--title T] [--entry P] [--spa] [--no-activate] [--json]` | Upload a file or folder as a release, make it live and print the address |
| `zuano drop list [--json]` | Sites you can see |
| `zuano drop releases <site>` | A site's releases; the live one is marked |
| `zuano drop rollback <site> <release>` | Make a retained release live |
| `zuano drop share <site> [--add EMAIL] [--group G] [--org] [--role viewer\|editor] [--public [--expires 7d]] [--remove X] [--private] [--yes]` | Show or change who can see the site |
| `zuano drop open <site> [--preview <release>] [--no-browser]` | Open the site, or a release's private preview |
| `zuano drop delete <site> [--yes]` | Delete a site (restorable for a while) |
| `zuano drop restore <site>` | Bring a deleted site back, private |

`<site>` is the site name (slug) or its `ast_...` id.

## Publishing

The path is canonicalized the way the gateway does it: NFC, forward slashes, no
`.`/`..`/empty components, no control characters or backslashes, no drive
prefix, nothing under `_drop/` or `api/`, at most 240 bytes and 32 components,
and no two paths that are equal ignoring case. The vectors both sides must pass
are in `internal/drop/testdata/paths.json`.

Left out with a warning: symlinks (never followed), `node_modules/`, OS
metadata (`.DS_Store`, `Thumbs.db`, `__MACOSX/`), and whatever a `.dropignore`
lists (one pattern per line; `#` comments; a trailing `/` matches folders; a
pattern with `/` matches from the folder root). A file that looks like a
credential stops the publish and is named: `.env*`, private keys (`id_rsa*`
and friends), `*.pem`/`*.p12`/`*.pfx`/`*.keystore`, `.npmrc`, `.netrc`,
`credentials*`, and anything inside `.git/`, `.ssh/`, `.aws/` or `.gnupg/`.
List them in `.dropignore` to leave them out deliberately.

Steps: `POST /api/artifacts/uploads` with the manifest (path, size, sha256 per
file); a `PUT` of each file the gateway does not already hold, four at a time,
straight to the signed address (no gateway credential is sent there, one retry
on a network failure); `POST .../finalize`; then polling the release until it
is `ready` or `failed` (five minutes at most). The site name defaults to the
file or folder name (lower case, hyphens, 3 to 30 characters).

A failed release prints the gateway's reason. `entry_required` lists the pages
that could be the entry; choose one with `--entry`.

## Sharing

`share` with no change prints the grants. With a change it asks the gateway for
a proposal (`mode=propose`), prints it (`+ person (role)`, `- group x`,
`Public access: off -> on`), asks `Apply this? [y/N]` and then applies it
(`mode=apply`, with the revision the proposal returned, so a concurrent change
is refused). Without a terminal, or with `--json`, `--yes` is required.
`--remove` takes an email, `group:NAME`, `org` or `public`; `--private` keeps
only the owner. `--expires` takes `7d`, `12h`, `30m`, `2w`, a date or a
timestamp.

## Agent sessions

In an agent session the gateway may hold publish, public sharing and delete for
approval. zuano prints the held call and exits 12, like `zuano call` does.

The routes are listed in [gateway-contracts.md](gateway-contracts.md#artifact-pages).
