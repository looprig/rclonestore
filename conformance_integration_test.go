//go:build integration

package rclonestore

import (
	"os/exec"
	"testing"

	"github.com/ciram-co/storekit"
	"github.com/ciram-co/storekit/storetest"
)

// TestBlobsConformance runs storekit's Blobs conformance suite against a REAL
// rclone binary using the LOCAL backend via a ":local:" connection string rooted
// at a fresh per-backend temp dir — no cloud credentials required. This is where
// the RC2 not-found classification (exit codes 3/4 and the stderr marker), the
// remote-form-aware object path, and the lsf-on-a-file existence probe are
// validated against real rclone rather than a fake.
//
// It skips (never fails) when rclone is absent, so the default `go test` run and CI
// without rclone stay green; run it explicitly with:
//
//	GOWORK=off go test -tags integration -race ./...
func TestBlobsConformance(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not on PATH; skipping real-rclone Blobs conformance")
	}
	storetest.TestBlobs(t, func(t *testing.T) storekit.Blobs {
		s, err := New(Options{Remote: ":local:" + t.TempDir()})
		if err != nil {
			t.Fatalf("New(:local: temp remote): %v", err)
		}
		return s
	})
}
