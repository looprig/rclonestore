package rclonestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/ciram-co/storekit"
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

// blobStore implements storekit.Blobs by driving the rclone binary through a
// runner. Each object's rclone path is built from the remote, the store prefix,
// and the per-call validated key by remoteJoin, which is aware of both remote
// forms — a named remote ("R:<path>") vs. a ":backend:" connection string
// ("<remote>/<path>"). remote and prefix are fixed at construction; the key is a
// validated storekit name supplied per call. The store is pure mechanism over
// rclone — the workspace store above it owns the single-writer lease that makes
// the Put existence-then-write sequence sound.
type blobStore struct {
	r          *runner
	remote     string
	prefix     string
	connString bool // remote is a ":backend:" connection string (leading ':'), not a named remote
}

var _ storekit.Blobs = (*blobStore)(nil)

// newBlobStore constructs a blobStore over an already-configured runner. Binary
// resolution, option validation, and the remote-reachability probe live in New;
// this constructor takes the already-resolved remote and prefix directly and
// derives the remote form (named vs. connection string) from the remote's leading
// byte.
func newBlobStore(r *runner, remote, prefix string) *blobStore {
	return &blobStore{
		r:          r,
		remote:     remote,
		prefix:     prefix,
		connString: strings.HasPrefix(remote, ":"),
	}
}

// PutSourceError reports that reading the caller-supplied Put reader failed before
// the blob could be compared against an existing object or streamed to rclone. It
// carries the storekit key (safe to log — it is a validated canonical name) and
// wraps the underlying read error.
type PutSourceError struct {
	Key   string
	cause error
}

func (e *PutSourceError) Error() string {
	return "rclonestore: reading Put source for blob " + strconv.Quote(e.Key) + " failed"
}

func (e *PutSourceError) Unwrap() error { return e.cause }

// Put honors storekit's content-addressed conflict contract. It first probes for
// an existing object:
//
//   - ABSENT (the common content-addressed case): stream r straight to
//     "rclone rcat" with no buffering — the blob never lands in memory.
//   - PRESENT: read r fully, "rclone cat" the existing object, and compare bytes.
//     Byte-identical → success/no-op (the object is NOT re-uploaded). Different →
//     *storekit.BlobConflictError with the original left untouched (no upload).
//
// The present branch buffers both the incoming reader (io.ReadAll) and the existing
// object (into memory) to compare them. This is deliberately the rare path: keys
// are content-addressed, so a re-Put of DIFFERENT bytes under the same key is
// pathological, and a re-Put of IDENTICAL bytes is a cheap idempotent no-op. The
// probe→write sequence is a TOCTOU only under concurrent writers to the same key;
// the workspace store holds a single-writer lease over the key space, so no
// concurrent writer exists by construction.
func (b *blobStore) Put(ctx context.Context, key string, r io.Reader) error {
	if err := storekit.ValidateName(key); err != nil {
		return err
	}
	path := b.objectPath(key)

	present, err := b.exists(ctx, path)
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
	return &storekit.BlobConflictError{Key: key}
}

// Get streams the object at key back to the caller. It runs "rclone cat" to
// completion into an in-memory buffer and returns an independent io.ReadCloser over
// those bytes. Buffering (rather than piping rclone's stdout through) is what lets
// Get satisfy the contract's synchronous not-found: storetest expects Get itself —
// not a later Read — to return *storekit.BlobNotFoundError, which is only knowable
// once rclone has exited. A missing object is classified from rclone's exit
// code/stderr and mapped to *storekit.BlobNotFoundError.
func (b *blobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := storekit.ValidateName(key); err != nil {
		return nil, err
	}
	path := b.objectPath(key)

	var buf bytes.Buffer
	if err := b.r.run(ctx, "cat", nil, []string{path}, nil, &buf); err != nil {
		if isNotFound(err) {
			return nil, &storekit.BlobNotFoundError{Key: key}
		}
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

// Delete removes the object at key via "rclone deletefile". Deleting an absent
// object is a success (idempotent): a not-found from rclone is classified and
// mapped to nil; every other failure propagates.
func (b *blobStore) Delete(ctx context.Context, key string) error {
	if err := storekit.ValidateName(key); err != nil {
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

// List returns the storekit keys of every object under the store's prefix whose key
// begins with the caller's prefix, lexicographically ascending and duplicate-free.
// It runs "rclone lsf --files-only -R" rooted at "<remote>:<prefix>": the emitted
// paths are relative to that root, so they ARE the storekit keys. The caller's
// prefix is applied locally (it need not fall on a directory boundary) and is NOT
// name-validated. The result is sorted locally — rclone's ordering is not trusted.
// An empty store surfaces as a not-found on the root directory, which maps to an
// empty listing rather than an error.
func (b *blobStore) List(ctx context.Context, prefix string) ([]string, error) {
	var out bytes.Buffer
	if err := b.r.run(ctx, "lsf", []string{"--files-only", "-R"}, []string{b.listRoot()}, nil, &out); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	seen := make(map[string]struct{})
	var keys []string
	for _, line := range strings.Split(out.String(), "\n") {
		key := strings.TrimRight(line, "\r")
		if key == "" {
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

// exists probes whether the object at path is present, using "rclone lsf
// --files-only" pointed at the exact object path. rclone resolves a remote:path
// that names a file to that single file, so a present object yields a non-empty
// listing (its leaf) and an absent one yields either a not-found exit or empty
// output — both reported as absent. Any non-not-found failure (auth, network)
// propagates, so a transient error is never mistaken for "absent" and silently
// overwritten by Put.
func (b *blobStore) exists(ctx context.Context, path string) (bool, error) {
	var out bytes.Buffer
	if err := b.r.run(ctx, "lsf", []string{"--files-only"}, []string{path}, nil, &out); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out.String()) != "", nil
}

// rcat streams r straight to "rclone rcat", which reads the object body from stdin.
// No buffering: the blob flows reader → subprocess without ever materializing in
// this process. rcat produces no stdout, so it is discarded.
func (b *blobStore) rcat(ctx context.Context, path string, r io.Reader) error {
	return b.r.run(ctx, "rcat", nil, []string{path}, r, io.Discard)
}

// objectPath builds the single positional path for a key: the key joined under
// the store prefix, appended to the remote in its correct form (see remoteJoin).
// The runner inserts "--" before this positional, so a key or remote beginning
// with '-' can never be parsed as a flag.
func (b *blobStore) objectPath(key string) string {
	return b.remoteJoin(joinPath(b.prefix, key))
}

// listRoot is the recursive-listing (and startup-probe) root: the store prefix
// appended to the remote. lsf paths under it are relative to it, i.e. they are
// storekit keys.
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
// stderr marker for remotes that surface not-found through another code. It never
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
	return strings.Contains(strings.ToLower(re.Stderr), notFoundMarker)
}
