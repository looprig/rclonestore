# rclonestore

`rclonestore` implements storage's **Blobs** primitive — content-addressed immutable byte
objects — by **exec'ing the external `rclone` binary** (`rcat`/`cat`/`deletefile`/`lsf`),
giving looprig's workspace store a cloud-agnostic backend across any of rclone's many local
and cloud remotes without ever linking librclone (whose dependency tree would defeat the
point of the extraction). It drives rclone via an argv list only — never a shell string —
with `--` inserted before positional path arguments so no key can be mistaken for a flag,
every call bounded by a `context.Context`, and rclone's config (which may embed remote
credentials) referenced by path only: never parsed, copied, logged, or placed in an error.
It depends only on the Go standard library and `github.com/looprig/storage`.

## Usage

```go
s, err := rclonestore.New(rclonestore.Options{
	Remote:           "myremote",       // a named rclone remote, OR a :backend: connection string
	Prefix:           "workspaces/v1",  // optional path prefix under the remote
	PersistencePaths: []string{"/data/workspaces/v1"}, // only when a named remote persists locally
	Timeout:          30 * time.Second, // optional per-call bound
})
if err != nil {
    return err // *OptionsError | *BinaryError | *ProbeError
}
// s is a storage.Blobs: Put / Get / Delete / List.
```

`New` fails fast (fail-secure) if the configuration is invalid, the binary is missing, or the
remote is unreachable — never deferring those failures to first use. `Store` holds no
persistent connection, so its `Close` is a documented no-op.

### Options

| Field              | Meaning |
| ------------------ | ------- |
| `Remote`           | **Required.** A named rclone remote **or** a `:backend:` connection string (see below). |
| `Prefix`           | Optional path fragment under the remote. Must be a safe relative path: no leading/trailing `/`, no empty, `.` or `..` segment. |
| `PersistencePaths` | Optional local filesystem roots used by the backend. Inline `:local:` roots are discovered automatically; named remotes are never introspected. Entries are exact effective roots, so `Prefix` is not appended. |
| `Binary`           | rclone executable; default `"rclone"`, resolved via `exec.LookPath`. |
| `ConfigPath`       | Optional `--config` path. Referenced by path only — never opened, parsed, copied, or logged (it may hold remote secrets). |
| `Timeout`          | Optional bound on every rclone invocation (including the startup probe). Zero means each call is bounded only by the caller's context. |

### The two remote forms

`Remote` is validated as **exactly one** of two forms, and the object path is built
accordingly (a hardcoded colon would break the connection-string form):

- **Named remote** — a config alias matching `^[A-Za-z0-9_-]+$` (e.g. `myremote`). The object
  path joins with a colon: `myremote:` + `prefix/key` → `myremote:prefix/key`.
- **Connection string** — an inline spec beginning with `:` and a lowercase alphanumeric
  backend (e.g. `:local:/data`, `:s3,provider=Minio:/bucket`). The backend spec already ends
  with a colon, so the path is appended with a slash instead:
  `:local:/data` + `prefix/key` → `:local:/data/prefix/key`.

The parser finds the spec-ending unquoted colon. Parameter values containing colons or commas
must use rclone's single- or double-quoted form; doubling the active quote escapes it. For
example, `:local,token='https://user:pass@host':/data` still has `/data` as its local path.
Unterminated quoted values are rejected without echoing the credential-bearing remote.
Colons after the spec delimiter belong to the path. An empty `Prefix` collapses cleanly.

### Local persistence paths

`Store` implements storage's optional `PathReporter` capability. `StoragePaths` returns the
canonical local roots that hold blobs, allowing workspace owners to reject unsafe overlap
with their own directories.

Inline `:local:` remotes are detected without invoking rclone or reading configuration. Their
effective root includes `Prefix`; for example `Remote: ":local:/data"` with
`Prefix: "workspaces/v1"` reports `/data/workspaces/v1`. Other inline backends and ordinary
named remotes report no automatic path.

A named remote can itself be configured as a local backend, but rclonestore deliberately
does not inspect its config because it may contain credentials. In that case the caller must
provide the exact effective root through `PersistencePaths`. Explicit paths are unioned with
any automatically detected local root, canonicalized through the nearest existing ancestor,
sorted, and deduplicated. A directory that does not exist yet is supported; a broken symlink
or path that resolves to a regular file returns `*PersistencePathError`. Both the option slice
and every returned slice are defensively copied.

### Startup probe

`New` probes reachability with `rclone lsf --max-depth 0 -- <remote-root>`, bounded by
`Timeout`. A benign not-found on the root (a fresh store whose root directory does not exist
yet — first `Put` creates it) is treated as reachable-but-empty and succeeds, matching
`List`'s semantics. Any other failure (unknown remote, auth, network) is a `*ProbeError`
wrapping the credential-safe `*RcloneError`.

## Security posture

- **argv exec only — never a shell string.** `exec.CommandContext` with every argument as a
  discrete element; no `sh -c`, no interpolation of any external value.
- **`--` before positionals**, inserted exactly once, so no key or remote path starting with
  `-` can be parsed as a flag.
- **Every call is `context.Context`-bounded**; on timeout/cancel the subprocess is killed.
- **No secrets in errors or logs.** rclone config may embed credentials — it is referenced by
  path only. Errors carry only the rclone subcommand, safe subflags, the exit code, and a
  bounded (~4 KiB) tail of stderr; never the config path, the remote, or any positional.
  `OptionsError` names the offending field and rule but never the offending value (a
  connection-string `Remote` can embed secrets), and `ProbeError` does not carry the remote.
- **Never links librclone / cgo** — rclone is driven as a subprocess only.

## Error types

All errors are typed; classify with `errors.As`.

- `*OptionsError` — invalid `Remote`/`Prefix`/`Timeout` (from `New`, before any exec).
- `*PersistencePathError` — a declared or automatically derived local persistence root could
  not be canonicalized (wraps an underlying filesystem cause when applicable).
- `*BinaryError` — the rclone binary could not be resolved on PATH (wraps the `exec.LookPath` cause).
- `*ProbeError` — the startup reachability probe failed (wraps the underlying `*RcloneError`).
- `*RcloneError` — a failed rclone invocation (non-zero exit, start failure, or ctx kill).
- `*PutSourceError` — reading the caller's `Put` reader failed.
- storage's `*BlobNotFoundError`, `*BlobConflictError`, `*InvalidNameError` per the Blobs contract.

## Testing

```sh
GOWORK=off make check                          # gofmt + vet + gosec + race unit tests
GOWORK=off go test -tags integration -race ./... # storage Blobs conformance vs. real rclone
```

The unit tests drive a generated fake `rclone` (per-test `#!/bin/sh` script) and never touch
the network. The **conformance** suite (`//go:build integration`) runs storage's
`storetest.TestBlobs` against a **real** `rclone` using the LOCAL backend
(`:local:<temp dir>`) — no cloud credentials — and **skips** (never fails) when `rclone` is
not on PATH. It is the harness that validates the not-found exit-code classification (3/4 plus
the stderr marker), the remote-form-aware object path, and the `lsf`-on-a-file existence probe
against real rclone.

Every Go command runs with **`GOWORK=off`** so the parent `go.work` at `~/code` never captures
this module.
