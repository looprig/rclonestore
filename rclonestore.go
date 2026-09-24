package rclonestore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/looprig/storage"
)

// defaultBinary is the rclone executable name resolved via exec.LookPath when
// Options.Binary is empty.
const defaultBinary = "rclone"

// namedRemoteRe validates a config alias. Inline connection strings need a small
// scanner rather than a regexp because quoted backend parameter values may contain
// colons and commas.
var namedRemoteRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type parsedRemote struct {
	connectionString bool
	backend          string
	spec             string // text between the leading ':' and the spec-ending ':' (backend plus any ",params")
	path             string
}

// Options configures a rclonestore Store. Only Remote is required. No field is
// ever logged or placed in an error verbatim: a connection-string Remote or a
// ConfigPath can embed remote credentials.
type Options struct {
	// Remote is required: a named rclone remote (e.g. "myremote") OR a ":backend:"
	// connection string (e.g. ":local:/data", ":s3,provider=Minio:/bucket").
	Remote string
	// Prefix is an optional path fragment under the remote. If set it must be a
	// safe relative path: no leading/trailing '/', no empty, "." or ".." segment.
	Prefix string
	// Binary is the rclone executable; default "rclone", resolved via exec.LookPath.
	Binary string
	// ConfigPath is an optional --config path. It is referenced by path only —
	// never opened, parsed, copied, or logged here (it may hold remote secrets).
	ConfigPath string
	// Timeout optionally bounds every rclone invocation (including the startup
	// probe). Zero means each call is bounded only by the caller's context.
	Timeout time.Duration
	// PersistencePaths declares local filesystem roots used by the backend. Inline
	// :local: remotes are discovered automatically, so this is primarily for named
	// remotes, which are never introspected. Entries are already-effective roots;
	// Prefix is not appended to them.
	PersistencePaths []string
}

// Store is a storage.Blobs backed by the external rclone binary. It embeds the
// unexported blobStore, so a Store IS-A storage.Blobs (Put/Get/Delete/List are
// promoted). Construct one with New.
type Store struct {
	*blobStore
}

var _ storage.Blobs = (*Store)(nil)
var _ storage.PathReporter = (*Store)(nil)

// New validates opts, resolves the rclone binary, and probes that the remote is
// reachable before returning a Store — failing loudly (fail-secure) at
// construction rather than at first use:
//
//  1. Remote is validated as a named remote or a connection string (*OptionsError).
//  2. Prefix, if set, is validated as a safe path fragment (*OptionsError).
//  3. Binary (default "rclone") is resolved with exec.LookPath (*BinaryError).
//  4. The remote root is probed with "rclone lsf --max-depth 0 -- <root>", bounded
//     by opts.Timeout. A not-found root is treated as reachable-but-empty (a fresh
//     store creates it on first Put); any other failure is a *ProbeError.
//  5. The root is scanned for objects written by rclonestore <= v0.4.x, which
//     stored each key at its bare path (v0.5.0 stores "<key>@blob"): one
//     recursive "rclone lsf --files-only -R" of the root, bounded by
//     opts.Timeout — the same cost as one List. A legacy object is refused with
//     *LegacyLayoutError (errors.Is ErrLegacyLayout); there is no migration. A
//     scan failure is a *LayoutScanError.
func New(opts Options) (*Store, error) {
	remote, err := parseRemote(opts.Remote)
	if err != nil {
		return nil, err
	}
	if err := validatePrefix(opts.Prefix); err != nil {
		return nil, err
	}
	if opts.Timeout < 0 {
		return nil, &OptionsError{Field: "Timeout", Rule: "must not be negative"}
	}
	paths, err := resolveStoragePaths(opts, remote)
	if err != nil {
		return nil, err
	}

	name := opts.Binary
	if name == "" {
		name = defaultBinary
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return nil, &BinaryError{Binary: name, cause: err}
	}

	r := &runner{
		binary:     resolved,
		configPath: opts.ConfigPath,
		timeout:    opts.Timeout,
		redact:     newRedactor(remote, opts.ConfigPath),
	}
	bs := newBlobStore(r, opts.Remote, opts.Prefix, paths)

	if err := probe(bs); err != nil {
		return nil, err
	}
	if err := refuseLegacyLayout(context.Background(), bs); err != nil {
		return nil, err
	}
	return &Store{blobStore: bs}, nil
}

// resolveStoragePaths freezes the local persistence roots described by opts.
// Named and non-local inline remotes are opaque unless the caller explicitly
// declares paths; rclone configuration is never inspected.
func resolveStoragePaths(opts Options, remote parsedRemote) ([]string, error) {
	candidates := append([]string(nil), opts.PersistencePaths...)
	if localRoot, ok := inlineLocalRoot(remote); ok {
		if localRoot == "" {
			localRoot = "."
		} else {
			localRoot = filepath.FromSlash(localRoot)
		}
		if opts.Prefix != "" {
			localRoot = filepath.Join(localRoot, filepath.FromSlash(opts.Prefix))
		}
		candidates = append(candidates, localRoot)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		canonical, err := canonicalizePersistencePath(candidate)
		if err != nil {
			return nil, err
		}
		paths = append(paths, canonical)
	}
	sort.Strings(paths)
	deduplicated := paths[:0]
	for _, path := range paths {
		if len(deduplicated) == 0 || path != deduplicated[len(deduplicated)-1] {
			deduplicated = append(deduplicated, path)
		}
	}
	return deduplicated, nil
}

// inlineLocalRoot returns the already-parsed path portion of an exact inline
// local backend. Backend parameters were discarded by parseRemote and therefore
// cannot enter a path or filesystem error.
func inlineLocalRoot(remote parsedRemote) (string, bool) {
	if !remote.connectionString || remote.backend != "local" {
		return "", false
	}
	return remote.path, true
}

// canonicalizePersistencePath resolves symlinks through the nearest existing
// ancestor while preserving a not-yet-created suffix. Lstat is intentional: it
// distinguishes an existing broken symlink from an ordinary nonexistent leaf.
func canonicalizePersistencePath(path string) (string, error) {
	if path == "" {
		return "", &PersistencePathError{Path: path, Rule: "must not be empty"}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", &PersistencePathError{Path: path, Rule: "must be an absolute filesystem path", cause: err}
	}

	current := filepath.Clean(abs)
	var suffix []string
	for {
		_, lstatErr := os.Lstat(current)
		if lstatErr == nil {
			resolved, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", &PersistencePathError{Path: path, Rule: "must not contain a broken symlink", cause: evalErr}
			}
			info, statErr := os.Stat(resolved)
			if statErr != nil {
				return "", &PersistencePathError{Path: path, Rule: "existing path must be a directory", cause: statErr}
			}
			if !info.IsDir() {
				cause := &os.PathError{Op: "resolve", Path: resolved, Err: syscall.ENOTDIR}
				return "", &PersistencePathError{Path: path, Rule: "existing path must be a directory", cause: cause}
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(lstatErr, fs.ErrNotExist) {
			return "", &PersistencePathError{Path: path, Rule: "cannot resolve filesystem path", cause: lstatErr}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", &PersistencePathError{Path: path, Rule: "cannot find an existing ancestor", cause: lstatErr}
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// Close releases resources held by the Store. rclone is driven per-call as a
// short-lived subprocess and holds no persistent connection, so Close is a
// documented no-op that always returns nil; it exists only so callers can treat a
// Store uniformly with stores that do hold resources.
func (s *Store) Close() error { return nil }

// probe verifies the remote is reachable by listing its root at depth 0. A
// not-found (the root directory does not exist yet on a fresh store) is NOT a
// failure — it means rclone reached the backend and found the root empty/absent,
// which first Put will create — so it maps to nil, matching List's semantics. Any
// other failure (unknown remote, auth, network) is wrapped in a *ProbeError. The
// probe is bounded by the runner's timeout (opts.Timeout).
func probe(bs *blobStore) error {
	var out bytes.Buffer
	err := bs.r.run(context.Background(), "lsf", []string{"--max-depth", "0"}, []string{bs.listRoot()}, nil, &out)
	if err == nil || isNotFound(err) {
		return nil
	}
	return &ProbeError{cause: err}
}

// parseRemote accepts a named remote or scans an inline connection string. The
// scan stops only at an unquoted colon; single- and double-quoted parameter values
// may contain colons/commas and escape their quote by doubling it. Errors never
// carry the remote value because backend parameters can contain credentials.
func parseRemote(remote string) (parsedRemote, error) {
	if remote == "" {
		return parsedRemote{}, &OptionsError{Field: "Remote", Rule: "required"}
	}
	if strings.HasPrefix(remote, ":") {
		quote := byte(0)
		for i := 1; i < len(remote); i++ {
			ch := remote[i]
			if quote != 0 {
				if ch == quote {
					if i+1 < len(remote) && remote[i+1] == quote {
						i++
						continue
					}
					quote = 0
				}
				continue
			}
			switch ch {
			case '\'', '"':
				quote = ch
			case ':':
				backendSpec := remote[1:i]
				backend, _, _ := strings.Cut(backendSpec, ",")
				if !validBackendName(backend) {
					return parsedRemote{}, &OptionsError{Field: "Remote", Rule: "malformed connection string (want :backend:[path])"}
				}
				return parsedRemote{connectionString: true, backend: backend, spec: backendSpec, path: remote[i+1:]}, nil
			}
		}
		return parsedRemote{}, &OptionsError{Field: "Remote", Rule: "malformed connection string (want balanced quoted parameters and :backend:[path])"}
	}
	if !namedRemoteRe.MatchString(remote) {
		return parsedRemote{}, &OptionsError{Field: "Remote", Rule: "not a named remote ([A-Za-z0-9_-]) or a :backend: connection string"}
	}
	return parsedRemote{}, nil
}

func validBackendName(backend string) bool {
	if backend == "" {
		return false
	}
	for i := 0; i < len(backend); i++ {
		if ch := backend[i]; (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

// validatePrefix accepts an empty prefix or a safe relative path fragment: no
// leading/trailing '/', no empty, "." or ".." segment, no NUL. The prefix is
// operator config (not an untrusted key), so segment bytes are otherwise
// unrestricted — the storage key appended per call carries the strict grammar.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.ContainsRune(prefix, 0) {
		return &OptionsError{Field: "Prefix", Rule: "must not contain a NUL byte"}
	}
	if strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return &OptionsError{Field: "Prefix", Rule: "no leading or trailing '/'"}
	}
	for _, seg := range strings.Split(prefix, "/") {
		switch seg {
		case "":
			return &OptionsError{Field: "Prefix", Rule: "no empty path segment"}
		case ".", "..":
			return &OptionsError{Field: "Prefix", Rule: "no '.' or '..' path segment"}
		}
	}
	return nil
}

// OptionsError reports an invalid field in Options. It names the Field and the
// Rule violated but never the offending value: Remote (a connection string) or a
// ConfigPath can embed credentials, so no option value is ever surfaced.
type OptionsError struct {
	Field string
	Rule  string
}

func (e *OptionsError) Error() string {
	return "rclonestore: invalid option " + e.Field + ": " + e.Rule
}

// BinaryError reports that the rclone binary could not be resolved on PATH. The
// binary name/path is operator-supplied configuration (no credential), so it is
// safe to name; the underlying exec.LookPath error is wrapped for errors.Is/As.
type BinaryError struct {
	Binary string
	cause  error
}

func (e *BinaryError) Error() string {
	return "rclonestore: rclone binary " + strconv.Quote(e.Binary) + " not found on PATH"
}

func (e *BinaryError) Unwrap() error { return e.cause }

// ProbeError reports that the startup reachability probe failed: rclone could not
// list the remote root and the failure was not a benign not-found. It wraps the
// underlying *RcloneError, which is credential-safe by construction (it excludes
// the config path, the remote, and every positional). The remote is deliberately
// NOT carried on ProbeError — a connection-string remote can embed secrets.
type ProbeError struct {
	cause error
}

func (e *ProbeError) Error() string {
	return "rclonestore: remote unreachable at startup probe: " + e.cause.Error()
}

func (e *ProbeError) Unwrap() error { return e.cause }
