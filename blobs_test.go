package rclonestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ciram-co/storekit"
)

// blobFakeSpec bakes the per-subcommand behavior of a stand-in rclone into a
// generated #!/bin/sh script. Each test gets its own t.TempDir() and its own
// script, so behavior is fixed per test (no shared env) and the suite is
// parallel-safe. The script records the exact argv of every invocation to
// argv.<subcommand> and captures rcat's stdin to rcat.stdin, so a test can assert
// which subcommands ran, the positionals (and `--` placement), and the streamed
// body. Binary-unsafe payloads (NUL bytes) are cat'd from fixture files rather than
// interpolated into the script.
type blobFakeSpec struct {
	// lsf: existence probe (Put) and recursive listing (List) both dispatch here.
	lsfOut  string // stdout emitted for lsf (a leaf line = present; a listing for List)
	lsfExit int    // lsf exit status (3/4 = not-found → absent/empty)

	// cat: existing-object read (Put present branch) and Get.
	catOut  []byte // stdout bytes emitted for cat (binary-safe)
	catErr  string // stderr emitted for cat
	catExit int    // cat exit status

	// deletefile.
	delErr  string // stderr emitted for deletefile
	delExit int    // deletefile exit status
}

// writeBlobFake generates the fake rclone script in dir and returns its path.
func writeBlobFake(t *testing.T, dir string, spec blobFakeSpec) string {
	t.Helper()

	catOutPath := filepath.Join(dir, "cat.out")
	if err := os.WriteFile(catOutPath, spec.catOut, 0o600); err != nil {
		t.Fatalf("write cat.out fixture: %v", err)
	}

	// Single-quote a value for safe embedding in the /bin/sh script.
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("sub=\"$1\"\n")
	b.WriteString("printf '%s\\n' \"$@\" > " + q(dir) + "/\"argv.$sub\"\n")
	b.WriteString("case \"$sub\" in\n")

	b.WriteString("lsf)\n")
	b.WriteString("printf '%s' " + q(spec.lsfOut) + "\n")
	b.WriteString("exit " + strconv.Itoa(spec.lsfExit) + "\n;;\n")

	b.WriteString("cat)\n")
	b.WriteString("cat " + q(catOutPath) + "\n")
	if spec.catErr != "" {
		b.WriteString("printf '%s' " + q(spec.catErr) + " >&2\n")
	}
	b.WriteString("exit " + strconv.Itoa(spec.catExit) + "\n;;\n")

	b.WriteString("rcat)\n")
	b.WriteString("cat > " + q(dir) + "/rcat.stdin\n")
	b.WriteString("exit 0\n;;\n")

	b.WriteString("deletefile)\n")
	if spec.delErr != "" {
		b.WriteString("printf '%s' " + q(spec.delErr) + " >&2\n")
	}
	b.WriteString("exit " + strconv.Itoa(spec.delExit) + "\n;;\n")

	b.WriteString("*)\nexit 99\n;;\nesac\n")

	path := filepath.Join(dir, "rclone")
	if err := os.WriteFile(path, []byte(b.String()), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake rclone: %v", err)
	}
	return path
}

// argvOf reads the recorded argv for a subcommand; ok is false if the subcommand
// was never invoked (its argv file was not written).
func argvOf(t *testing.T, dir, sub string) (argv []string, ok bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "argv."+sub)) //nolint:gosec // test fixture path
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read argv.%s: %v", sub, err)
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil, true
	}
	return strings.Split(s, "\n"), true
}

// assertDashDashBeforePositional verifies the recorded argv contains exactly one
// `--` and that want is the immediately-following positional — the core security
// invariant (a key can never be read as a flag), asserted per invocation.
func assertDashDashBeforePositional(t *testing.T, argv []string, wantPositional string) {
	t.Helper()
	if got := countOf(argv, "--"); got != 1 {
		t.Fatalf("`--` count = %d, want 1; argv=%v", got, argv)
	}
	dd := indexOf(argv, "--")
	if dd+1 >= len(argv) || argv[dd+1] != wantPositional {
		t.Fatalf("positional after `--` = %v, want %q; argv=%v", argv[dd+1:], wantPositional, argv)
	}
}

// newTestBlobStoreWith wires a blobStore over a runner pointed at a freshly
// generated fake rclone in a fresh temp dir, for the given remote and prefix,
// returning the store and the dir (for argv/stdin assertions). No config path is
// set, so no --config appears in any argv.
func newTestBlobStoreWith(t *testing.T, spec blobFakeSpec, remote, prefix string) (*blobStore, string) {
	t.Helper()
	dir := t.TempDir()
	bin := writeBlobFake(t, dir, spec)
	return newBlobStore(&runner{binary: bin}, remote, prefix), dir
}

// newTestBlobStore is newTestBlobStoreWith for the default named-remote fixture
// ("remote", prefix "pfx"), used by the bulk of the behavioral tests.
func newTestBlobStore(t *testing.T, spec blobFakeSpec) (*blobStore, string) {
	t.Helper()
	return newTestBlobStoreWith(t, spec, "remote", "pfx")
}

func TestBlobStoreImplementsBlobs(t *testing.T) {
	t.Parallel()
	var _ storekit.Blobs = (*blobStore)(nil)
}

// TestObjectPathForms pins the remote-form-aware path construction for BOTH remote
// forms and the empty-prefix collapse: a named remote joins its path with a colon
// ("R:<path>"), a ":backend:" connection string joins with a slash
// ("<remote>/<path>"), and an empty store prefix introduces no spurious slash.
func TestObjectPathForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		remote   string
		prefix   string
		key      string
		wantObj  string
		wantRoot string
	}{
		{
			name: "named remote with prefix", remote: "myremote", prefix: "pfx", key: "blobs/abc",
			wantObj: "myremote:pfx/blobs/abc", wantRoot: "myremote:pfx",
		},
		{
			name: "named remote empty prefix collapses (no spurious slash)", remote: "myremote", prefix: "", key: "blobs/abc",
			wantObj: "myremote:blobs/abc", wantRoot: "myremote:",
		},
		{
			name: "connection string with path and prefix", remote: ":local:/tmp/x", prefix: "pfx", key: "blobs/abc",
			wantObj: ":local:/tmp/x/pfx/blobs/abc", wantRoot: ":local:/tmp/x/pfx",
		},
		{
			name: "connection string empty prefix appends with slash", remote: ":local:/tmp/x", prefix: "", key: "blobs/abc",
			wantObj: ":local:/tmp/x/blobs/abc", wantRoot: ":local:/tmp/x",
		},
		{
			name: "bare backend connection string empty prefix", remote: ":local:", prefix: "", key: "blobs/abc",
			wantObj: ":local:/blobs/abc", wantRoot: ":local:",
		},
		{
			name: "s3-style connection string with params", remote: ":s3,provider=Minio:", prefix: "snaps", key: "x/y",
			wantObj: ":s3,provider=Minio:/snaps/x/y", wantRoot: ":s3,provider=Minio:/snaps",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := newBlobStore(nil, tt.remote, tt.prefix)
			if got := b.objectPath(tt.key); got != tt.wantObj {
				t.Errorf("objectPath(%q) = %q, want %q", tt.key, got, tt.wantObj)
			}
			if got := b.listRoot(); got != tt.wantRoot {
				t.Errorf("listRoot() = %q, want %q", got, tt.wantRoot)
			}
		})
	}
}

// TestBlobsConnStringPositional drives every verb through a fake rclone with a
// connection-string remote and asserts the actual `--`-guarded positional is the
// correctly slash-joined path — the end-to-end proof of the remote-form fix (a
// hardcoded colon would produce the invalid ":local:/tmp/root:pfx/blobs/k").
func TestBlobsConnStringPositional(t *testing.T) {
	t.Parallel()
	const (
		remote  = ":local:/tmp/root"
		prefix  = "pfx"
		key     = "blobs/k"
		wantObj = ":local:/tmp/root/pfx/blobs/k"
		wantRt  = ":local:/tmp/root/pfx"
	)

	t.Run("Get object path", func(t *testing.T) {
		t.Parallel()
		b, dir := newTestBlobStoreWith(t, blobFakeSpec{catOut: []byte("x")}, remote, prefix)
		rc, err := b.Get(context.Background(), key)
		requireNoErr(t, err)
		if cerr := rc.Close(); cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
		argv, ok := argvOf(t, dir, "cat")
		if !ok {
			t.Fatalf("cat not invoked")
		}
		assertDashDashBeforePositional(t, argv, wantObj)
	})

	t.Run("Put probe and rcat object path", func(t *testing.T) {
		t.Parallel()
		b, dir := newTestBlobStoreWith(t, blobFakeSpec{lsfExit: exitDirNotFound}, remote, prefix)
		requireNoErr(t, b.Put(context.Background(), key, bytes.NewReader([]byte("body"))))
		lsfArgv, ok := argvOf(t, dir, "lsf")
		if !ok {
			t.Fatalf("lsf not invoked")
		}
		assertDashDashBeforePositional(t, lsfArgv, wantObj)
		rcatArgv, ok := argvOf(t, dir, "rcat")
		if !ok {
			t.Fatalf("rcat not invoked")
		}
		assertDashDashBeforePositional(t, rcatArgv, wantObj)
	})

	t.Run("Delete object path", func(t *testing.T) {
		t.Parallel()
		b, dir := newTestBlobStoreWith(t, blobFakeSpec{delExit: 0}, remote, prefix)
		requireNoErr(t, b.Delete(context.Background(), key))
		argv, ok := argvOf(t, dir, "deletefile")
		if !ok {
			t.Fatalf("deletefile not invoked")
		}
		assertDashDashBeforePositional(t, argv, wantObj)
	})

	t.Run("List root", func(t *testing.T) {
		t.Parallel()
		b, dir := newTestBlobStoreWith(t, blobFakeSpec{lsfOut: "blobs/a\n"}, remote, prefix)
		_, err := b.List(context.Background(), "")
		requireNoErr(t, err)
		argv, ok := argvOf(t, dir, "lsf")
		if !ok {
			t.Fatalf("lsf not invoked")
		}
		assertDashDashBeforePositional(t, argv, wantRt)
	})
}

func TestBlobsPut(t *testing.T) {
	t.Parallel()
	const key = "blobs/abc123"
	const wantPath = "remote:pfx/blobs/abc123"
	content := []byte("content-addressed bytes \x00\xff end")

	tests := []struct {
		name       string
		spec       blobFakeSpec
		body       []byte
		wantErr    func(t *testing.T, err error)
		wantRcat   bool   // rcat must have been invoked
		wantStdin  []byte // expected captured rcat stdin (when wantRcat)
		wantNoRcat bool   // rcat must NOT have been invoked
	}{
		{
			name:      "absent streams via rcat",
			spec:      blobFakeSpec{lsfExit: exitDirNotFound}, // lsf: object not present
			body:      content,
			wantErr:   func(t *testing.T, err error) { requireNoErr(t, err) },
			wantRcat:  true,
			wantStdin: content,
		},
		{
			name:      "absent via empty lsf output still streams",
			spec:      blobFakeSpec{lsfOut: "", lsfExit: 0}, // present exit but empty listing = absent
			body:      content,
			wantErr:   func(t *testing.T, err error) { requireNoErr(t, err) },
			wantRcat:  true,
			wantStdin: content,
		},
		{
			name:       "present identical is a no-op success",
			spec:       blobFakeSpec{lsfOut: "abc123\n", catOut: content},
			body:       content,
			wantErr:    func(t *testing.T, err error) { requireNoErr(t, err) },
			wantNoRcat: true,
		},
		{
			name:       "present different conflicts and does not upload",
			spec:       blobFakeSpec{lsfOut: "abc123\n", catOut: []byte("original bytes")},
			body:       []byte("different bytes"),
			wantErr:    func(t *testing.T, err error) { requireBlobConflict(t, err, key) },
			wantNoRcat: true,
		},
		{
			name:       "present empty-vs-nonempty conflicts",
			spec:       blobFakeSpec{lsfOut: "abc123\n", catOut: []byte("original")},
			body:       []byte{},
			wantErr:    func(t *testing.T, err error) { requireBlobConflict(t, err, key) },
			wantNoRcat: true,
		},
		{
			name:    "lsf probe hard-error propagates (not swallowed as absent)",
			spec:    blobFakeSpec{lsfExit: 5, lsfOut: ""}, // exit 5 = temporary error, not not-found
			body:    content,
			wantErr: func(t *testing.T, err error) { requireRcloneExit(t, err, 5) },
			// no rcat, no cat: a non-not-found probe error must abort Put.
			wantNoRcat: true,
		},
		{
			name: "present cat hard-error propagates (not swallowed)",
			// Present per lsf, but reading the existing object hard-fails (exit 5).
			// The present branch must propagate it, never re-interpret it or upload.
			spec:       blobFakeSpec{lsfOut: "abc123\n", catExit: 5, catErr: "connection refused\n"},
			body:       content,
			wantErr:    func(t *testing.T, err error) { requireRcloneExit(t, err, 5) },
			wantNoRcat: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, dir := newTestBlobStore(t, tt.spec)

			err := b.Put(context.Background(), key, bytes.NewReader(tt.body))
			tt.wantErr(t, err)

			// The lsf existence probe always runs first, with `--` before the path.
			lsfArgv, ok := argvOf(t, dir, "lsf")
			if !ok {
				t.Fatalf("lsf probe was not invoked")
			}
			assertDashDashBeforePositional(t, lsfArgv, wantPath)

			rcatArgv, rcatRan := argvOf(t, dir, "rcat")
			switch {
			case tt.wantRcat:
				if !rcatRan {
					t.Fatalf("rcat was not invoked")
				}
				assertDashDashBeforePositional(t, rcatArgv, wantPath)
				gotStdin, rerr := os.ReadFile(filepath.Join(dir, "rcat.stdin"))
				if rerr != nil {
					t.Fatalf("read rcat.stdin: %v", rerr)
				}
				if !bytes.Equal(gotStdin, tt.wantStdin) {
					t.Fatalf("rcat stdin = %q, want %q", gotStdin, tt.wantStdin)
				}
			case tt.wantNoRcat:
				if rcatRan {
					t.Fatalf("rcat was invoked but must not have been; argv=%v", rcatArgv)
				}
			}
		})
	}
}

func TestBlobsGet(t *testing.T) {
	t.Parallel()
	const key = "blobs/get"
	const wantPath = "remote:pfx/blobs/get"

	tests := []struct {
		name    string
		spec    blobFakeSpec
		want    []byte
		wantErr func(t *testing.T, err error)
	}{
		{
			name: "present streams bytes",
			spec: blobFakeSpec{catOut: []byte("streamed \x00\xff bytes")},
			want: []byte("streamed \x00\xff bytes"),
		},
		{
			name: "present empty object",
			spec: blobFakeSpec{catOut: []byte{}},
			want: []byte{},
		},
		{
			name:    "absent via exit 4 maps to BlobNotFoundError",
			spec:    blobFakeSpec{catExit: exitFileNotFound, catErr: "2026/07/03 ERROR : object not found\n"},
			wantErr: func(t *testing.T, err error) { requireBlobNotFound(t, err, key) },
		},
		{
			name:    "absent via exit 3 maps to BlobNotFoundError",
			spec:    blobFakeSpec{catExit: exitDirNotFound, catErr: "directory not found\n"},
			wantErr: func(t *testing.T, err error) { requireBlobNotFound(t, err, key) },
		},
		{
			name:    "absent via stderr marker under non-standard exit code",
			spec:    blobFakeSpec{catExit: 1, catErr: "Failed to open: object not found\n"},
			wantErr: func(t *testing.T, err error) { requireBlobNotFound(t, err, key) },
		},
		{
			name:    "genuine error propagates as RcloneError",
			spec:    blobFakeSpec{catExit: 5, catErr: "connection refused\n"},
			wantErr: func(t *testing.T, err error) { requireRcloneExit(t, err, 5) },
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, dir := newTestBlobStore(t, tt.spec)

			rc, err := b.Get(context.Background(), key)
			if tt.wantErr != nil {
				tt.wantErr(t, err)
				return
			}
			requireNoErr(t, err)
			got, rerr := io.ReadAll(rc)
			if rerr != nil {
				t.Fatalf("ReadAll: %v", rerr)
			}
			if cerr := rc.Close(); cerr != nil {
				t.Fatalf("Close: %v", cerr)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("Get = %q, want %q", got, tt.want)
			}
			catArgv, ok := argvOf(t, dir, "cat")
			if !ok {
				t.Fatalf("cat was not invoked")
			}
			assertDashDashBeforePositional(t, catArgv, wantPath)
		})
	}
}

func TestBlobsDelete(t *testing.T) {
	t.Parallel()
	const key = "blobs/del"
	const wantPath = "remote:pfx/blobs/del"

	tests := []struct {
		name    string
		spec    blobFakeSpec
		wantErr func(t *testing.T, err error)
	}{
		{
			name:    "existing deletes cleanly",
			spec:    blobFakeSpec{delExit: 0},
			wantErr: func(t *testing.T, err error) { requireNoErr(t, err) },
		},
		{
			name:    "absent via exit 4 is idempotent nil",
			spec:    blobFakeSpec{delExit: exitFileNotFound, delErr: "object not found\n"},
			wantErr: func(t *testing.T, err error) { requireNoErr(t, err) },
		},
		{
			name:    "absent via stderr marker is idempotent nil",
			spec:    blobFakeSpec{delExit: 1, delErr: "Couldn't delete: object not found\n"},
			wantErr: func(t *testing.T, err error) { requireNoErr(t, err) },
		},
		{
			name:    "genuine error propagates",
			spec:    blobFakeSpec{delExit: 5, delErr: "connection refused\n"},
			wantErr: func(t *testing.T, err error) { requireRcloneExit(t, err, 5) },
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, dir := newTestBlobStore(t, tt.spec)

			err := b.Delete(context.Background(), key)
			tt.wantErr(t, err)

			delArgv, ok := argvOf(t, dir, "deletefile")
			if !ok {
				t.Fatalf("deletefile was not invoked")
			}
			assertDashDashBeforePositional(t, delArgv, wantPath)
		})
	}
}

func TestBlobsList(t *testing.T) {
	t.Parallel()
	const wantRoot = "remote:pfx"

	tests := []struct {
		name    string
		spec    blobFakeSpec
		prefix  string
		want    []string
		wantErr func(t *testing.T, err error)
	}{
		{
			name:   "unsorted with duplicates sorted and deduped",
			spec:   blobFakeSpec{lsfOut: "blobs/c\nblobs/a\nsnaps/z\nblobs/b\nblobs/a\n"},
			prefix: "",
			want:   []string{"blobs/a", "blobs/b", "blobs/c", "snaps/z"},
		},
		{
			name:   "prefix filters then sorts",
			spec:   blobFakeSpec{lsfOut: "snaps/z\nblobs/c\nblobs/a\nblobs/b\n"},
			prefix: "blobs/",
			want:   []string{"blobs/a", "blobs/b", "blobs/c"},
		},
		{
			name:   "prefix matching nothing is empty",
			spec:   blobFakeSpec{lsfOut: "blobs/a\nblobs/b\n"},
			prefix: "snaps/",
			want:   nil,
		},
		{
			name:   "blank lines and CR are ignored",
			spec:   blobFakeSpec{lsfOut: "blobs/a\r\n\nblobs/b\r\n"},
			prefix: "",
			want:   []string{"blobs/a", "blobs/b"},
		},
		{
			name:   "empty store via not-found root is empty",
			spec:   blobFakeSpec{lsfExit: exitDirNotFound, lsfOut: ""},
			prefix: "",
			want:   nil,
		},
		{
			name:   "empty store via empty output is empty",
			spec:   blobFakeSpec{lsfExit: 0, lsfOut: ""},
			prefix: "",
			want:   nil,
		},
		{
			name:    "genuine listing error propagates",
			spec:    blobFakeSpec{lsfExit: 5, lsfOut: ""},
			prefix:  "",
			wantErr: func(t *testing.T, err error) { requireRcloneExit(t, err, 5) },
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, dir := newTestBlobStore(t, tt.spec)

			got, err := b.List(context.Background(), tt.prefix)
			if tt.wantErr != nil {
				tt.wantErr(t, err)
				return
			}
			requireNoErr(t, err)
			if !equalStrings(got, tt.want) {
				t.Fatalf("List(%q) = %v, want %v", tt.prefix, got, tt.want)
			}

			lsfArgv, ok := argvOf(t, dir, "lsf")
			if !ok {
				t.Fatalf("lsf was not invoked")
			}
			assertDashDashBeforePositional(t, lsfArgv, wantRoot)
			if !containsStr(lsfArgv, "-R") {
				t.Fatalf("List lsf missing -R; argv=%v", lsfArgv)
			}
			if !containsStr(lsfArgv, "--files-only") {
				t.Fatalf("List lsf missing --files-only; argv=%v", lsfArgv)
			}
		})
	}
}

// TestBlobsInvalidKeyNeverExecs asserts that every write/read method validates the
// key with storekit.ValidateName BEFORE any subprocess runs: an invalid key yields
// *storekit.InvalidNameError and the fake rclone is never invoked (no argv.* file).
func TestBlobsInvalidKeyNeverExecs(t *testing.T) {
	t.Parallel()

	invalid := []struct {
		label string
		value string
	}{
		{"empty", ""},
		{"leading slash", "/leading"},
		{"trailing slash", "trailing/"},
		{"doubled slash", "a//b"},
		{"uppercase", "Upper"},
		{"space", "has space"},
		{"dot-dot", ".."},
		{"dash-leading segment (flag-looking)", "-rf"},
	}
	methods := []struct {
		name string
		call func(b *blobStore, key string) error
	}{
		{"Put", func(b *blobStore, key string) error {
			return b.Put(context.Background(), key, bytes.NewReader([]byte("x")))
		}},
		{"Get", func(b *blobStore, key string) error {
			_, err := b.Get(context.Background(), key)
			return err
		}},
		{"Delete", func(b *blobStore, key string) error {
			return b.Delete(context.Background(), key)
		}},
	}

	for _, m := range methods {
		for _, bad := range invalid {
			m, bad := m, bad
			t.Run(m.name+"/"+bad.label, func(t *testing.T) {
				t.Parallel()
				b, dir := newTestBlobStore(t, blobFakeSpec{})

				err := m.call(b, bad.value)
				var ine *storekit.InvalidNameError
				if !errors.As(err, &ine) {
					t.Fatalf("%s(%q) = %v, want *InvalidNameError", m.name, bad.value, err)
				}
				if ine.Name != bad.value {
					t.Fatalf("InvalidNameError.Name = %q, want %q", ine.Name, bad.value)
				}
				for _, sub := range []string{"lsf", "cat", "rcat", "deletefile"} {
					if _, ran := argvOf(t, dir, sub); ran {
						t.Fatalf("%s(%q) invoked rclone %s before validation", m.name, bad.value, sub)
					}
				}
			})
		}
	}
}

// TestBlobsListPrefixNotValidated confirms List does not name-validate its prefix:
// a grammar-violating prefix is a legal filter, not an error.
func TestBlobsListPrefixNotValidated(t *testing.T) {
	t.Parallel()
	b, _ := newTestBlobStore(t, blobFakeSpec{lsfOut: "blobs/a\nblobs/b\n"})
	got, err := b.List(context.Background(), "Bad//Prefix..")
	if err != nil {
		t.Fatalf("List(invalid-looking prefix) = %v, want nil error", err)
	}
	if got != nil {
		t.Fatalf("List = %v, want nil (nothing matches the odd prefix)", got)
	}
}

func TestBlobsPutSourceError(t *testing.T) {
	t.Parallel()
	// Present object (lsf says present, cat returns bytes) so Put takes the
	// buffering branch and reads the caller's reader — which fails mid-stream.
	b, _ := newTestBlobStore(t, blobFakeSpec{lsfOut: "k\n", catOut: []byte("existing")})

	const key = "blobs/src"
	err := b.Put(context.Background(), key, &failingReader{})
	var pse *PutSourceError
	if !errors.As(err, &pse) {
		t.Fatalf("Put(failing reader) = %v, want *PutSourceError", err)
	}
	if pse.Key != key {
		t.Fatalf("PutSourceError.Key = %q, want %q", pse.Key, key)
	}
	if !errors.Is(err, errReadFail) {
		t.Fatalf("PutSourceError does not unwrap the read cause")
	}
}

var errReadFail = errors.New("read boom")

type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) { return 0, errReadFail }

// --- shared test assertions -------------------------------------------------

func requireNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requireBlobConflict(t *testing.T, err error, key string) {
	t.Helper()
	var bc *storekit.BlobConflictError
	if !errors.As(err, &bc) {
		t.Fatalf("error = %v, want *BlobConflictError", err)
	}
	if bc.Key != key {
		t.Fatalf("BlobConflictError.Key = %q, want %q", bc.Key, key)
	}
}

func requireBlobNotFound(t *testing.T, err error, key string) {
	t.Helper()
	var nf *storekit.BlobNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error = %v, want *BlobNotFoundError", err)
	}
	if nf.Key != key {
		t.Fatalf("BlobNotFoundError.Key = %q, want %q", nf.Key, key)
	}
}

func requireRcloneExit(t *testing.T, err error, wantExit int) {
	t.Helper()
	var re *RcloneError
	if !errors.As(err, &re) {
		t.Fatalf("error = %T %v, want *RcloneError", err, err)
	}
	if re.ExitCode != wantExit {
		t.Fatalf("RcloneError.ExitCode = %d, want %d", re.ExitCode, wantExit)
	}
}

// equalStrings treats nil and empty as equal (List may return either for an empty
// result) and asserts order — so it checks sorted+dedup+membership in one shot.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
