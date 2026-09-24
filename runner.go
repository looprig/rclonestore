// Package rclonestore implements storage.Blobs by driving the external rclone
// binary as a context-bounded subprocess (argv exec — never a shell string,
// never librclone/cgo).
package rclonestore

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// stderrTailBytes bounds how many trailing bytes of a failed rclone invocation's
// stderr are retained in a RcloneError, so a pathological run cannot balloon an
// error value or a log line. rclone's diagnostics DO echo the remote (inline
// credentials included), so the capture is redacted before it is retained; see
// redactor.
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
	redact     redactor      // removes inline-remote credentials and the config path from captured stderr
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
	cmd.Env = childEnv(os.Environ())
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	// Capture margin extra bytes so a secret cut by the bound can be dropped after
	// redaction without shrinking the retained tail below stderrTailBytes.
	tail := &tailWriter{max: stderrTailBytes + r.redact.margin()}
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
			Stderr:     r.redact.redactTail(tail.tail(), tail.truncated(), stderrTailBytes),
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
	max   int
	buf   []byte
	total int // bytes ever written, to tell whether the tail lost its start
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.total += n
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

// truncated reports whether bytes were discarded from the front of the tail.
func (w *tailWriter) truncated() bool { return w.total > len(w.buf) }

// rclone maps EVERY global flag to an RCLONE_<FLAG> environment variable (and
// backend options to RCLONE_<BACKEND>_<OPTION>), and the child would inherit
// them. Many change the data path or the output rclonestore parses — measured
// with rclone v1.71.1: RCLONE_PROGRESS writes transfer stats into Get's stdout
// and defeats Put's exact-leaf conflict probe; RCLONE_DRY_RUN and
// RCLONE_INTERACTIVE make Put return success having written nothing; the log
// family (RCLONE_LOG_*, RCLONE_USE_JSON_LOG, RCLONE_VERBOSE, …) changes the
// stderr grammar redaction relies on, and argv flags cannot override it. So the
// child environment is DENY BY DEFAULT: every RCLONE_* variable is dropped except
// this allowlist, each checked against rclone v1.71.1 and neutral to what is read
// or written, the output format, interactivity and dry-run:
//
//   - RCLONE_CONFIG (the config file path) and RCLONE_CONFIG_* (RCLONE_CONFIG_PASS,
//     RCLONE_CONFIG_DIR and env-defined remotes RCLONE_CONFIG_<REMOTE>_<OPTION>);
//   - config decryption: RCLONE_PASSWORD_COMMAND, RCLONE_ASK_PASSWORD;
//   - transport tuning (fs/config.go, configflags): timeouts, retries, TLS
//     material, bandwidth/transaction limits, user agent, bind address, HTTP/2 and
//     keep-alive switches, DSCP.
//
// Excluded on purpose, among others: RCLONE_HEADER* and RCLONE_METADATA_SET
// (they change requests and stored metadata), RCLONE_DISABLE (feature
// switches), RCLONE_RC* (would start an rc server per call), and every backend
// option such as RCLONE_S3_VERSION_AT or RCLONE_LOCAL_ENCODING (they change what
// is read or how names map) — put backend options in the connection string or the
// config file. Variables not named RCLONE_* (HTTP(S)_PROXY, HOME, PATH, cloud SDK
// credentials such as AWS_*) pass through untouched.
var allowedRcloneEnv = map[string]struct{}{
	"RCLONE_CONFIG":                   {},
	"RCLONE_PASSWORD_COMMAND":         {},
	"RCLONE_ASK_PASSWORD":             {},
	"RCLONE_CONTIMEOUT":               {},
	"RCLONE_TIMEOUT":                  {},
	"RCLONE_EXPECT_CONTINUE_TIMEOUT":  {},
	"RCLONE_RETRIES":                  {},
	"RCLONE_RETRIES_SLEEP":            {},
	"RCLONE_LOW_LEVEL_RETRIES":        {},
	"RCLONE_CA_CERT":                  {},
	"RCLONE_CLIENT_CERT":              {},
	"RCLONE_CLIENT_KEY":               {},
	"RCLONE_CLIENT_PASS":              {},
	"RCLONE_NO_CHECK_CERTIFICATE":     {},
	"RCLONE_BWLIMIT":                  {},
	"RCLONE_BWLIMIT_FILE":             {},
	"RCLONE_TPSLIMIT":                 {},
	"RCLONE_TPSLIMIT_BURST":           {},
	"RCLONE_USER_AGENT":               {},
	"RCLONE_BIND":                     {},
	"RCLONE_DISABLE_HTTP2":            {},
	"RCLONE_DISABLE_HTTP_KEEP_ALIVES": {},
	"RCLONE_DSCP":                     {},
}

// rcloneEnvPrefix marks the variables rclone reads; rcloneConfigEnvPrefix is the
// allowed config family (a non-empty remainder is required).
const (
	rcloneEnvPrefix       = "RCLONE_"
	rcloneConfigEnvPrefix = "RCLONE_CONFIG_"
)

// childEnv returns environ with every RCLONE_* variable removed except the
// allowlist above. Names compare case-insensitively: Windows environment names
// are case-insensitive, and elsewhere a differently-cased name is not one rclone
// reads, so dropping it is harmless.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if allowedInChildEnv(strings.ToUpper(name)) {
			out = append(out, kv)
		}
	}
	return out
}

func allowedInChildEnv(upperName string) bool {
	if !strings.HasPrefix(upperName, rcloneEnvPrefix) {
		return true
	}
	if _, ok := allowedRcloneEnv[upperName]; ok {
		return true
	}
	return strings.HasPrefix(upperName, rcloneConfigEnvPrefix) && len(upperName) > len(rcloneConfigEnvPrefix)
}
