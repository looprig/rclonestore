package rclonestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/storage"
)

var _ storage.PathReporter = (*Store)(nil)
var _ storage.PathReporter = (*blobStore)(nil)

func canonicalExistingTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return canonical
}

func requireSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if symlinkUnavailable(err) {
			t.Skipf("symlink creation is unavailable: %v", err)
		}
		t.Fatalf("Symlink(%q, %q): %v", target, link, err)
	}
}

func TestSymlinkUnavailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "permission denied is unavailable", err: &os.LinkError{Op: "symlink", Old: "target", New: "link", Err: fs.ErrPermission}, want: true},
		{name: "missing parent is fixture failure", err: &os.LinkError{Op: "symlink", Old: "target", New: "link", Err: fs.ErrNotExist}},
		{name: "invalid path is fixture failure", err: &os.LinkError{Op: "symlink", Old: "target", New: "link", Err: fs.ErrInvalid}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := symlinkUnavailable(tt.err); got != tt.want {
				t.Errorf("symlinkUnavailable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNewStoragePaths(t *testing.T) {
	t.Parallel()

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	workingDir, err = filepath.EvalSymlinks(workingDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(working directory): %v", err)
	}

	tests := []struct {
		name string
		opts func(t *testing.T) (Options, []string)
	}{
		{
			name: "empty inline local path is current working directory",
			opts: func(t *testing.T) (Options, []string) {
				return Options{Remote: ":local:"}, []string{workingDir}
			},
		},
		{
			name: "inline local absolute root",
			opts: func(t *testing.T) (Options, []string) {
				root := t.TempDir()
				return Options{Remote: ":local:" + root}, []string{canonicalExistingTestPath(t, root)}
			},
		},
		{
			name: "inline local includes prefix",
			opts: func(t *testing.T) (Options, []string) {
				root := t.TempDir()
				canonicalRoot := canonicalExistingTestPath(t, root)
				return Options{Remote: ":local:" + root, Prefix: "blobs/v1"}, []string{filepath.Join(canonicalRoot, "blobs", "v1")}
			},
		},
		{
			name: "single-quoted parameters may contain credentials colons and commas",
			opts: func(t *testing.T) (Options, []string) {
				root := t.TempDir()
				return Options{Remote: ":local,secret='https://user:LEAKME@host/a,b':" + root}, []string{canonicalExistingTestPath(t, root)}
			},
		},
		{
			name: "double-quoted parameters support doubled quote escaping",
			opts: func(t *testing.T) (Options, []string) {
				root := t.TempDir()
				return Options{Remote: `:local,note="a:""LEAKME,b":` + root}, []string{canonicalExistingTestPath(t, root)}
			},
		},
		{
			name: "colon in local path remains after spec delimiter",
			opts: func(t *testing.T) (Options, []string) {
				root := canonicalExistingTestPath(t, t.TempDir())
				path := filepath.Join(root, "volume:part")
				return Options{Remote: ":local:" + path}, []string{path}
			},
		},
		{
			name: "non-local inline remote has no automatic path",
			opts: func(t *testing.T) (Options, []string) {
				return Options{Remote: ":s3,provider=Minio:/bucket"}, nil
			},
		},
		{
			name: "named remote has no automatic path",
			opts: func(t *testing.T) (Options, []string) {
				return Options{Remote: "named"}, nil
			},
		},
		{
			name: "named remote accepts declared path",
			opts: func(t *testing.T) (Options, []string) {
				root := t.TempDir()
				return Options{Remote: "named", Prefix: "not-appended", PersistencePaths: []string{root}}, []string{canonicalExistingTestPath(t, root)}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin := writeBlobFake(t, dir, blobFakeSpec{lsfExit: 0})
			opts, want := tt.opts(t)
			opts.Binary = bin

			s, err := New(opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := s.StoragePaths(); !reflect.DeepEqual(got, want) {
				t.Errorf("StoragePaths() = %v, want %v", got, want)
			} else if strings.Contains(strings.Join(got, ""), "LEAKME") {
				t.Fatalf("StoragePaths leaks connection parameters: %v", got)
			}
		})
	}
}

func TestNewStoragePathsCanonicalization(t *testing.T) {
	t.Parallel()

	realRoot := t.TempDir()
	canonicalRealRoot := canonicalExistingTestPath(t, realRoot)
	container := t.TempDir()
	alias := filepath.Join(container, "alias")
	requireSymlink(t, realRoot, alias)
	wantTail := filepath.Join(canonicalRealRoot, "new", "tail")
	second := filepath.Join(canonicalRealRoot, "z-second")
	dirs := []string{second, filepath.Join(alias, "new", "tail"), wantTail}

	dir := t.TempDir()
	bin := writeBlobFake(t, dir, blobFakeSpec{lsfExit: 0})
	s, err := New(Options{
		Remote:           ":local:" + filepath.Join(alias, "new"),
		Prefix:           "tail",
		PersistencePaths: dirs,
		Binary:           bin,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := []string{wantTail, second}
	if got := s.StoragePaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("StoragePaths() = %v, want sorted and deduplicated %v", got, want)
	}
}

func TestStoragePathsDefensiveCopy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	canonicalRoot := canonicalExistingTestPath(t, root)
	paths := []string{root}
	dir := t.TempDir()
	bin := writeBlobFake(t, dir, blobFakeSpec{lsfExit: 0})
	s, err := New(Options{Remote: "named", PersistencePaths: paths, Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	paths[0] = t.TempDir()
	first := s.StoragePaths()
	first[0] = t.TempDir()
	if got := s.StoragePaths(); !reflect.DeepEqual(got, []string{canonicalRoot}) {
		t.Errorf("StoragePaths() after mutations = %v, want [%s]", got, canonicalRoot)
	}
}

func TestNewPersistencePathErrors(t *testing.T) {
	t.Parallel()

	container := t.TempDir()
	missing := filepath.Join(container, "missing")
	broken := filepath.Join(container, "broken")
	requireSymlink(t, missing, broken)
	regular := filepath.Join(container, "regular")
	if err := os.WriteFile(regular, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name       string
		opts       Options
		wantUnwrap bool
		noLeak     string
	}{
		{name: "empty declared path", opts: Options{Remote: "named", PersistencePaths: []string{""}}},
		{name: "exact regular file", opts: Options{Remote: "named", PersistencePaths: []string{regular}}, wantUnwrap: true},
		{name: "broken symlink", opts: Options{Remote: "named", PersistencePaths: []string{filepath.Join(broken, "tail")}}, wantUnwrap: true},
		{name: "regular file ancestor", opts: Options{Remote: "named", PersistencePaths: []string{filepath.Join(regular, "tail")}}, wantUnwrap: true},
		{
			name:       "derived path does not leak backend parameters",
			opts:       Options{Remote: ":local,secret='https://user:LEAKME@host/a,b':" + filepath.Join(broken, "tail")},
			wantUnwrap: true,
			noLeak:     "LEAKME",
		},
	}

	fileAlias := filepath.Join(container, "file-alias")
	requireSymlink(t, regular, fileAlias)
	tests = append(tests, struct {
		name       string
		opts       Options
		wantUnwrap bool
		noLeak     string
	}{name: "exact symlink to regular file", opts: Options{Remote: "named", PersistencePaths: []string{fileAlias}}, wantUnwrap: true})

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.opts)
			var pe *PersistencePathError
			if !errors.As(err, &pe) {
				t.Fatalf("New = %v, want *PersistencePathError", err)
			}
			if tt.wantUnwrap && errors.Unwrap(pe) == nil {
				t.Error("PersistencePathError.Unwrap() = nil, want filesystem cause")
			}
			if tt.noLeak != "" && strings.Contains(err.Error(), tt.noLeak) {
				t.Fatalf("PersistencePathError leaks remote parameters: %q", err.Error())
			}
		})
	}
}

// TestNewOptionsValidation covers the pre-exec validation: a bad Remote, Prefix,
// or Timeout is rejected with a typed *OptionsError naming the offending field —
// before any binary is resolved or any subprocess is spawned.
func TestNewOptionsValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		opts      Options
		wantField string
	}{
		{name: "empty remote", opts: Options{Remote: ""}, wantField: "Remote"},
		{name: "remote with space", opts: Options{Remote: "bad remote"}, wantField: "Remote"},
		{name: "remote with slash", opts: Options{Remote: "bad/remote"}, wantField: "Remote"},
		{name: "remote with dot", opts: Options{Remote: "bad.remote"}, wantField: "Remote"},
		{name: "bare colon is not a connection string", opts: Options{Remote: ":"}, wantField: "Remote"},
		{name: "empty backend connection string", opts: Options{Remote: "::x"}, wantField: "Remote"},
		{name: "connection string missing closing colon", opts: Options{Remote: ":local"}, wantField: "Remote"},
		{name: "connection string unterminated single quote", opts: Options{Remote: ":local,secret='LEAKME:/data"}, wantField: "Remote"},
		{name: "connection string unterminated double quote", opts: Options{Remote: `:local,secret="LEAKME:/data`}, wantField: "Remote"},
		{name: "prefix leading slash", opts: Options{Remote: "myremote", Prefix: "/x"}, wantField: "Prefix"},
		{name: "prefix trailing slash", opts: Options{Remote: "myremote", Prefix: "x/"}, wantField: "Prefix"},
		{name: "prefix doubled slash", opts: Options{Remote: "myremote", Prefix: "a//b"}, wantField: "Prefix"},
		{name: "prefix dot-dot segment", opts: Options{Remote: "myremote", Prefix: "a/../b"}, wantField: "Prefix"},
		{name: "prefix leading dot-dot", opts: Options{Remote: "myremote", Prefix: "../x"}, wantField: "Prefix"},
		{name: "prefix dot segment", opts: Options{Remote: "myremote", Prefix: "a/./b"}, wantField: "Prefix"},
		{name: "prefix NUL byte", opts: Options{Remote: "myremote", Prefix: "a\x00b"}, wantField: "Prefix"},
		{name: "negative timeout", opts: Options{Remote: "myremote", Timeout: -1}, wantField: "Timeout"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.opts)
			var oe *OptionsError
			if !errors.As(err, &oe) {
				t.Fatalf("New(%+v) = %v, want *OptionsError", tt.opts, err)
			}
			if oe.Field != tt.wantField {
				t.Errorf("OptionsError.Field = %q, want %q", oe.Field, tt.wantField)
			}
		})
	}
}

// TestNewValidRemoteForms confirms every accepted remote form passes validation
// and reaches binary resolution (which then fails with *BinaryError on a bogus
// binary — proof validation did NOT reject the remote). Pairing the accept-set
// with a guaranteed-absent binary lets one table assert acceptance without a real
// rclone or a probe.
func TestNewValidRemoteForms(t *testing.T) {
	t.Parallel()
	accepted := []string{
		"myremote",
		"UPPER_and-mixed99",
		":local:",
		":local:/some/root",
		":s3,provider=Minio:",
		":s3,provider=Minio:/bucket/prefix",
		":s3,endpoint=http://minio:9000,provider=Minio:/bucket",
	}
	const absent = "rclonestore-guaranteed-absent-binary-xyz"
	for _, remote := range accepted {
		remote := remote
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{Remote: remote, Binary: absent})
			var be *BinaryError
			if !errors.As(err, &be) {
				t.Fatalf("New(remote=%q) = %v, want *BinaryError (remote accepted, binary absent)", remote, err)
			}
		})
	}
}

// TestNewBinaryNotFound checks a valid remote with an unresolvable binary yields a
// *BinaryError that names the binary and unwraps the exec.LookPath cause.
func TestNewBinaryNotFound(t *testing.T) {
	t.Parallel()
	const name = "rclonestore-guaranteed-absent-binary-xyz"
	_, err := New(Options{Remote: "myremote", Binary: name})
	var be *BinaryError
	if !errors.As(err, &be) {
		t.Fatalf("New = %v, want *BinaryError", err)
	}
	if be.Binary != name {
		t.Errorf("BinaryError.Binary = %q, want %q", be.Binary, name)
	}
	if errors.Unwrap(err) == nil {
		t.Errorf("BinaryError.Unwrap() = nil, want the exec.LookPath cause")
	}
}

// TestNewOptionsErrorNoLeak asserts a malformed connection-string Remote (which can
// embed credentials) never has its value echoed into the OptionsError message.
func TestNewOptionsErrorNoLeak(t *testing.T) {
	t.Parallel()
	tests := []string{
		":s3,secret_access_key=LEAKME",
		":local,secret='https://user:LEAKME@host/path",
		`:local,secret="https://user:LEAKME@host/path`,
	}
	for _, remote := range tests {
		remote := remote
		t.Run(remote[:6], func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{Remote: remote})
			var oe *OptionsError
			if !errors.As(err, &oe) {
				t.Fatalf("New = %v, want *OptionsError", err)
			}
			if strings.Contains(err.Error(), "LEAKME") {
				t.Fatalf("OptionsError leaks the remote value: %q", err.Error())
			}
		})
	}
}

// TestNewProbe exercises the startup reachability probe through a fake rclone. A
// reachable root (exit 0) or a benign not-found root (exit 3/4) succeeds; any other
// failure is a *ProbeError wrapping the credential-safe *RcloneError. Binary is set
// to the absolute fake path, which exec.LookPath resolves directly.
func TestNewProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		spec     blobFakeSpec
		remote   string
		wantErr  bool // expect *ProbeError
		wantExit int  // wrapped RcloneError exit code when wantErr
	}{
		{name: "reachable empty root (exit 0)", spec: blobFakeSpec{lsfExit: 0}, remote: "myremote"},
		{name: "root not found (exit 3) is reachable-but-empty", spec: blobFakeSpec{lsfExit: exitDirNotFound}, remote: "myremote"},
		{name: "object not found (exit 4) is reachable-but-empty", spec: blobFakeSpec{lsfExit: exitFileNotFound}, remote: "myremote"},
		{name: "connection-string remote reachable", spec: blobFakeSpec{lsfExit: 0}, remote: ":local:/tmp/x"},
		{name: "unreachable (exit 5) is ProbeError", spec: blobFakeSpec{lsfExit: 5}, remote: ":local:/tmp/SECRETROOT", wantErr: true, wantExit: 5},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin := writeBlobFake(t, dir, tt.spec)

			s, err := New(Options{Remote: tt.remote, Binary: bin})
			if tt.wantErr {
				var pe *ProbeError
				if !errors.As(err, &pe) {
					t.Fatalf("New = %v, want *ProbeError", err)
				}
				var re *RcloneError
				if !errors.As(err, &re) {
					t.Fatalf("ProbeError does not unwrap to *RcloneError: %v", err)
				}
				if re.ExitCode != tt.wantExit {
					t.Errorf("wrapped RcloneError.ExitCode = %d, want %d", re.ExitCode, tt.wantExit)
				}
				// The remote (a positional) must never reach the error message.
				if strings.Contains(err.Error(), "SECRETROOT") {
					t.Fatalf("ProbeError leaks the remote path: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("New = %v, want success", err)
			}
			var _ storage.Blobs = s
			if cerr := s.Close(); cerr != nil {
				t.Errorf("Close() = %v, want nil", cerr)
			}
			// The probe listed the remote root with `--max-depth 0` before `--`.
			argv, ok := argvOf(t, dir, "lsf")
			if !ok {
				t.Fatalf("probe did not invoke lsf")
			}
			if !containsStr(argv, "--max-depth") || !containsStr(argv, "0") {
				t.Errorf("probe lsf missing --max-depth 0; argv=%v", argv)
			}
			assertDashDashBeforePositional(t, argv, newBlobStore(nil, tt.remote, "", nil).listRoot())
		})
	}
}

// TestNewStoreRoutesThroughBlobs proves the returned *Store promotes the blobStore
// methods: after a successful probe, Get routes to the fake's cat and streams its
// bytes back.
func TestNewStoreRoutesThroughBlobs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := []byte("promoted \x00\xff bytes")
	bin := writeBlobFake(t, dir, blobFakeSpec{lsfExit: 0, catOut: want})

	s, err := New(Options{Remote: "myremote", Binary: bin})
	if err != nil {
		t.Fatalf("New = %v, want success", err)
	}
	rc, err := s.Get(context.Background(), "blobs/k")
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if cerr := rc.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get = %q, want %q", got, want)
	}
}

// TestLookPathResolvesAbsoluteFake documents the invariant the probe tests rely on:
// exec.LookPath resolves an absolute path to an executable file directly (no PATH
// search), so tests can hand New the absolute fake-rclone path via Options.Binary.
func TestLookPathResolvesAbsoluteFake(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := writeBlobFake(t, dir, blobFakeSpec{})
	resolved, err := exec.LookPath(bin)
	if err != nil {
		t.Fatalf("LookPath(%q) = %v, want the absolute path resolved", bin, err)
	}
	if resolved != bin {
		t.Errorf("LookPath = %q, want %q", resolved, bin)
	}
}
