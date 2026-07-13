# Rclone Storage Path Reporting Design

**Date:** 2026-07-12

**Status:** Approved

## Context

`storage` v0.2.0 adds the optional `storage.PathReporter` capability:

```go
type PathReporter interface {
	StoragePaths() []string
}
```

Higher-level stores use this capability to reject unsafe overlap between their
workspace and persistence directories. `rclonestore` can normally treat its
backend as remote, but an inline `:local:` remote persists directly on the same
filesystem. Operators can also configure a named rclone remote that ultimately
maps to local storage.

The adapter must report local persistence roots without inspecting rclone
configuration or command output. Those inputs may contain credentials and the
package's security contract explicitly forbids opening, parsing, copying, or
logging configuration.

## Goals

- Implement `storage.PathReporter` on both `*Store` and its embedded blob
  provider.
- Automatically report the effective filesystem root of inline `:local:`
  remotes, including `Options.Prefix`.
- Let callers explicitly declare local filesystem roots for named or other
  remotes with `Options.PersistencePaths`.
- Return stable, canonical, sorted, duplicate-free paths through defensive
  copies.
- Preserve credential-safety and the existing subprocess boundary.

## Non-goals

- Inspecting rclone config files, environment, remote metadata, or subprocess
  output to discover whether a named remote is local.
- Reporting cache, temporary, or configuration paths that do not hold the blob
  data itself.
- Changing blob commands or storage semantics.

## Public API

`Options` gains:

```go
// PersistencePaths declares local filesystem roots used by the configured
// backend. It is normally unnecessary; inline :local: remotes are discovered
// automatically. Named remotes are never introspected, so callers must declare
// their local roots here when applicable.
PersistencePaths []string
```

`Store` and `blobStore` expose:

```go
func (b *blobStore) StoragePaths() []string
```

Because `Store` embeds `*blobStore`, the method is promoted to `*Store`. Compile-
time assertions cover both types.

## Path discovery

### Inline local remotes

The connection string is scanned for the unquoted colon terminating its backend
specification. Single- and double-quoted parameter values may contain colons and
commas; doubling the active quote escapes it. Unterminated quoted values are
rejected with a credential-safe `OptionsError`. The scanner returns one parsed
representation used by validation and path discovery, so backend parameters are
discarded before filesystem handling. The backend name is the portion before the
first comma; only the exact backend `local` is filesystem-backed. Colons after
the spec delimiter remain part of the path.

For `:local:/data` with prefix `blobs/v1`, the reported candidate is
`/data/blobs/v1`. Rclone paths use slash separators, so the remote path and
prefix are converted with `filepath.FromSlash` before joining.

An empty local remote path (`:local:`) denotes the process working directory.
The effective root is therefore the current working directory plus the optional
prefix. It is resolved once during `New`, not on every `StoragePaths` call.

Connection strings for S3 and every backend other than exact `local` add no
automatic path.

### Explicit declarations

Every non-empty `Options.PersistencePaths` entry is added as a candidate. These
paths describe the actual blob root, so `Prefix` is not appended to them. This
allows a caller using a named local remote to declare the exact effective root
without exposing or parsing rclone configuration.

Explicit and automatically discovered candidates are unioned, canonicalized,
lexicographically sorted, and deduplicated.

## Canonicalization

Each candidate becomes an absolute clean path. `filepath.EvalSymlinks` then
resolves the longest existing ancestor; any nonexistent suffix is appended back
unchanged. This supports a fresh store whose directory will be created by the
first `Put`, while still making aliases through existing symlinked parents
comparable.

The walk stops at the first existing ancestor. That ancestor must be a directory
even when it is the exact candidate; an exact regular file or symlink to a file
is rejected. Errors other than not-existence, including a broken symlink or a
non-directory ancestor, fail construction. Empty explicit declarations fail
rather than silently disappearing. Failures return a typed
`*PersistencePathError` that carries the affected explicit filesystem path and
unwraps the underlying cause. Automatically derived local paths are reported
without echoing the credential-bearing `Remote` value.

Paths are frozen before binary resolution and the startup probe. The internal
slice is owned by the store, and `StoragePaths` always returns a clone so callers
cannot mutate future results.

## Error handling and security

```go
type PersistencePathError struct {
	Path  string
	cause error
}
```

The error follows repository conventions with `Error` and `Unwrap`. Explicit
filesystem paths are operator-supplied and safe to identify. For derived paths,
only the parsed local filesystem portion is carried; the full connection string
is never included. Named remotes remain opaque.

No new process invocation is needed. No shell is introduced, no config is read,
and no remote or positional argument is logged.

## Testing

Table-driven parallel tests use the existing fake rclone binary and cover:

- compile-time `storage.PathReporter` satisfaction for `Store` and `blobStore`;
- inline local roots with and without a prefix;
- quoted inline parameters containing colons, commas, doubled quote escapes, and
  credentials that never enter paths or errors;
- unterminated quoted parameters returning credential-safe `OptionsError`;
- `:local:` resolving from the current working directory;
- non-local inline and named remotes reporting no automatic path;
- named remotes with explicit paths;
- union, canonical sort, and deduplication;
- symlinked existing ancestors with nonexistent tails;
- exact regular files and symlinks to files being rejected;
- empty, broken, and otherwise invalid declared paths returning the typed error;
- mutation of both input options and returned slices not affecting stored paths.

The dependency is bumped to `github.com/looprig/storage v0.2.0`. Existing unit,
race, vet, formatting, security, and integration gates remain authoritative.
