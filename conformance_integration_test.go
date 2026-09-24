//go:build integration

package rclonestore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/storage"
	"github.com/looprig/storage/storetest"
)

// conformanceRemoteEnv names an optional extra remote for the conformance suite:
// a named remote or ":backend:" connection string (e.g. an S3-compatible bucket).
// Each backend instance gets its own fresh Prefix under it. Unset, the test
// skips. It is how the suite is run against an object-store remote, whose flat
// key space behaves differently from the local filesystem.
const conformanceRemoteEnv = "RCLONESTORE_CONFORMANCE_REMOTE"

// TestBlobsConformance runs storage's Blobs conformance suite against a REAL
// rclone binary using the LOCAL backend via a ":local:" connection string rooted
// at a fresh per-backend temp dir — no cloud credentials required. This is where
// the RC2 not-found classification (exit codes 3/4 and the stderr marker), the
// remote-form-aware object path, the lsf-on-a-file existence probe and the
// "@blob" leaf encoding (a key and its "/…" extension coexist, storage v0.7.0)
// are validated against real rclone rather than a fake.
//
// It skips (never fails) when rclone is absent, so the default `go test` run and CI
// without rclone stay green; run it explicitly with:
//
//	GOWORK=off go test -tags integration -race ./...
func TestBlobsConformance(t *testing.T) {
	requireRclone(t)
	storetest.TestBlobs(t, func(t *testing.T) storage.Blobs {
		s, err := New(Options{Remote: ":local:" + t.TempDir()})
		if err != nil {
			t.Fatalf("New(:local: temp remote): %v", err)
		}
		return s
	})
}

var remoteInstance atomic.Uint64

// TestBlobsConformanceRemote runs the same suite against the remote named by
// RCLONESTORE_CONFORMANCE_REMOTE, one fresh prefix per backend instance.
func TestBlobsConformanceRemote(t *testing.T) {
	requireRclone(t)
	remote := os.Getenv(conformanceRemoteEnv)
	if remote == "" {
		t.Skip(conformanceRemoteEnv + " not set; skipping remote Blobs conformance")
	}
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	storetest.TestBlobs(t, func(t *testing.T) storage.Blobs {
		prefix := "conformance-" + run + "/" + strconv.FormatUint(remoteInstance.Add(1), 10)
		s, err := New(Options{Remote: remote, Prefix: prefix, Timeout: time.Minute})
		if err != nil {
			t.Fatalf("New(%s, prefix %s): %v", conformanceRemoteEnv, prefix, err)
		}
		return s
	})
}

// TestLocalLegacyLayoutRefused writes the layout rclonestore <= v0.4.x produced —
// an object at its bare key path — into a real :local: root and asserts New
// refuses it, while a root holding only current-layout data opens.
func TestLocalLegacyLayoutRefused(t *testing.T) {
	requireRclone(t)
	ctx := context.Background()

	current := t.TempDir()
	s, err := New(Options{Remote: ":local:" + current})
	if err != nil {
		t.Fatalf("New(fresh root): %v", err)
	}
	for _, key := range []string{"sessions/a", "sessions/a/b"} {
		if err := s.Put(ctx, key, bytes.NewReader([]byte(key))); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	if _, err := New(Options{Remote: ":local:" + current}); err != nil {
		t.Fatalf("New(current-layout root) = %v, want success", err)
	}

	// A bare-path object written beside current data (a rolled-back binary).
	legacyFile := filepath.Join(current, "sessions", "c")
	if err := os.WriteFile(legacyFile, []byte("v0.4 bytes"), 0o600); err != nil {
		t.Fatalf("seed legacy object: %v", err)
	}
	_, err = New(Options{Remote: ":local:" + current})
	requireLegacyLayout(t, err, "sessions/c")
	_, err = s.List(ctx, "")
	requireLegacyLayout(t, err, "sessions/c")
	if _, statErr := os.Stat(legacyFile); statErr != nil {
		t.Fatalf("refusal touched the legacy object: %v", statErr)
	}
}

// TestLocalDirectoryIsNeverAConflictingBlob is the v0.4.2 defect: a directory at
// a blob's own location was listed, cat'd (rclone concatenates a directory's
// files) and reported as *storage.BlobConflictError. A directory is not a blob:
// Put must fail with the backend's own error, never a conflict.
func TestLocalDirectoryIsNeverAConflictingBlob(t *testing.T) {
	requireRclone(t)
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(Options{Remote: ":local:" + root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A foreign directory occupying the encoded location of key "sessions/a".
	if err := os.MkdirAll(filepath.Join(root, "sessions", "a@blob"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions", "a@blob", "x@blob"), []byte("other"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err = s.Put(ctx, "sessions/a", bytes.NewReader([]byte("mine")))
	var conflict *storage.BlobConflictError
	if errors.As(err, &conflict) {
		t.Fatalf("Put over a directory = BlobConflictError; a directory is not a blob")
	}
	var re *RcloneError
	if !errors.As(err, &re) {
		t.Fatalf("Put over a directory = %T %v, want *RcloneError from rcat", err, err)
	}
}

func requireRclone(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not on PATH; skipping real-rclone Blobs conformance")
	}
}
