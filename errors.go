package rclonestore

import (
	"errors"
	"strconv"
	"strings"
)

// PersistencePathError reports an invalid local filesystem root declared by the
// caller or derived from an inline local remote. Path contains only the local
// filesystem portion, never a named remote or inline backend parameters.
type PersistencePathError struct {
	Path string
	Rule string

	cause error
}

func (e *PersistencePathError) Error() string {
	return "rclonestore: invalid persistence path " + strconv.Quote(e.Path) + ": " + e.Rule
}

func (e *PersistencePathError) Unwrap() error { return e.cause }

// RcloneError reports a failed rclone invocation: a non-zero exit, a start
// failure, or a subprocess killed by its context deadline/cancellation. Callers
// classify with errors.As.
//
// It deliberately carries only information that cannot embed a credential:
//
//   - Subcommand — the rclone subcommand (e.g. "rcat", "cat", "lsf", "deletefile").
//   - Args       — the subcommand's own flags (the subflags). It NEVER contains the
//     positional path arguments (which embed the remote name and the storage key)
//     nor the global --config path (which points at a file that may hold remote
//     credentials). The caller passes only non-secret subflags.
//   - ExitCode   — the process exit status; -1 for a start failure or a signal kill.
//   - Stderr     — a bounded tail (~4 KiB) of the process's stderr, surfaced as-is
//     from rclone (whose diagnostics do not echo config secrets) and bounded so it
//     cannot balloon an error or log line.
//
// The config path, the remote, and every positional are excluded by construction,
// so an RcloneError is always safe to log.
type RcloneError struct {
	Subcommand string
	Args       []string
	ExitCode   int
	Stderr     string

	cause error // underlying *exec.ExitError / start error, or the context error on a kill
}

// Error renders the subcommand, the exit status, and a sanitized (quoted) stderr
// tail. The tail is strconv.Quote'd so newlines or control bytes emitted by the
// subprocess cannot inject into a log line. No credential-bearing value is ever
// included.
func (e *RcloneError) Error() string {
	var b strings.Builder
	b.WriteString("rclonestore: rclone ")
	b.WriteString(e.Subcommand)
	if e.ExitCode >= 0 {
		b.WriteString(" exited ")
		b.WriteString(strconv.Itoa(e.ExitCode))
	} else {
		b.WriteString(" failed")
	}
	if tail := strings.TrimRight(e.Stderr, "\n"); tail != "" {
		b.WriteString(": stderr ")
		b.WriteString(strconv.Quote(tail))
	}
	return b.String()
}

// Unwrap returns the underlying cause so callers can classify with errors.Is /
// errors.As: the *exec.ExitError (or start error) on a genuine failure, or the
// context error (context.DeadlineExceeded / context.Canceled) when the call was
// killed by its deadline or cancellation.
func (e *RcloneError) Unwrap() error { return e.cause }

// ErrLegacyLayout is the sentinel every *LegacyLayoutError matches with
// errors.Is: the store root holds objects written by rclonestore <= v0.4.x, which
// stored each key at its bare path. v0.5.0 stores a key at "<key>@blob" so that a
// key and its "/…" extension can coexist (storage v0.7.0); it does not migrate
// the old layout and does not read it.
var ErrLegacyLayout = errors.New("rclonestore: store root holds pre-v0.5.0 data")

// LegacyLayoutError reports that New (or a later List) found an object at a bare
// key path under the store root — the layout rclonestore <= v0.4.x wrote. It is
// refused rather than ignored because that data would otherwise be silently
// invisible: Get would report it absent and List would omit it while new writes
// landed beside it. There is no migration; move or delete the store root (or,
// if it was written by something other than rclonestore, keep that data out of
// the root — the root must be dedicated to rclonestore).
//
// Path is the lexically first such object, relative to the store root. It is a
// valid storage name (that is what makes it legacy-shaped) and carries neither the
// remote nor the prefix, so the error is safe to log.
type LegacyLayoutError struct {
	Path string
}

func (e *LegacyLayoutError) Error() string {
	return "rclonestore: store root holds pre-v0.5.0 blob " + strconv.Quote(e.Path) +
		" (unsuffixed layout; no migration — move or delete the store root)"
}

// Unwrap returns ErrLegacyLayout.
func (e *LegacyLayoutError) Unwrap() error { return ErrLegacyLayout }

// LayoutScanError reports that New's legacy-layout scan — one recursive listing
// of the store root, run after the reachability probe succeeded — failed. The
// root is neither known clean nor known legacy, so no Store is returned. It wraps
// the credential-safe *RcloneError.
type LayoutScanError struct {
	cause error
}

func (e *LayoutScanError) Error() string {
	return "rclonestore: legacy-layout scan of the store root failed: " + e.cause.Error()
}

func (e *LayoutScanError) Unwrap() error { return e.cause }
