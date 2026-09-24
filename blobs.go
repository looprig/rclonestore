package rclonestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/looprig/storage"
)

// rclone's documented exit-code table (https://rclone.org/docs/#list-of-exit-codes)
// distinguishes a missing directory from a missing file. Both mean "the object the
// caller asked for is not there" for our purposes: an absent blob for Get/Delete,
// an empty listing for List, a not-present object for Put's existence probe.
const (
	exitDirNotFound  = 3 // rclone: "directory not found"
	exitFileNotFound = 4 // rclone: "object/file not found"
)

// notFoundMarker is the case-insensitive stderr substring used as a secondary
// not-found signal, in case a remote surfaces a not-found through a different exit
// code than 3/4 (some backends historically returned 1). Matching a stable phrase
// rather than a full-string equality keeps the classifier robust to diagnostic
// wording that varies by rclone version and backend.
const notFoundMarker = "not found"

// blobLeafSuffix is appended to every key to form its object's leaf name. The
// storage grammar allows only [a-z0-9][a-z0-9_.-]* per segment, so '@' never
// appears in a directory component (always a valid segment) and always appears in
// a leaf name (never a valid segment): a key "k" (object "k@blob") and a key
// "k/x" (object "k/x@blob", under directory "k") can never contend for one
// location on a filesystem-shaped remote, whichever is written first — the
// storage v0.7.0 rule that a key and its "/…" extension are distinct names.
// Stripping exactly one trailing suffix is the exact inverse, so the encoding is
// injective. A dotted suffix (".blob") would not be: key "a" would own file
// "a.blob" while key "a.blob/b" needs a directory "a.blob". '@' is outside every
// rclone backend's default encoding set, and valid names are lowercase ASCII, so
// case-insensitive remotes cannot fold two encodings together.
//
// rclonestore <= v0.4.x stored a key at its bare path. That layout is not
// migrated: New refuses such a root with *LegacyLayoutError (see
// refuseLegacyLayout), and List fails closed on a bare-path object it meets.
const blobLeafSuffix = "@blob"

// encodeLeaf maps a validated storage key to its store-relative object path.
func encodeLeaf(key string) string { return key + blobLeafSuffix }

// decodeLeaf is encodeLeaf's inverse over listing output: rel is a key's object
// only if it ends in the suffix AND the stripped remainder is a valid storage
// name. Anything else — rclone's ".partial" in-flight uploads, a doubled or bare
// suffix, a directory carrying '@', uppercase or dotfile names, other tools'
// objects — is not a key and is skipped by List.
func decodeLeaf(rel string) (string, bool) {
	key, ok := strings.CutSuffix(rel, blobLeafSuffix)
	if !ok || storage.ValidateName(key) != nil {
		return "", false
	}
	return key, true
}

// isLegacyLeaf reports whether a store-relative object path is one rclonestore
// <= v0.4.x would have written: its whole path is a valid storage name (the key
// itself). No path this layout writes is one — every leaf ends in "@blob" — so the
// test is exact for rclonestore's own output. Foreign objects that happen to be
// spelled as a valid name are indistinguishable from legacy ones and are refused
// too: the store root must be dedicated to rclonestore.
func isLegacyLeaf(rel string) bool { return storage.ValidateName(rel) == nil }

// blobStore implements storage.Blobs by driving the rclone binary through a
// runner. Each object's rclone path is built from the remote, the store prefix,
// and the per-call validated key by remoteJoin, which is aware of both remote
// forms — a named remote ("R:<path>") vs. a ":backend:" connection string
// ("<remote>/<path>"). remote and prefix are fixed at construction; the key is a
// validated storage name supplied per call. The store is pure mechanism over
// rclone — the workspace store above it owns the single-writer lease that makes
// the Put existence-then-write sequence sound.
type blobStore struct {
	r          *runner
	remote     string
	prefix     string
	connString bool // remote is a ":backend:" connection string (leading ':'), not a named remote
	paths      []string
}

var _ storage.Blobs = (*blobStore)(nil)
var _ storage.PathReporter = (*blobStore)(nil)

// newBlobStore constructs a blobStore over an already-configured runner. Binary
// resolution, option validation, and the remote-reachability probe live in New;
// this constructor takes the already-resolved remote and prefix directly and
// derives the remote form (named vs. connection string) from the remote's leading
// byte.
func newBlobStore(r *runner, remote, prefix string, paths []string) *blobStore {
	return &blobStore{
		r:          r,
		remote:     remote,
		prefix:     prefix,
		connString: strings.HasPrefix(remote, ":"),
		paths:      append([]string(nil), paths...),
	}
}

// StoragePaths returns the canonical local filesystem roots used by this blob
// provider. Remote backends return nil. The result is a defensive copy so callers
// cannot mutate the provider's construction-time view.
func (b *blobStore) StoragePaths() []string {
	return append([]string(nil), b.paths...)
}

// PutSourceError reports that reading the caller-supplied Put reader failed before
// the blob could be compared against an existing object or streamed to rclone. It
// carries the storage key (safe to log — it is a validated canonical name) and
// wraps the underlying read error.
type PutSourceError struct {
	Key   string
	cause error
}

func (e *PutSourceError) Error() string {
	return "rclonestore: reading Put source for blob " + strconv.Quote(e.Key) + " failed"
}

func (e *PutSourceError) Unwrap() error { return e.cause }

// Put honors storage's content-addressed conflict contract. It first probes for
// an existing object at the key's encoded path ("<key>@blob"; see exists — a
// directory there is never taken for the blob):
//
//   - ABSENT (the common content-addressed case): stream r straight to
//     "rclone rcat" with no buffering — the blob never lands in memory.
//   - PRESENT: read r fully, "rclone cat" the existing object, and compare bytes.
//     Byte-identical → success/no-op (the object is NOT re-uploaded). Different →
//     *storage.BlobConflictError with the original left untouched (no upload).
//
// The present branch buffers both the incoming reader (io.ReadAll) and the existing
// object (into memory) to compare them. This is deliberately the rare path: keys
// are content-addressed, so a re-Put of DIFFERENT bytes under the same key is
// pathological, and a re-Put of IDENTICAL bytes is a cheap idempotent no-op. The
// probe→write sequence is a TOCTOU only under concurrent writers to the same key;
// the workspace store holds a single-writer lease over the key space, so no
// concurrent writer exists by construction.
func (b *blobStore) Put(ctx context.Context, key string, r io.Reader) error {
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	path := b.objectPath(key)

	present, err := b.exists(ctx, path, encodeLeaf(lastSegment(key)))
	if err != nil {
		return err
	}
	if !present {
		return b.rcat(ctx, path, r)
	}

	incoming, err := io.ReadAll(r)
	if err != nil {
		return &PutSourceError{Key: key, cause: err}
	}
	var existing bytes.Buffer
	if err := b.r.run(ctx, "cat", nil, []string{path}, nil, &existing); err != nil {
		// Under the single-writer lease the object cannot vanish between the probe
		// and this cat, so a not-found here is not expected; propagate every cat
		// failure as-is rather than silently re-interpreting it.
		return err
	}
	if bytes.Equal(existing.Bytes(), incoming) {
		return nil
	}
	return &storage.BlobConflictError{Key: key}
}

// Get streams the object at key back to the caller. It runs "rclone cat" to
// completion into an in-memory buffer and returns an independent io.ReadCloser over
// those bytes. Buffering (rather than piping rclone's stdout through) is what lets
// Get satisfy the contract's synchronous not-found: storetest expects Get itself —
// not a later Read — to return *storage.BlobNotFoundError, which is only knowable
// once rclone has exited. A missing object is classified from rclone's exit
// code/stderr and mapped to *storage.BlobNotFoundError. Empty output is confirmed
// with the exact-leaf existence probe before it is returned as an empty blob,
// because a bucket-based remote cats an absent key as nothing with exit 0.
func (b *blobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := storage.ValidateName(key); err != nil {
		return nil, err
	}
	path := b.objectPath(key)

	var buf bytes.Buffer
	if err := b.r.run(ctx, "cat", nil, []string{path}, nil, &buf); err != nil {
		if isNotFound(err) {
			return nil, &storage.BlobNotFoundError{Key: key}
		}
		return nil, err
	}
	if buf.Len() == 0 {
		// Empty output is ambiguous. On a bucket-based remote (S3 and kin) rclone
		// resolves an absent key to an empty "directory" and cats nothing with
		// exit 0, so an absent blob would read as an empty one. Confirm with the
		// exact-leaf probe; only a present object is an empty blob.
		present, err := b.exists(ctx, path, encodeLeaf(lastSegment(key)))
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, &storage.BlobNotFoundError{Key: key}
		}
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

// Delete removes the object at key via "rclone deletefile". Deleting an absent
// object is a success (idempotent): a not-found from rclone is classified and
// mapped to nil; every other failure propagates.
func (b *blobStore) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	path := b.objectPath(key)

	if err := b.r.run(ctx, "deletefile", nil, []string{path}, nil, io.Discard); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// List returns the storage keys of every object under the store's prefix whose key
// begins with the caller's prefix, lexicographically ascending and duplicate-free.
// It runs "rclone lsf --files-only -R" rooted at "<remote>:<prefix>": the emitted
// paths are relative to that root, and each object path is decoded back to its
// key (decodeLeaf); entries that are not "@blob" leaves are skipped. The caller's
// prefix is applied to the DECODED key (it need not fall on a directory boundary)
// and is NOT name-validated. The result is sorted locally — rclone's ordering is
// not trusted, and the encoded order would put "a/b" before "a". An empty store
// surfaces as a not-found on the root directory, which maps to an empty listing
// rather than an error.
//
// List fails closed with *LegacyLayoutError if the listing holds a bare-path
// object (isLegacyLeaf) anywhere under the store root, whatever the prefix: New
// refused such a root, so one appearing later was written by an rclonestore <=
// v0.4.x (a rollback) and would otherwise be silently invisible. The check costs
// nothing — the recursive listing is already in hand.
func (b *blobStore) List(ctx context.Context, prefix string) ([]string, error) {
	rels, err := b.listObjects(ctx)
	if err != nil {
		return nil, err
	}
	if legacy, found := firstLegacyLeaf(rels); found {
		return nil, &LegacyLayoutError{Path: legacy}
	}

	seen := make(map[string]struct{})
	var keys []string
	for _, rel := range rels {
		key, ok := decodeLeaf(rel)
		if !ok {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// listObjects returns every object path under the store root, relative to it, as
// emitted by "rclone lsf --files-only -R" (blank lines and CRs dropped, order as
// rclone emitted it). A not-found root is an empty store. Every other failure
// propagates as the runner's *RcloneError.
func (b *blobStore) listObjects(ctx context.Context) ([]string, error) {
	var out bytes.Buffer
	if err := b.r.run(ctx, "lsf", []string{"--files-only", "-R"}, []string{b.listRoot()}, nil, &out); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var rels []string
	for _, line := range strings.Split(out.String(), "\n") {
		rel := strings.TrimRight(line, "\r")
		if rel == "" {
			continue
		}
		rels = append(rels, rel)
	}
	return rels, nil
}

// firstLegacyLeaf returns the lexically first legacy-shaped object path in rels,
// so the one a refusal names does not depend on rclone's listing order.
func firstLegacyLeaf(rels []string) (string, bool) {
	first, found := "", false
	for _, rel := range rels {
		if !isLegacyLeaf(rel) {
			continue
		}
		if !found || rel < first {
			first, found = rel, true
		}
	}
	return first, found
}

// refuseLegacyLayout is New's fail-closed check for a store root written by
// rclonestore <= v0.4.x: one recursive listing of the root (the same cost as one
// List call), refusing on the first legacy-shaped object path. There is no
// migration — the owner abandons pre-v0.5.0 data — and no marker object: a marker
// would be a second authority and would miss a later bare-path write by a
// rolled-back binary, which this check (and List) still catch. A listing failure
// is a *LayoutScanError, never read as clean and never as legacy.
func refuseLegacyLayout(ctx context.Context, b *blobStore) error {
	rels, err := b.listObjects(ctx)
	if err != nil {
		return &LayoutScanError{cause: err}
	}
	if legacy, found := firstLegacyLeaf(rels); found {
		return &LegacyLayoutError{Path: legacy}
	}
	return nil
}

// exists probes whether the blob object at path is present, using "rclone lsf
// --files-only" pointed at the exact object path. rclone resolves a remote:path
// that names a file to that single file and prints its leaf name, so the object is
// present only when the output is EXACTLY one line equal to leaf. A path that is a
// DIRECTORY instead lists its children — which is not the blob, so it is reported
// absent and Put's rcat decides (a filesystem remote refuses to write over a
// directory with an *RcloneError). It is never read as present, so a directory can
// never be cat'd and reported as a conflicting blob. Not-found exits and empty
// output are absent. Any non-not-found failure (auth, network) propagates, so a
// transient error is never mistaken for "absent" and silently overwritten by Put.
func (b *blobStore) exists(ctx context.Context, path, leaf string) (bool, error) {
	var out bytes.Buffer
	if err := b.r.run(ctx, "lsf", []string{"--files-only"}, []string{path}, nil, &out); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimRight(out.String(), "\r\n") == leaf, nil
}

// lastSegment returns the final '/'-separated segment of a validated key.
func lastSegment(key string) string {
	return key[strings.LastIndexByte(key, '/')+1:]
}

// rcat streams r straight to "rclone rcat", which reads the object body from stdin.
// No buffering: the blob flows reader → subprocess without ever materializing in
// this process. rcat produces no stdout, so it is discarded.
func (b *blobStore) rcat(ctx context.Context, path string, r io.Reader) error {
	return b.r.run(ctx, "rcat", nil, []string{path}, r, io.Discard)
}

// objectPath builds the single positional path for a key: the key's encoded
// object path (encodeLeaf) joined under the store prefix, appended to the remote in its correct form (see remoteJoin).
// The runner inserts "--" before this positional, so a key or remote beginning
// with '-' can never be parsed as a flag.
func (b *blobStore) objectPath(key string) string {
	return b.remoteJoin(joinPath(b.prefix, encodeLeaf(key)))
}

// listRoot is the recursive-listing (and startup-probe) root: the store prefix
// appended to the remote. lsf paths under it are relative to it, i.e. they are
// storage keys.
func (b *blobStore) listRoot() string {
	return b.remoteJoin(b.prefix)
}

// remoteJoin appends a store-relative path to the remote in the form rclone
// requires, which differs by how the remote was given:
//
//   - A NAMED remote "R" (a config alias, e.g. "myremote") is separated from its
//     path by a colon ("R:" concatenated with path): "myremote" and "blobs/x"
//     give "myremote:blobs/x". An empty path yields the bare "R:".
//   - A CONNECTION STRING (e.g. ":local:/tmp/x" or ":s3,provider=…:") already ends
//     its backend spec with a colon, so a further colon would be misparsed; the
//     path is appended with a slash ("<remote>/" concatenated with path):
//     ":local:/tmp/x" and "blobs/x" give ":local:/tmp/x/blobs/x". An empty path
//     yields the bare remote.
//
// The form is fixed at construction (connString) from whether the remote begins
// with ':'. Getting this wrong is the RC2 bug this replaces: a hardcoded colon
// turned ":local:/tmp/x" + key into the invalid ":local:/tmp/x:key".
func (b *blobStore) remoteJoin(path string) string {
	if b.connString {
		if path == "" {
			return b.remote
		}
		return b.remote + "/" + path
	}
	return b.remote + ":" + path
}

// joinPath joins a store-relative rest (a validated key, or a caller List prefix)
// under the store prefix, collapsing an empty store prefix so no spurious leading
// slash is introduced.
func joinPath(prefix, rest string) string {
	if prefix == "" {
		return rest
	}
	return prefix + "/" + rest
}

// isNotFound reports whether err is a not-found from rclone. It classifies on the
// runner's *RcloneError, primarily by exit code (3 directory-not-found, 4
// file-not-found per rclone's documented table) and secondarily by a "not found"
// stderr marker for remotes that surface not-found through another code — on any
// stderr line except rclone's missing-config notice (isMissingConfigNotice). It never
// inspects the remote or the positional path (RcloneError excludes them), so no
// credential can leak through classification.
//
// ASSUMPTION (RC3 validates against real rclone across remotes): exit codes 3/4 are
// the primary signal. If a backend reports a missing object with a different code
// and non-"not found" wording, RC3's storetest conformance will surface it and the
// marker set here is where the fix goes.
func isNotFound(err error) bool {
	var re *RcloneError
	if !errors.As(err, &re) {
		return false
	}
	if re.ExitCode == exitDirNotFound || re.ExitCode == exitFileNotFound {
		return true
	}
	for _, line := range strings.Split(re.Stderr, "\n") {
		if isMissingConfigNotice(line) {
			continue
		}
		if strings.Contains(strings.ToLower(line), notFoundMarker) {
			return true
		}
	}
	return false
}

// isMissingConfigNotice reports rclone's per-invocation notice that no config
// file exists (`NOTICE: Config file "<path>" not found - using defaults`). It is
// printed on EVERY call when there is no rclone.conf — the norm with
// connection-string remotes — and its "not found" is about the config file, not
// the object, so it must never turn an auth or network failure into "absent".
func isMissingConfigNotice(line string) bool {
	return strings.Contains(line, "Config file ") && strings.Contains(line, "not found - using defaults")
}
