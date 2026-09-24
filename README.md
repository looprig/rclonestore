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

The connection string is parsed with a port of **rclone's own parser** (`fs/fspath` `Parse`), so
rclonestore and rclone agree on every parameter value and on where the path starts. A value is
quoted only if its **first** byte is `'` or `"` (a quote anywhere else is literal); inside a
quoted value the quote is escaped by doubling it; an unquoted value ends at the first `,` or `:`.
Values containing colons or commas must therefore be quoted: for example,
`:local,token='https://user:pass@host':/data` has `/data` as its local path, while an unquoted
`endpoint=http://h:9` ends the value at `http` and makes `//h:9…` the path, exactly as rclone
reads it. Malformed strings (unterminated quotes, bad parameter names, text after a closing
quote) are rejected without echoing the credential-bearing remote. An empty `Prefix` collapses
cleanly.

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
and is refused too, and a foreign file whose *name contains a newline* can forge a line of
rclone's listing (a phantom key, or a legacy-shaped path and a refusal). **One-way:** ≤ v0.4.x does not understand a v0.5.0 root (its `Get` finds
nothing and its `List` reports `…@blob` names), so never roll a root back.

The scan is one recursive listing of the root at every `New` — the same cost as one `List` call.
On a large bucket that is many paged list requests (roughly one per 1,000 objects on S3) at every
construction; this is accepted, not an oversight. Open a `Store` once and reuse it. A scan failure is `*LayoutScanError` (wrapping
the `*RcloneError`), never read as clean or as legacy.

### Startup probe

`New` probes reachability with `rclone lsf --max-depth 0 -- <remote-root>`, bounded by
`Timeout` — or, when `Timeout` is zero, by a default of two minutes, because rclone retries an
unreachable endpoint for minutes. The legacy-layout scan has no default bound (it lists the whole
root); set `Timeout` to bound it and every later call. A benign not-found on the root (a fresh store whose root directory does not exist
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

**Not-found is rclone's exit code 3 or 4, and nothing else.** Stderr text is never classified:
rclone prints `Config file "…" not found - using defaults` on every call when no config file
exists, and a misconfigured endpoint can answer `404 Not Found`; reading either as "absent" would
make `Get` report a missing blob and `Put` upload over an object it failed to probe. A backend
that reported not-found some other way would fail loudly instead (both `:local:` and S3 use
3/4).

## rclone environment (deny by default)

rclone reads every global flag from an `RCLONE_<FLAG>` environment variable and every backend
option from `RCLONE_<BACKEND>_<OPTION>`. Inherited unchanged, several of these silently break the
store: `RCLONE_PROGRESS` writes transfer stats into `Get`'s bytes and defeats `Put`'s conflict
check, while `RCLONE_DRY_RUN` and `RCLONE_INTERACTIVE` make `Put` succeed having written nothing
(all measured with rclone v1.71.1). rclonestore therefore **drops every `RCLONE_*` variable from
the rclone child's environment except this allowlist** (names compared case-insensitively):

| Allowed | Why |
|---|---|
| `RCLONE_CONFIG` | config file path (`ConfigPath`, passed as `--config`, takes precedence) |
| `RCLONE_CONFIG_*` | `RCLONE_CONFIG_PASS`, `RCLONE_CONFIG_DIR`, and **environment-defined remotes** `RCLONE_CONFIG_<REMOTE>_<OPTION>` |
| `RCLONE_PASSWORD_COMMAND`, `RCLONE_ASK_PASSWORD` | decrypting an encrypted config |
| `RCLONE_CONTIMEOUT`, `RCLONE_TIMEOUT`, `RCLONE_EXPECT_CONTINUE_TIMEOUT` | timeouts |
| `RCLONE_RETRIES`, `RCLONE_RETRIES_SLEEP`, `RCLONE_LOW_LEVEL_RETRIES` | retries |
| `RCLONE_CA_CERT`, `RCLONE_CLIENT_CERT`, `RCLONE_CLIENT_KEY`, `RCLONE_CLIENT_PASS`, `RCLONE_NO_CHECK_CERTIFICATE` | TLS |
| `RCLONE_BWLIMIT`, `RCLONE_BWLIMIT_FILE`, `RCLONE_TPSLIMIT`, `RCLONE_TPSLIMIT_BURST` | rate limits |
| `RCLONE_USER_AGENT`, `RCLONE_BIND`, `RCLONE_DISABLE_HTTP2`, `RCLONE_DISABLE_HTTP_KEEP_ALIVES`, `RCLONE_DSCP` | networking |

Everything else named `RCLONE_*` is dropped. That includes the logging, progress, dry-run and
interactive flags, `RCLONE_RC*`, `RCLONE_HEADER*`, `RCLONE_METADATA_SET`, `RCLONE_DISABLE`, and
**all backend options such as `RCLONE_S3_REGION`**. Put backend options in the connection string
or the config file (or define the remote with `RCLONE_CONFIG_<REMOTE>_*`). Variables not named
`RCLONE_*`, such as `HTTPS_PROXY`, `HOME`, `PATH` and cloud SDK credentials like `AWS_*`, pass
through unchanged.

## Known limits

- **A misnamed bucket or container reads as absent.** rclone reports a missing bucket with the
  same not-found exit code as a missing object, so `Get` answers `BlobNotFoundError`, `List` is
  empty, and the first `Put` creates the bucket. Check the remote name when a store looks
  unexpectedly empty.
- **Quote any inline value that contains `:` or `,`.** rclone ends an unquoted value at the first
  `:`. An unquoted secret containing `:` is split: the part after the colon becomes the remote
  **path**, which rclone echoes and rclonestore does not redact, because a path is not a secret.
- `Get` of a foreign *directory* at an object's location, newline-named foreign files and the
  cost of `New`'s scan are described in the sections above.

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
  the stderr tail is **redacted before it is kept**: `:s3,access_key_id=…,secret_access_key=…:bkt`
  becomes `:s3,<redacted>:bkt`, and every inline parameter value and the config path become
  `<redacted>` wherever they appear (a value that is also ordinary text is over-redacted). The
  masking marks the original bytes, so a secret cut by the tail bound leaves no fragment.
  - **Exact spellings only.** Redaction matches a value verbatim, as rclone unquotes it, and
    Go-escaped (`%q`). A value rclone or an SDK *transforms* — URL-encoded (a SAS or endpoint
    userinfo inside a request URL), reordered query parameters, case-folded — is not matched.
  - **The rclone child environment is deny-by-default** (see "rclone environment" above), so
    the log grammar is pinned: argv flags cannot override rclone's logging variables.
  - **Not covered:** an inline connection string is in rclone's **argv**,
    so any local user who can list processes (`ps`, `/proc/<pid>/cmdline`) can read it while a
    call runs. **Prefer a named remote with its secrets in the config file.**
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
