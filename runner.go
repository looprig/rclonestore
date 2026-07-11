// Package rclonestore implements storage.Blobs by driving the external rclone
// binary as a context-bounded subprocess (argv exec — never a shell string,
// never librclone/cgo).
package rclonestore

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// stderrTailBytes bounds how many trailing bytes of a failed rclone invocation's
// stderr are retained in a RcloneError. rclone's own diagnostics do not echo config
// secrets, but the tail is bounded regardless so a pathological run cannot balloon
// an error value or a log line.
const stderrTailBytes = 4 << 10 // 4 KiB

// runner executes a single rclone subcommand as a context-bounded subprocess. It
// is pure mechanism: it builds an argv (no shell), streams stdin in and stdout out,
// and captures a bounded tail of stderr. Validation of keys/remotes and the choice
// of which subflags are safe to surface belong to the caller (the Blobs adapter),
// not here.
type runner struct {
	binary     string        // path to the rclone binary (resolved by the caller)
	configPath string        // optional --config path; referenced only, never read or logged here
	timeout    time.Duration // optional per-call bound; 0 relies on the caller's ctx alone
}

// run executes:
//
//	<binary> [--config <configPath>] <subcommand> [subflags...] -- <positionals...>
//
// It streams stdin to the process and the process's stdout to stdout, capturing a
// bounded tail of stderr. The whole call is bounded by ctx (and, if set, by
// r.timeout); on timeout or cancellation the subprocess is killed. On a non-zero
// exit, a start failure, or a kill it returns a *RcloneError carrying the
// subcommand, the (safe) subflags, the exit code, and the stderr tail — never the
// config path, the remote, or any positional (which embed credentials/keys). On
// success stdout is fully streamed and run returns nil.
func (r *runner) run(ctx context.Context, subcommand string, subflags, positionals []string, stdin io.Reader, stdout io.Writer) error {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	args := buildArgs(r.configPath, subcommand, subflags, positionals)

	// argv exec only — no shell string is ever constructed. Every element is a
	// discrete argument, `--` terminates flags before the positionals so no key
	// can be read as a flag, and ctx bounds and kills the process. The permission
	// to run rclone at all is the caller's; this is why the exec is safe.
	cmd := exec.CommandContext(ctx, r.binary, args...) // #nosec G204 -- argv-only (no shell), ctx-bounded, `--` before positionals; see CLAUDE.md
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	tail := &tailWriter{max: stderrTailBytes}
	cmd.Stderr = tail

	if err := cmd.Run(); err != nil {
		cause := err
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The deadline/cancellation is the true cause and the one callers
			// classify on (errors.Is(err, context.DeadlineExceeded)).
			cause = ctxErr
		}
		return &RcloneError{
			Subcommand: subcommand,
			Args:       append([]string(nil), subflags...),
			ExitCode:   exitCode(err),
			Stderr:     string(tail.tail()),
			cause:      cause,
		}
	}
	return nil
}

// buildArgs assembles the rclone argv: the global --config flag (if set) before
// the subcommand, then the subflags, then `--` inserted exactly once immediately
// before the positional path arguments so no positional beginning with '-' is
// parsed as a flag.
func buildArgs(configPath, subcommand string, subflags, positionals []string) []string {
	args := make([]string, 0, 3+len(subflags)+len(positionals))
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, subcommand)
	args = append(args, subflags...)
	args = append(args, "--")
	args = append(args, positionals...)
	return args
}

// exitCode extracts the process exit status from a cmd.Run error: the real code
// for a normal non-zero exit, or -1 for a start failure or a signal kill (e.g. a
// ctx-triggered SIGKILL) — matching (*exec.ExitError).ExitCode()'s -1 convention.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// tailWriter is an io.Writer that retains only the last max bytes written to it.
// It backs the bounded stderr capture: an unbounded (or hostile) subprocess cannot
// grow the buffer past max, and a single oversized write is truncated to its own
// trailing max bytes. Not safe for concurrent use; exec writes stderr from one
// goroutine and the tail is read only after cmd.Run returns.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.max <= 0 {
		return n, nil
	}
	if len(p) >= w.max {
		trimmed := make([]byte, w.max)
		copy(trimmed, p[len(p)-w.max:])
		w.buf = trimmed
		return n, nil
	}
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		trimmed := make([]byte, w.max)
		copy(trimmed, w.buf[len(w.buf)-w.max:])
		w.buf = trimmed
	}
	return n, nil
}

func (w *tailWriter) tail() []byte { return w.buf }
