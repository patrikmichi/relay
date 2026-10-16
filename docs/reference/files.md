# Files

The gateway keeps a per-organisation file store so a file moves into and out
of a tool by reference instead of inside the request body (which is limited to
a few megabytes). Files expire; `zuano files list` shows when.

| Command | What it does |
|---|---|
| `zuano files upload <path>... [--name N] [--share-org] [--no-wait] [--json]` | Upload files and print their ids |
| `zuano files download <fil_...\|file:fil_...\|resource URI> [-o PATH] [--force] [--json]` | Save a file |
| `zuano files list [--state S] [--created-via V] [--json]` | List files, newest first |
| `zuano files rm <fil_...>...` | Delete files |
| `zuano call <service> <tool> --file param=@path` | Upload a file and pass it as the argument `param` |
| `zuano call ... --save-files DIR` | Save the files a tool result refers to |

## How an upload works

1. zuano hashes the file (sha256) and asks the gateway for an upload target
   (`POST /api/files`).
2. It sends the bytes with a plain `PUT` to the signed address the gateway
   returned, with the headers the gateway listed. The gateway's credential is
   never sent to the object store, and redirects from it are not followed. The
   address must be `https` (plain `http` only to a loopback host). A network
   failure is retried once; a refusal by the store is not.
3. It tells the gateway the upload is complete (`POST /api/files/{id}/complete`).
4. Unless `--no-wait`, it polls `GET /api/files/{id}` until the file is
   `ready`. A `quarantined`, `failed` or `deleted` file is an error, and so is
   one that is still being checked after two minutes (check it later with
   `files list`).

Progress is shown on a terminal only. `zuano call --file` waits for `ready`
before it calls the tool, because a tool only accepts a file the gateway has
checked. The argument is sent as the string `file:fil_...`; naming the same
argument with `--arg` is an error.

## How a download works

`GET /api/files/{id}/download` answers with a redirect to a signed address (or
the bytes). zuano does not follow the redirect with its credential: it fetches
the signed address without one. The file is written to a temporary file in the
destination directory and renamed into place only after its size and sha256
match the gateway's record, so a failed download leaves nothing behind. An
existing file is replaced only with `--force`. The name the gateway supplies is
cut to its last path element and never points outside the target directory.

## Results by reference

A tool result may carry `resource_link` content blocks and
`structuredContent.files[]`. zuano prints each link as
`File: name uri (type, size bytes)`. With `--save-files DIR` it downloads every
`structuredContent.files[].file_id` into `DIR` (created with mode 0700) after
the result is printed; a file that cannot be saved is reported and the command
exits 1 even though the call itself succeeded.
