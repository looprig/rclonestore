//go:build integration

package rclonestore

import (
	"context"
	"errors"
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
