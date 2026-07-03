package rclonestore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSpec configures a generated #!/bin/sh stand-in for the rclone binary. Each
// test writes its own script into its own t.TempDir(), so behavior is baked into
// the script (no shared env vars) and the suite stays parallel-safe.
type fakeSpec struct {
	argvPath   string // absolute path the fake writes its argv to, one arg per line
	copyStdin  bool   // exec cat: stream stdin -> stdout
	stdoutPath string // if set (and not copyStdin), cat this file to stdout
	stderrPath string // if set, cat this file to stderr
	exitCode   int    // exit status (ignored when copyStdin or sleepSecs > 0)
	sleepSecs  int    // if > 0, exec sleep this long (for the ctx-timeout test)
}

// writeFakeRclone generates the fake rclone script and returns its path. The
// script first records its exact argv, then behaves per spec. For the sleep and
// copy cases it uses `exec` so the tracked PID *is* the long-lived process: a
// ctx-triggered SIGKILL then kills it directly, leaving no orphan holding the
// stdout pipe open (which would otherwise block cmd.Wait).
func writeFakeRclone(t *testing.T, dir string, spec fakeSpec) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("printf '%s\\n' \"$@\" > '" + spec.argvPath + "'\n")
	switch {
	case spec.sleepSecs > 0:
		b.WriteString("exec sleep " + strconv.Itoa(spec.sleepSecs) + "\n")
	case spec.copyStdin:
		if spec.stderrPath != "" {
			b.WriteString("cat '" + spec.stderrPath + "' >&2\n")
		}
		b.WriteString("exec cat\n")
	default:
		if spec.stdoutPath != "" {
			b.WriteString("cat '" + spec.stdoutPath + "'\n")
		}
		if spec.stderrPath != "" {
			b.WriteString("cat '" + spec.stderrPath + "' >&2\n")
		}
		b.WriteString("exit " + strconv.Itoa(spec.exitCode) + "\n")
	}
	path := filepath.Join(dir, "rclone")
	if err := os.WriteFile(path, []byte(b.String()), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake rclone: %v", err)
	}
	return path
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func countOf(ss []string, target string) int {
	n := 0
	for _, s := range ss {
		if s == target {
			n++
		}
	}
	return n
}

func indexOf(ss []string, target string) int {
	for i, s := range ss {
		if s == target {
			return i
		}
	}
	return -1
}

func containsStr(ss []string, target string) bool { return indexOf(ss, target) >= 0 }

func TestRunnerArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		configPath  string
		subcommand  string
		subflags    []string
		positionals []string
	}{
		{name: "config set, single positional", configPath: "/etc/rclone.conf", subcommand: "rcat", positionals: []string{"myremote:blobs/abc123"}},
		{name: "no config, subflag before positional", subcommand: "lsf", subflags: []string{"--max-depth", "0"}, positionals: []string{"myremote:blobs"}},
		{name: "multiple positionals all after dashdash", subcommand: "copyto", positionals: []string{"a:1", "b:2"}},
		{name: "no positionals still emits dashdash exactly once", configPath: "/c/cfg", subcommand: "version"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			argvPath := filepath.Join(dir, "argv")
			bin := writeFakeRclone(t, dir, fakeSpec{argvPath: argvPath, exitCode: 0})
			r := &runner{binary: bin, configPath: tt.configPath}

			var out bytes.Buffer
			if err := r.run(context.Background(), tt.subcommand, tt.subflags, tt.positionals, nil, &out); err != nil {
				t.Fatalf("run: %v", err)
			}
			argv := readArgv(t, argvPath)

			// exactly one `--`, sitting immediately before the positionals
			if got := countOf(argv, "--"); got != 1 {
				t.Fatalf("`--` count = %d, want 1; argv=%v", got, argv)
			}
			dd := indexOf(argv, "--")
			if want := len(argv) - len(tt.positionals) - 1; dd != want {
				t.Fatalf("`--` at %d, want %d (immediately before positionals); argv=%v", dd, want, argv)
			}
			for i, p := range tt.positionals {
				if argv[dd+1+i] != p {
					t.Fatalf("positional[%d] = %q, want %q; argv=%v", i, argv[dd+1+i], p, argv)
				}
			}

			// subcommand present, before `--`
			sc := indexOf(argv, tt.subcommand)
			if sc < 0 || sc >= dd {
				t.Fatalf("subcommand %q index=%d not before `--` (%d); argv=%v", tt.subcommand, sc, dd, argv)
			}

			// global --config placement
			if tt.configPath == "" {
				if containsStr(argv, "--config") {
					t.Fatalf("--config present with empty configPath; argv=%v", argv)
				}
			} else {
				if got := countOf(argv, "--config"); got != 1 {
					t.Fatalf("--config count = %d, want 1; argv=%v", got, argv)
				}
				if argv[0] != "--config" || argv[1] != tt.configPath {
					t.Fatalf("--config not the leading global flag with its path; argv=%v", argv)
				}
				if indexOf(argv, "--config") >= sc {
					t.Fatalf("--config not before subcommand; argv=%v", argv)
				}
			}

			// subflags sit between the subcommand and `--`
			for _, f := range tt.subflags {
				fi := indexOf(argv, f)
				if fi <= sc || fi >= dd {
					t.Fatalf("subflag %q index=%d not between subcommand(%d) and `--`(%d); argv=%v", f, fi, sc, dd, argv)
				}
			}
		})
	}
}

func TestRunnerStreaming(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		copyStdin   bool
		stdin       string
		fixedStdout string
		want        string
	}{
		{name: "stdin streamed to process and back", copyStdin: true, stdin: "hello payload \x00 binary\xff bytes", want: "hello payload \x00 binary\xff bytes"},
		{name: "empty stdin copies empty", copyStdin: true, stdin: "", want: ""},
		{name: "process stdout streamed to writer", fixedStdout: "FIXED-OUTPUT-1234", want: "FIXED-OUTPUT-1234"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			argvPath := filepath.Join(dir, "argv")
			spec := fakeSpec{argvPath: argvPath}
			if tt.copyStdin {
				spec.copyStdin = true
			} else {
				sp := filepath.Join(dir, "stdout")
				if err := os.WriteFile(sp, []byte(tt.fixedStdout), 0o600); err != nil {
					t.Fatalf("write stdout fixture: %v", err)
				}
				spec.stdoutPath = sp
			}
			bin := writeFakeRclone(t, dir, spec)
			r := &runner{binary: bin}

			var out bytes.Buffer
			var stdin *strings.Reader
			if tt.copyStdin {
				stdin = strings.NewReader(tt.stdin)
			}
			var err error
			if stdin != nil {
				err = r.run(context.Background(), "cat", nil, []string{"remote:blobs/key"}, stdin, &out)
			} else {
				err = r.run(context.Background(), "cat", nil, []string{"remote:blobs/key"}, nil, &out)
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if out.String() != tt.want {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestRunnerExitError(t *testing.T) {
	t.Parallel()
	const secretConfig = "/very/secret/rclone.conf.SECRETMARKER"
	const secretRemote = "SECRETREMOTE:blobs/deadbeef"
	tests := []struct {
		name     string
		exitCode int
		stderr   string
	}{
		{name: "exit 1 short stderr", exitCode: 1, stderr: "rclone: directory not found\n"},
		{name: "exit 7 with diagnostic", exitCode: 7, stderr: "2026/07/03 ERROR : failed to open: DIAGNOSTIC_TOKEN\n"},
		{name: "exit 3 empty stderr", exitCode: 3, stderr: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			argvPath := filepath.Join(dir, "argv")
			var stderrPath string
			if tt.stderr != "" {
				stderrPath = filepath.Join(dir, "stderr")
				if err := os.WriteFile(stderrPath, []byte(tt.stderr), 0o600); err != nil {
					t.Fatalf("write stderr fixture: %v", err)
				}
			}
			bin := writeFakeRclone(t, dir, fakeSpec{argvPath: argvPath, stderrPath: stderrPath, exitCode: tt.exitCode})
			r := &runner{binary: bin, configPath: secretConfig}

			var out bytes.Buffer
			err := r.run(context.Background(), "rcat", []string{"--fake-subflag"}, []string{secretRemote}, strings.NewReader("body"), &out)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var re *RcloneError
			if !errors.As(err, &re) {
				t.Fatalf("error type = %T, want *RcloneError; err=%v", err, err)
			}
			if re.ExitCode != tt.exitCode {
				t.Fatalf("ExitCode = %d, want %d", re.ExitCode, tt.exitCode)
			}
			if re.Subcommand != "rcat" {
				t.Fatalf("Subcommand = %q, want rcat", re.Subcommand)
			}
			if tt.stderr != "" && !strings.Contains(re.Stderr, strings.TrimRight(tt.stderr, "\n")) {
				t.Fatalf("Stderr = %q, want to contain %q", re.Stderr, strings.TrimRight(tt.stderr, "\n"))
			}

			// no secret / config / remote may appear in the message or in Args
			msg := err.Error()
			for _, secret := range []string{secretConfig, "SECRETMARKER", secretRemote, "SECRETREMOTE"} {
				if strings.Contains(msg, secret) {
					t.Fatalf("error message leaks secret %q: %q", secret, msg)
				}
				if containsStr(re.Args, secret) {
					t.Fatalf("Args leaks secret %q: %v", secret, re.Args)
				}
			}
			// the underlying exec error is unwrappable
			if errors.Unwrap(err) == nil {
				t.Fatalf("Unwrap() = nil, want the underlying exec error")
			}
		})
	}
}

func TestRunnerStderrBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")

	var sb strings.Builder
	sb.WriteString("HEAD_MARKER_SHOULD_BE_DROPPED\n")
	for sb.Len() < stderrTailBytes*2 {
		sb.WriteString("filler filler filler filler filler filler\n")
	}
	sb.WriteString("TAIL_MARKER_SHOULD_SURVIVE\n")
	stderrPath := filepath.Join(dir, "stderr")
	if err := os.WriteFile(stderrPath, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write stderr fixture: %v", err)
	}
	bin := writeFakeRclone(t, dir, fakeSpec{argvPath: argvPath, stderrPath: stderrPath, exitCode: 2})
	r := &runner{binary: bin}

	var out bytes.Buffer
	err := r.run(context.Background(), "cat", nil, []string{"remote:blobs/k"}, nil, &out)
	var re *RcloneError
	if !errors.As(err, &re) {
		t.Fatalf("want *RcloneError, got %T: %v", err, err)
	}
	if len(re.Stderr) > stderrTailBytes {
		t.Fatalf("Stderr len = %d, want <= %d", len(re.Stderr), stderrTailBytes)
	}
	if !strings.Contains(re.Stderr, "TAIL_MARKER_SHOULD_SURVIVE") {
		t.Fatalf("Stderr dropped the tail marker; got last bytes: %q", re.Stderr[max(0, len(re.Stderr)-80):])
	}
	if strings.Contains(re.Stderr, "HEAD_MARKER_SHOULD_BE_DROPPED") {
		t.Fatalf("Stderr kept the head; the bound was not applied to the tail")
	}
}

func TestRunnerContextTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		useRunnerTimeout bool
	}{
		{name: "ctx deadline kills the sleeper"},
		{name: "runner timeout kills the sleeper", useRunnerTimeout: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			argvPath := filepath.Join(dir, "argv")
			bin := writeFakeRclone(t, dir, fakeSpec{argvPath: argvPath, sleepSecs: 30})

			r := &runner{binary: bin}
			ctx := context.Background()
			if tt.useRunnerTimeout {
				r.timeout = 200 * time.Millisecond
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}

			var out bytes.Buffer
			start := time.Now()
			err := r.run(ctx, "cat", nil, []string{"remote:blobs/k"}, nil, &out)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("expected timeout error, got nil")
			}
			if elapsed > 10*time.Second {
				t.Fatalf("run took %v; ctx deadline not enforced (fake sleeps 30s)", elapsed)
			}
			var re *RcloneError
			if !errors.Is(err, context.DeadlineExceeded) && !errors.As(err, &re) {
				t.Fatalf("error = %T %v, want context.DeadlineExceeded or *RcloneError", err, err)
			}
		})
	}
}
