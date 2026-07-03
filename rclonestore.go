package rclonestore

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ciram-co/storekit"
)

// defaultBinary is the rclone executable name resolved via exec.LookPath when
// Options.Binary is empty.
const defaultBinary = "rclone"

// Named-remote and connection-string remote forms. A remote is EITHER a named
// rclone remote (a config alias) matching namedRemoteRe, OR an inline connection
// string beginning with ':' whose backend spec matches connStringRe.
//
//   - namedRemoteRe: a config alias, e.g. "myremote" — letters, digits, '_', '-'.
//   - connStringRe: ":<backend>[,<params>…]:" — a leading colon, a lowercase
//     alphanumeric backend name, optional comma-separated backend parameters
//     (their values may themselves contain colons/paths), and a closing colon that
//     terminates the backend spec. Anything after the closing colon (a path) is
//     the operator's and is not matched here. Examples matched: ":local:",
//     ":local:/some/root", ":s3,provider=Minio:/bucket/prefix".
var (
	namedRemoteRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	connStringRe  = regexp.MustCompile(`^:[a-z0-9]+(,[^:]*)*:`)
)

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
}

// Store is a storekit.Blobs backed by the external rclone binary. It embeds the
// unexported blobStore, so a Store IS-A storekit.Blobs (Put/Get/Delete/List are
// promoted). Construct one with New.
type Store struct {
	*blobStore
}

var _ storekit.Blobs = (*Store)(nil)

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
func New(opts Options) (*Store, error) {
	if err := validateRemote(opts.Remote); err != nil {
		return nil, err
	}
	if err := validatePrefix(opts.Prefix); err != nil {
		return nil, err
	}
	if opts.Timeout < 0 {
		return nil, &OptionsError{Field: "Timeout", Rule: "must not be negative"}
	}

	name := opts.Binary
	if name == "" {
		name = defaultBinary
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return nil, &BinaryError{Binary: name, cause: err}
	}

	r := &runner{binary: resolved, configPath: opts.ConfigPath, timeout: opts.Timeout}
	bs := newBlobStore(r, opts.Remote, opts.Prefix)

	if err := probe(bs); err != nil {
		return nil, err
	}
	return &Store{blobStore: bs}, nil
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

// validateRemote accepts a named remote or a connection string and rejects
// anything else. It never places the remote value in the error: a connection
// string can embed credentials.
func validateRemote(remote string) error {
	if remote == "" {
		return &OptionsError{Field: "Remote", Rule: "required"}
	}
	if strings.HasPrefix(remote, ":") {
		if !connStringRe.MatchString(remote) {
			return &OptionsError{Field: "Remote", Rule: "malformed connection string (want :backend:[path])"}
		}
		return nil
	}
	if !namedRemoteRe.MatchString(remote) {
		return &OptionsError{Field: "Remote", Rule: "not a named remote ([A-Za-z0-9_-]) or a :backend: connection string"}
	}
	return nil
}

// validatePrefix accepts an empty prefix or a safe relative path fragment: no
// leading/trailing '/', no empty, "." or ".." segment, no NUL. The prefix is
// operator config (not an untrusted key), so segment bytes are otherwise
// unrestricted — the storekit key appended per call carries the strict grammar.
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
