# Rclone Storage Path Reporting Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make rclonestore report canonical local persistence roots through `storage.PathReporter` without inspecting credential-bearing rclone configuration.

**Architecture:** Resolve all path candidates once in `New`: automatically derive the effective root for exact inline `:local:` remotes, union it with caller-declared roots, canonicalize through the nearest existing ancestor, then sort and deduplicate. Store the frozen slice on `blobStore`; its defensive `StoragePaths` method is promoted by `Store`.

**Tech Stack:** Go 1.25 standard library, `github.com/looprig/storage` v0.2.0, existing fake-rclone test harness, Go race detector.

---

### Task 1: Specify path reporting and local-root discovery

**Files:**
- Modify: `rclonestore_test.go`
- Modify: `blobs.go`
- Modify: `rclonestore.go`

**Step 1: Write compile-time and behavior tests**

Add compile-time assertions for both public and embedded providers:

```go
var _ storage.PathReporter = (*Store)(nil)
var _ storage.PathReporter = (*blobStore)(nil)
```

Add `TestNewStoragePaths` as a table-driven, parallel test. Each case creates the
existing fake binary, calls `New`, and compares `StoragePaths()` with the expected
canonical slice. Cover:

```go
Options{Remote: ":local:"}                         // current working directory
Options{Remote: ":local:/tmp/root"}                // absolute local root
Options{Remote: ":local:/tmp/root", Prefix: "a/b"} // effective prefixed root
Options{Remote: ":s3,provider=Minio:/bucket"}      // nil
Options{Remote: "named"}                           // nil
Options{Remote: "named", PersistencePaths: []string{declared}}
```

Use temp directories rather than hard-coded `/tmp` in actual cases. Assert nil,
not merely length zero, when no path exists.

**Step 2: Run the focused test to verify RED**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./... -run 'TestNewStoragePaths'
```

Expected: FAIL to compile because `Options.PersistencePaths` and
`StoragePaths` do not exist.

**Step 3: Add minimal discovery and reporting**

Extend `Options`:

```go
PersistencePaths []string
```

Extend `blobStore`:

```go
paths []string
```

Change `newBlobStore` to accept the resolved slice, cloning it on assignment.
Add:

```go
func (b *blobStore) StoragePaths() []string {
	return slices.Clone(b.paths)
}
```

Add `storagePaths(opts Options) ([]string, error)` and an inline-remote parser
that uses `connStringRe.FindString` to isolate the backend specification, compares
the backend name before its first comma with exact `local`, and appends
`filepath.FromSlash(opts.Prefix)` to the local path. Treat an empty local path as
`.`. Initially resolve each candidate with `filepath.Abs` and `filepath.Clean`,
then sort and compact duplicates.

Call `storagePaths` in `New` after option validation and before binary lookup,
then pass its result to `newBlobStore`. Update internal test call sites for the
constructor's added argument.

**Step 4: Run focused and full unit tests to verify GREEN**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./... -run 'TestNewStoragePaths|TestNewProbe'
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./...
```

Expected: PASS.

**Step 5: Commit**

```bash
git add rclonestore.go rclonestore_test.go blobs.go blobs_test.go
git commit -m "feat: report rclone local storage paths"
```

### Task 2: Canonicalize safely through nonexistent tails

**Files:**
- Modify: `rclonestore_test.go`
- Modify: `rclonestore.go`
- Modify: `errors.go`

**Step 1: Write canonicalization and validation tests**

Add table-driven parallel tests that create filesystem fixtures before their
subtest runs and then call `New` with the fake binary. Cover:

- an existing symlink ancestor plus a two-component nonexistent tail resolves to
  the real ancestor plus that tail;
- explicit and automatic candidates resolving to the same canonical path are
  deduplicated;
- multiple candidates are lexicographically sorted;
- an empty explicit path returns `*PersistencePathError`;
- a broken symlink returns `*PersistencePathError` and unwraps a filesystem error;
- a path traversing through a regular file returns `*PersistencePathError`;
- an input `PersistencePaths` slice mutation after `New` has no effect;
- mutation of one `StoragePaths` result has no effect on the next result.

Check that errors from automatically derived local roots never contain the full
remote string.

**Step 2: Run focused tests to verify RED**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./... -run 'TestNewStoragePathsCanonicalization|TestStoragePathsDefensiveCopy|TestNewPersistencePathErrors'
```

Expected: FAIL because simple absolute cleaning neither resolves symlinks nor
rejects invalid candidates, and `PersistencePathError` is absent.

**Step 3: Add typed errors and nearest-ancestor canonicalization**

Add to `errors.go`:

```go
type PersistencePathError struct {
	Path  string
	Rule  string
	cause error
}

func (e *PersistencePathError) Error() string { /* typed, path-specific message */ }
func (e *PersistencePathError) Unwrap() error { return e.cause }
```

Implement `canonicalizePersistencePath`:

1. Reject an empty value with `PersistencePathError`.
2. Compute the absolute clean form.
3. `os.Lstat` the whole path.
4. On `fs.ErrNotExist`, save the leaf and retry its parent.
5. On any other `Lstat` error, return `PersistencePathError`.
6. For the first existing ancestor, call `filepath.EvalSymlinks`; a broken
   symlink therefore fails rather than being treated as a nonexistent suffix.
7. Append saved suffix components in root-to-leaf order.

Canonicalize every automatic and explicit candidate with this helper before
sorting and in-place deduplication. Preserve nil when there are no candidates.

**Step 4: Run focused and full unit tests to verify GREEN**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./... -run 'TestNewStoragePaths|TestStoragePathsDefensiveCopy|TestNewPersistencePathErrors'
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./...
```

Expected: PASS.

**Step 5: Commit**

```bash
git add rclonestore.go rclonestore_test.go errors.go
git commit -m "fix: canonicalize reported storage paths"
```

### Task 3: Pin the storage contract and document the option

**Files:**
- Modify: `go.mod`
- Modify: `README.md`
- Modify: `rclonestore.go`

**Step 1: Update dependency and public documentation**

Change the required storage version:

```go
require github.com/looprig/storage v0.2.0
```

Document `PersistencePaths`, automatic inline-local discovery, named-remote
opacity, and defensive canonical results in the README. Ensure public Go comments
state that explicit paths are already-effective roots and therefore do not have
`Prefix` appended.

**Step 2: Run dependency and documentation-adjacent checks**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go mod tidy
git diff --check
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./...
```

Expected: `go.mod` requires storage v0.2.0, no unrelated module changes, and all
tests PASS.

**Step 3: Commit**

```bash
git add go.mod go.sum README.md rclonestore.go
git commit -m "docs: document local persistence paths"
```

### Task 4: Verify the complete migration

**Files:**
- Review: `docs/plans/2026-07-12-storage-path-reporting-design.md`
- Review: `docs/plans/2026-07-12-storage-path-reporting-implementation.md`
- Review: all changed source and tests

**Step 1: Run formatting and diff gates**

```bash
gofmt -w rclonestore.go rclonestore_test.go blobs.go blobs_test.go errors.go
git diff --check
```

Expected: no output from `git diff --check` and no remaining Go formatting diff.

**Step 2: Run repository gates**

```bash
GOCACHE=/private/tmp/rclonestore-gocache make check
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -tags integration -race ./...
```

Expected: formatting, vet, gosec (when installed), unit/race, and integration/race
gates PASS. A documented integration skip is acceptable only when the external
`rclone` binary is unavailable.

**Step 3: Self-review against the design**

Confirm line by line that:

- only exact inline `local` is auto-discovered;
- named remotes are not introspected;
- prefix is appended only to automatic local paths;
- explicit paths are unioned, canonicalized, sorted, and deduplicated;
- nonexistent tails work while broken symlinks fail;
- no remote/config secret enters an error;
- both provider types satisfy `storage.PathReporter`;
- all slices are defensively owned.

**Step 4: Commit any review-only corrections**

If review finds an issue, use a new RED→GREEN test cycle and commit the focused
correction. If no issue is found, do not create an empty commit.

### Task 5: Address connection-string and exact-file review findings

**Files:**
- Modify: `rclonestore.go`
- Modify: `rclonestore_test.go`
- Modify: `README.md`
- Modify: `docs/plans/2026-07-12-storage-path-reporting-design.md`

**Step 1: Write focused failing tests**

Add table cases proving that single- and double-quoted parameter values can
contain colons/commas, doubled quotes are escaped, the path after the delimiter
retains colons, and no credential marker reaches paths or errors. Add malformed
unterminated-quote cases. Add exact regular-file and symlink-to-file rejection
cases, and make symlink fixture setup skip narrowly when the platform cannot
create links.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/rclonestore-gocache go test -race ./... -run 'TestNewStoragePaths|TestNewPersistencePathErrors|TestNewOptionsValidation|TestNewOptionsErrorNoLeak'
```

Expected: quoted remotes are split at a colon inside their parameter, unterminated
quotes proceed to binary resolution, and exact files are accepted.

**Step 3: Implement the minimal fixes**

Replace connection-string regex parsing with a credential-safe scanner that
tracks single/double quote state and doubled-quote escapes. Return one parsed
remote used by validation and inline-local discovery. Require the resolved
existing candidate to be a directory regardless of whether a nonexistent suffix
was accumulated.

**Step 4: Verify GREEN and all gates**

Run the focused command, full race suite, `make check`, integration race suite,
cross-platform compile, formatting, vet, and diff checks. Commit the review fix.
