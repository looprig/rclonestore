//go:build integration

package rclonestore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/storage"
)

// TestRedactionNeverBreaksClassification (adopted from the v0.5.0 gate, finding
// M1): an inline value that is a substring of rclone's missing-config notice
// ("using defaults") is redacted out of that notice, so isMissingConfigNotice no
// longer recognises it and its "not found" reclassifies a real failure.
func TestRedactionNeverBreaksClassification(t *testing.T) {
	requireRclone(t)
	root := t.TempDir()
	for _, v := range []string{"default", "us", "fault"} {
		remote := ":local,description=" + v + ",links=NOTABOOL:" + root
		_, err := New(Options{Remote: remote, ConfigPath: root + "/absent.conf"})
		t.Logf("value %q: New err = %v", v, err)
		if err == nil {
			t.Errorf("value %q: New SUCCEEDED on an invalid remote (misclassified as not-found)", v)
		}
	}
	// control
	_, err := New(Options{Remote: ":local,description=zzz,links=NOTABOOL:" + root, ConfigPath: root + "/absent.conf"})
	if err == nil {
		t.Errorf("control: New succeeded on an invalid remote")
	}
}

// TestGetOnBrokenRemoteIsNotNotFound (gate M1): Get on a broken remote whose inline params include "default".
func TestGetOnBrokenRemoteIsNotNotFound(t *testing.T) {
	requireRclone(t)
	root := t.TempDir()
	good := ":local,description=default:" + root
	s, err := New(Options{Remote: good, ConfigPath: root + "/absent.conf"})
	if err != nil {
		t.Fatal(err)
	}
	// make the root unreadable-ish: point the blobstore at a remote with a bad option
	bad := newBlobStore(s.r, ":local,description=default,links=NOTABOOL:"+root, "", nil)
	bad.r.redact = newRedactor(mustParse(t, ":local,description=default,links=NOTABOOL:"+root), root+"/absent.conf")
	_, gerr := bad.Get(context.Background(), "k")
	var nf *storage.BlobNotFoundError
	t.Logf("Get err = %v", gerr)
	if errors.As(gerr, &nf) {
		t.Errorf("config-option failure reported as BlobNotFoundError")
	}
}

func mustParse(t *testing.T, r string) parsedRemote {
	p, err := parseRemote(r)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMidValueQuoteSplit (gate M3): mid-value quotes: rclone treats a quote as literal unless it starts the
// value; rclonestore's splitter treats any quote as opening a quoted run, so two
// values each carrying one mid-value apostrophe are merged into one needle and
// neither is redacted on its own.
func TestMidValueQuoteSplit(t *testing.T) {
	requireRclone(t)
	root := t.TempDir()
	remote := ":local,description=ab'SECRETONE,links=NOTA'BOOLSECRET:" + root
	_, err := New(Options{Remote: remote})
	if err == nil {
		t.Fatalf("New succeeded on an invalid remote")
	}
	for _, s := range []string{"SECRETONE", "BOOLSECRET"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("leaks %q: %q", s, err.Error())
		}
	}
}

// TestInheritedLogEnvCannotDefeatRedaction (gate A1): an operator's RCLONE_*
// logging variables must not reach rclone. JSON logging escapes a value holding
// '"' into a spelling no needle matches, and DEBUG adds argv lines; the child
// environment is scrubbed, so the error stays in the text grammar and redacted.
// Not parallel: it sets process environment.
func TestInheritedLogEnvCannotDefeatRedaction(t *testing.T) {
	requireRclone(t)
	for _, env := range [][2]string{
		{"RCLONE_USE_JSON_LOG", "true"},
		{"RCLONE_LOG_FORMAT", "json"},
		{"RCLONE_LOG_LEVEL", "DEBUG"},
		{"RCLONE_VERBOSE", "2"},
	} {
		t.Run(env[0], func(t *testing.T) {
			t.Setenv(env[0], env[1])
			root := t.TempDir()
			_, err := New(Options{Remote: `:local,description='S"EC<R>&ET',links=NOTABOOLSECRET:` + root})
			var re *RcloneError
			if !errors.As(err, &re) {
				t.Fatalf("New = %v, want an rclone failure", err)
			}
			if strings.Contains(re.Stderr, `"level"`) || strings.Contains(re.Stderr, "DEBUG") {
				t.Fatalf("inherited %s changed rclone's log grammar: %q", env[0], re.Stderr)
			}
			for _, secret := range []string{"EC<R>", "NOTABOOL"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("with %s, error leaks %q: %q", env[0], secret, err.Error())
				}
			}
		})
	}
}

// TestInheritedDataPathEnvIsNeutralised (regate M4): inherited RCLONE_<FLAG>
// variables that change rclone's output or writes must not reach it. Measured
// before the fix: PROGRESS appended stats to Get's bytes and turned a
// conflicting re-Put into a silent overwrite; DRY_RUN and INTERACTIVE made Put
// return nil having written nothing. Not parallel: it sets process environment.
func TestInheritedDataPathEnvIsNeutralised(t *testing.T) {
	requireRclone(t)
	for _, env := range [][2]string{
		{"RCLONE_PROGRESS", "true"},
		{"RCLONE_DRY_RUN", "true"},
		{"RCLONE_INTERACTIVE", "true"},
		{"RCLONE_MAX_DELETE", "0"},
	} {
		t.Run(env[0], func(t *testing.T) {
			t.Setenv(env[0], env[1])
			ctx := context.Background()
			s, err := New(Options{Remote: ":local:" + t.TempDir()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := s.Put(ctx, "sessions/k", strings.NewReader("hello")); err != nil {
				t.Fatalf("Put: %v", err)
			}
			rc, err := s.Get(ctx, "sessions/k")
			if err != nil {
				t.Fatalf("Get after Put: %v (the write was dropped)", err)
			}
			got, rerr := io.ReadAll(rc)
			if rerr != nil || string(got) != "hello" {
				t.Fatalf("Get = %q, %v; want exactly %q", got, rerr, "hello")
			}
			_ = rc.Close()
			var conflict *storage.BlobConflictError
			if err := s.Put(ctx, "sessions/k", strings.NewReader("different")); !errors.As(err, &conflict) {
				t.Fatalf("different-content re-Put = %v, want *BlobConflictError", err)
			}
			if err := s.Delete(ctx, "sessions/k"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			var nf *storage.BlobNotFoundError
			if _, err := s.Get(ctx, "sessions/k"); !errors.As(err, &nf) {
				t.Fatalf("Get after Delete = %v, want *BlobNotFoundError (the delete was dropped)", err)
			}
		})
	}
}

// TestEnvDefinedRemoteStillWorks: RCLONE_CONFIG_<REMOTE>_* is on the allowlist,
// so a remote defined only in the environment (here an alias onto a temp dir)
// still resolves as a named remote.
func TestEnvDefinedRemoteStillWorks(t *testing.T) {
	requireRclone(t)
	root := t.TempDir()
	t.Setenv("RCLONE_CONFIG_RSENVREMOTE_TYPE", "alias")
	t.Setenv("RCLONE_CONFIG_RSENVREMOTE_REMOTE", root)
	s, err := New(Options{Remote: "rsenvremote", Prefix: "pfx", PersistencePaths: []string{root}})
	if err != nil {
		t.Fatalf("New(env-defined remote): %v", err)
	}
	if err := s.Put(context.Background(), "a/b", strings.NewReader("env")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "pfx", "a", "b@blob")); err != nil || string(data) != "env" {
		t.Fatalf("object not written through the env-defined remote: %q, %v", data, err)
	}
}
