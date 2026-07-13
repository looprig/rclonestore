package rclonestore

import (
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
