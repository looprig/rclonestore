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
    return err // *OptionsError | *BinaryError | *ProbeError | *LegacyLayoutError | *LayoutScanError
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

### Object layout (v0.5.0) — BREAKING, no migration

Each key is stored at `<prefix>/<key>@blob`: key `sessions/a` is the object `sessions/a@blob`,
key `sessions/a/b` is `sessions/a/b@blob`. Storage names allow only `[a-z0-9][a-z0-9_.-]*` per
segment, so `@` never appears in a directory component and always appears in a leaf. A key and
a key extending it with `/…` therefore never contend for one location, on filesystem-shaped
remotes (`:local:`, SFTP, …) as well as bucket-based ones — the storage v0.7.0 rule that they are
distinct names, both storable whichever is written first. `List` decodes the leaves back to keys
and skips anything that is not an `@blob` leaf (rclone `.partial` uploads, dotfiles, uppercase
names). The suffix counts against a filesystem's 255-byte file-name limit, so a key's final
segment may be at most 250 bytes on such a remote.

rclonestore ≤ v0.4.x stored a key at its bare path. **That layout is not migrated and not
read.** `New` refuses a store root holding any object whose whole root-relative path is a valid
storage name (which is exactly what ≤ v0.4.x wrote, and never what v0.5.0 writes) with
`*LegacyLayoutError` (`errors.Is(err, ErrLegacyLayout)`; `Path` names the lexically first such
object). `List` fails closed the same way if one appears later, e.g. from a rolled-back binary,
instead of silently omitting it. Move or delete the old root. The root must be dedicated to
rclonestore: a foreign object spelled like a storage name is indistinguishable from legacy data
and is refused too. **One-way:** ≤ v0.4.x does not understand a v0.5.0 root (its `Get` finds
nothing and its `List` reports `…@blob` names), so never roll a root back.

The scan is one recursive listing of the root at every `New` — the same cost as one `List` call.
On a large bucket that is many paged list requests (roughly one per 1,000 objects on S3) at every
construction; this is accepted, not an oversight. Open a `Store` once and reuse it. A scan failure is `*LayoutScanError` (wrapping
the `*RcloneError`), never read as clean or as legacy.

### Startup probe

`New` probes reachability with `rclone lsf --max-depth 0 -- <remote-root>`, bounded by
`Timeout`. A benign not-found on the root (a fresh store whose root directory does not exist
yet — first `Put` creates it) is treated as reachable-but-empty and succeeds, matching
`List`'s semantics. Any other failure (unknown remote, auth, network) is a `*ProbeError`
wrapping the credential-safe `*RcloneError`. The legacy-layout scan described above runs only
after the probe succeeds.

### Existence and empty objects

`Put` probes for an existing object with `rclone lsf --files-only -- <object>` and treats it as
present only when rclone prints exactly that object's leaf name. A directory at the object's
location lists its children instead, so it is never taken for the blob and never reported as a
`*BlobConflictError`; `rcat` then fails with the backend's own error on a filesystem remote.
A bucket-based remote cats an absent key as nothing with exit 0, so `Get` confirms an empty
result with the same probe and reports `*BlobNotFoundError` unless the object exists.

**Known limit:** `Get` of a key whose `<key>@blob` location is a *directory* (only possible if
something other than rclonestore wrote into the root) returns the concatenation of that
directory's files, because `rclone cat` of a directory does so. Confirming every read would
double `Get`'s cost, so it is not done; keep the root dedicated.

rclone prints `Config file "…" not found - using defaults` on every call when no config file
exists. That line is never taken as an object not-found; only exit codes 3/4 or a "not found" on
any other stderr line are.

## Security posture

- **argv exec only — never a shell string.** `exec.CommandContext` with every argument as a
  discrete element; no `sh -c`, no interpolation of any external value.
- **`--` before positionals**, inserted exactly once, so no key or remote path starting with
  `-` can be parsed as a flag.
- **Every call is `context.Context`-bounded**; on timeout/cancel the subprocess is killed.
- **No secrets in errors or logs.** rclone config may embed credentials — it is referenced by
  path only. Errors carry only the rclone subcommand, safe subflags, the exit code, and a
  bounded (~4 KiB) tail of stderr; never the config path, the remote, or any positional.
  **rclone's own diagnostics do echo the remote**, inline `key=value` credentials included, so
  the stderr tail is **redacted before capture**: `:s3,access_key_id=…,secret_access_key=…:bkt`
  becomes `:s3,<redacted>:bkt`, every inline parameter value and the config path become
  `<redacted>` wherever they appear (a value that is also ordinary text is over-redacted), and a
  secret cut by the tail bound is dropped. Credentials passed through `RCLONE_*` environment
  variables are invisible to rclonestore and are not redacted — prefer a named remote with its
  secrets in the config file.
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
- `*LegacyLayoutError` — the store root holds rclonestore ≤ v0.4.x (bare-path) objects; matches
  `ErrLegacyLayout`. Returned by `New`, and by `List` if such an object appears later.
- `*LayoutScanError` — `New`'s legacy-layout scan could not list the root (wraps the `*RcloneError`).
- `*RcloneError` — a failed rclone invocation (non-zero exit, start failure, or ctx kill).
- `*PutSourceError` — reading the caller's `Put` reader failed.
- storage's `*BlobNotFoundError`, `*BlobConflictError`, `*InvalidNameError` per the Blobs contract.

## Testing

```sh
GOWORK=off make check                          # gofmt + vet + gosec + race unit tests
GOWORK=off go test -tags integration -race ./... # storage Blobs conformance vs. real rclone
RCLONESTORE_CONFORMANCE_REMOTE=':s3,provider=Other,endpoint=…:bucket' \
  GOWORK=off go test -tags integration -race -run ConformanceRemote ./... # …and vs. another remote
```

The unit tests drive a generated fake `rclone` (per-test `#!/bin/sh` script) and never touch
the network. The **conformance** suite (`//go:build integration`) runs storage's
`storetest.TestBlobs` against a **real** `rclone` using the LOCAL backend
(`:local:<temp dir>`) — no cloud credentials — and **skips** (never fails) when `rclone` is
not on PATH. It is the harness that validates the not-found exit-code classification (3/4 plus
the stderr marker), the remote-form-aware object path, and the `lsf`-on-a-file existence probe
against real rclone, together with the storage v0.7.0 nested-key cases, the legacy-layout
refusal and the directory-is-not-a-conflict case on a real `:local:` root. Setting
`RCLONESTORE_CONFORMANCE_REMOTE` to any remote (for example an S3-compatible bucket) runs the same
suite there, one fresh prefix per backend instance.

Every Go command runs with **`GOWORK=off`** so the parent `go.work` at `~/code` never captures
this module.
