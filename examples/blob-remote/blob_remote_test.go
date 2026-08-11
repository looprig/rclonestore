//go:build !windows

// Package blobremote_test contains a deterministic public-API example. Its fake
// rclone is a POSIX-shell fixture for offline tests on Unix runners; the
// rclonestore package itself is not limited to Unix or to this fixture.
package blobremote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/rclonestore"
	"github.com/looprig/storage"
)

const fakeRclone = `#!/bin/sh
set -eu

fail() {
  echo "fake rclone: $*" >&2
  exit 64
}

[ "$#" -ge 5 ] || fail "too few arguments"
[ "$1" = "--config" ] || fail "missing --config"
[ "$2" = "$FAKE_RCLONE_CONFIG" ] || fail "unexpected config path"
shift 2
subcommand=$1
shift

live="$FAKE_RCLONE_LIVE/$$"
: > "$live"
trap 'rm -f "$live"' 0 1 2 3 15

record() {
  printf '%s\n' "$*" >> "$FAKE_RCLONE_LOG"
}

object_file() {
  case "$1" in
    docs-remote:examples/*)
      relative=${1#docs-remote:examples/}
      ;;
    *) fail "unexpected object path" ;;
  esac
  printf '%s/%s\n' "$FAKE_RCLONE_STATE" "$relative"
}

case "$subcommand" in
  lsf)
    if [ "$#" -eq 4 ] && [ "$1" = "--max-depth" ] && [ "$2" = "0" ] && [ "$3" = "--" ] && [ "$4" = "docs-remote:examples" ]; then
      record "lsf --max-depth 0 -- docs-remote:examples"
      exit 0
    fi
    if [ "$#" -eq 3 ] && [ "$1" = "--files-only" ] && [ "$2" = "--" ]; then
      file=$(object_file "$3")
      record "lsf --files-only -- $3"
      if [ -f "$file" ]; then
        basename "$file"
        exit 0
      fi
      echo "not found" >&2
      exit 4
    fi
    if [ "$#" -eq 4 ] && [ "$1" = "--files-only" ] && [ "$2" = "-R" ] && [ "$3" = "--" ] && [ "$4" = "docs-remote:examples" ]; then
      record "lsf --files-only -R -- docs-remote:examples"
      (cd "$FAKE_RCLONE_STATE" && find . -type f -print | sed 's|^\./||' | sort -r)
      exit 0
    fi
    fail "unexpected lsf framing"
    ;;
  rcat)
    [ "$#" -eq 2 ] && [ "$1" = "--" ] || fail "unexpected rcat framing"
    file=$(object_file "$2")
    record "rcat -- $2"
    mkdir -p "$(dirname "$file")"
    cat > "$file"
    ;;
  cat)
    [ "$#" -eq 2 ] && [ "$1" = "--" ] || fail "unexpected cat framing"
    file=$(object_file "$2")
    record "cat -- $2"
    if [ ! -f "$file" ]; then
      echo "not found" >&2
      exit 4
    fi
    cat "$file"
    ;;
  deletefile)
    [ "$#" -eq 2 ] && [ "$1" = "--" ] || fail "unexpected deletefile framing"
    file=$(object_file "$2")
    record "deletefile -- $2"
    if [ ! -f "$file" ]; then
      echo "not found" >&2
      exit 4
    fi
    rm "$file"
    ;;
  *) fail "unexpected subcommand" ;;
esac
`

// TestExampleBlobRemote demonstrates the full supported blob lifecycle using
// only exported APIs and a deterministic rclone subprocess. Production uses a
// real rclone executable and remote; this fixture intentionally requires none.
func TestExampleBlobRemote(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stateDir := filepath.Join(root, "fake-remote")
	liveDir := filepath.Join(root, "live-processes")
	declaredPath := filepath.Join(root, "declared-persistence")
	for _, path := range []string{stateDir, liveDir, declaredPath} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
	}

	binaryPath := filepath.Join(root, "rclone")
	if err := os.WriteFile(binaryPath, []byte(fakeRclone), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatalf("write fake rclone: %v", err)
	}
	configPath := filepath.Join(root, "rclone.conf")
	if err := os.WriteFile(configPath, []byte("fixture only; rclonestore does not read this file\n"), 0o600); err != nil {
		t.Fatalf("write fake config: %v", err)
	}
	logPath := filepath.Join(root, "rclone.log")
	t.Setenv("FAKE_RCLONE_CONFIG", configPath)
	t.Setenv("FAKE_RCLONE_STATE", stateDir)
	t.Setenv("FAKE_RCLONE_LIVE", liveDir)
	t.Setenv("FAKE_RCLONE_LOG", logPath)

	// Timeout applies independently to the startup probe and every later call.
	// ConfigPath is forwarded to rclone but never opened by rclonestore. A named
	// remote is opaque, so local persistence roots must be declared explicitly.
	store, err := rclonestore.New(rclonestore.Options{
		Remote:           "docs-remote",
		Prefix:           "examples",
		Binary:           binaryPath,
		ConfigPath:       configPath,
		Timeout:          15 * time.Second,
		PersistencePaths: []string{declaredPath},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	canonicalPath, err := filepath.EvalSymlinks(declaredPath)
	if err != nil {
		t.Fatalf("resolve persistence path: %v", err)
	}
	if got := store.StoragePaths(); !reflect.DeepEqual(got, []string{canonicalPath}) {
		t.Fatalf("StoragePaths = %v, want [%s]", got, canonicalPath)
	}

	const alphaKey = "blobs/alpha"
	const alphaBody = "alpha payload"
	if err := store.Put(ctx, alphaKey, strings.NewReader(alphaBody)); err != nil {
		t.Fatalf("Put absent blob: %v", err)
	}
	// Put is immutable and only conditionally safe: callers must provide the
	// documented external single-writer guarantee around its probe/write sequence.
	if err := store.Put(ctx, alphaKey, strings.NewReader(alphaBody)); err != nil {
		t.Fatalf("Put identical blob: %v", err)
	}
	err = store.Put(ctx, alphaKey, strings.NewReader("different payload"))
	var conflict *storage.BlobConflictError
	if !errors.As(err, &conflict) || conflict.Key != alphaKey {
		t.Fatalf("Put conflicting blob = %v, want BlobConflictError for %q", err, alphaKey)
	}

	// Get buffers the subprocess output so errors are reported by Get itself. The
	// returned reader is caller-owned and must be closed.
	reader, err := store.Get(ctx, alphaKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close blob reader: %v", err)
	}
	if !bytes.Equal(body, []byte(alphaBody)) {
		t.Fatalf("Get body = %q, want %q", body, alphaBody)
	}

	const zetaKey = "blobs/zeta"
	if err := store.Put(ctx, zetaKey, strings.NewReader("zeta payload")); err != nil {
		t.Fatalf("Put second blob: %v", err)
	}
	keys, err := store.List(ctx, "blobs/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{alphaKey, zetaKey}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("List = %v, want %v", keys, want)
	}

	if err := store.Delete(ctx, alphaKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(ctx, alphaKey); err != nil {
		t.Fatalf("Delete absent blob: %v", err)
	}
	_, err = store.Get(ctx, alphaKey)
	var notFound *storage.BlobNotFoundError
	if !errors.As(err, &notFound) || notFound.Key != alphaKey {
		t.Fatalf("Get deleted blob = %v, want BlobNotFoundError for %q", err, alphaKey)
	}

	// Close owns no persistent process or connection and is therefore an
	// idempotent no-op; it does not invalidate the Store.
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read subprocess log: %v", err)
	}
	log := string(logData)
	if !strings.HasPrefix(log, "lsf --max-depth 0 -- docs-remote:examples\n") {
		t.Fatalf("first subprocess was not the exact startup probe:\n%s", log)
	}
	if got := strings.Count(log, "rcat -- docs-remote:examples/"+alphaKey+"\n"); got != 1 {
		t.Fatalf("alpha rcat count = %d, want 1 (identical/conflicting Put must not upload)\n%s", got, log)
	}
	liveEntries, err := os.ReadDir(liveDir)
	if err != nil {
		t.Fatalf("read live-process directory: %v", err)
	}
	if len(liveEntries) != 0 {
		t.Fatalf("subprocesses still live after synchronous calls: %v", liveEntries)
	}
}
