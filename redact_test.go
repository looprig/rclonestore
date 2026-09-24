package rclonestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRedactorApply pins what redaction removes from captured rclone stderr: the
// inline backend spec of a ":backend,params:" remote (verbatim and Go-quoted, as
// rclone's CRITICAL/-vv lines print it) becomes ":backend,<redacted>:", every
// parameter value is removed wherever it appears on its own (raw, unquoted and
// Go-quoted), and the --config path is removed. A named remote or a
// parameter-free spec has no inline secret, so only the config path applies.
func TestRedactorApply(t *testing.T) {
	t.Parallel()
	const s3 = ":s3,provider=Other,access_key_id=AKSECRET,secret_access_key=SKSECRET,endpoint='http://h:9':bkt"
	tests := []struct {
		name       string
		remote     string
		configPath string
		in         string
		want       string
		secrets    []string
	}{
		{
			name:    "verbatim spec keeps backend and path",
			remote:  s3,
			in:      "ERROR : " + s3 + "/p/k@blob is a directory",
			want:    "ERROR : :s3,<redacted>:bkt/p/k@blob is a directory",
			secrets: []string{"AKSECRET", "SKSECRET", "http://h:9"},
		},
		{
			name:    "Go-quoted spec (CRITICAL line) is redacted",
			remote:  s3,
			in:      `CRITICAL: Failed to create file system for ` + strconv.Quote(s3) + `: bad`,
			want:    `CRITICAL: Failed to create file system for ":s3,<redacted>:bkt": bad`,
			secrets: []string{"AKSECRET", "SKSECRET"},
		},
		{
			name:    "a value echoed on its own is redacted",
			remote:  s3,
			in:      `couldn't parse "SKSECRET" and "AKSECRET" at http://h:9`,
			want:    `couldn't parse "<redacted>" and "<redacted>" at <redacted>`,
			secrets: []string{"AKSECRET", "SKSECRET", "http://h:9"},
		},
		{
			name:    "single-quoted value with colon, comma and doubled quote",
			remote:  ":local,description='SEC:RET,it''s':/data",
			in:      "for :local,description='SEC:RET,it''s':/data and SEC:RET,it's alone",
			want:    "for :local,<redacted>:/data and <redacted> alone",
			secrets: []string{"SEC:RET", "it's", "it''s"},
		},
		{
			name:    "double-quoted value and its Go-escaped form",
			remote:  `:local,description="DQ""SECRET":/data`,
			in:      `raw DQ"SECRET, quoted ` + strconv.Quote(`DQ"SECRET`) + `, spec ` + strconv.Quote(`:local,description="DQ""SECRET":/data`),
			want:    `raw <redacted>, quoted "<redacted>", spec ":local,<redacted>:/data"`,
			secrets: []string{"DQ", "SECRET"},
		},
		{
			// rclone prints a rejected value with %q; a backslash doubles there.
			name:    "a Go-escaped value echoed on its own is redacted",
			remote:  `:local,description=BS\SECRET:/d`,
			in:      `couldn't parse config item "description" = ` + strconv.Quote(`BS\SECRET`),
			want:    `couldn't parse config item "description" = "<redacted>"`,
			secrets: []string{"SECRET"},
		},
		{
			// A short value inside a longer one: replacing the short one first would
			// leave the rest of the long one behind.
			name:    "a value that contains another value is redacted whole",
			remote:  ":s3,region=KEY,secret_access_key=KEYTAILSECRET:b",
			in:      "echo KEYTAILSECRET",
			want:    "echo <redacted>",
			secrets: []string{"TAIL", "SECRET"},
		},
		{
			name:    "a bare flag parameter carries no value",
			remote:  ":local,links,description=FLAGSECRET:/d",
			in:      ":local,links,description=FLAGSECRET:/d FLAGSECRET",
			want:    ":local,<redacted>:/d <redacted>",
			secrets: []string{"FLAGSECRET"},
		},
		{
			name:       "config path is redacted",
			remote:     "myremote",
			configPath: "/etc/SECRETCONF/rclone.conf",
			in:         `NOTICE: Config file "/etc/SECRETCONF/rclone.conf" not found`,
			want:       `NOTICE: Config file "<redacted>" not found`,
			secrets:    []string{"SECRETCONF"},
		},
		{
			name:   "named remote without config is untouched",
			remote: "myremote",
			in:     "myremote:pfx/k@blob: object not found",
			want:   "myremote:pfx/k@blob: object not found",
		},
		{
			name:   "parameter-free spec is untouched",
			remote: ":local:/tmp/x",
			in:     ":local:/tmp/x/k@blob: directory not found",
			want:   ":local:/tmp/x/k@blob: directory not found",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := parseRemote(tt.remote)
			if err != nil {
				t.Fatalf("parseRemote: %v", err)
			}
			got := newRedactor(parsed, tt.configPath).apply(tt.in)
			if got != tt.want {
				t.Fatalf("apply =\n  %q\nwant\n  %q", got, tt.want)
			}
			for _, secret := range tt.secrets {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q survives redaction: %q", secret, got)
				}
			}
		})
	}
}

// TestRunnerRedactsStderrTail drives the runner against a fake rclone whose
// stderr echoes a secret-bearing remote twice: once cut by the bounded capture at
// every possible offset and immediately followed by a whole copy, and once whole
// near the end (so redaction SHRINKS the
// capture and the final length bound alone cannot hide a cut remnant). No
// fragment of the secret may survive in RcloneError, and the tail stays bounded.
func TestRunnerRedactsStderrTail(t *testing.T) {
	t.Parallel()
	const secret = "QZXJWKVQZXJWKVQZXJWKVQZXJWKV" // letters absent from everything else
	remote := ":s3,access_key_id=" + secret + ":bkt"
	parsed, err := parseRemote(remote)
	if err != nil {
		t.Fatalf("parseRemote: %v", err)
	}
	red := newRedactor(parsed, "")
	capture := stderrTailBytes + red.margin()
	trailer := "\nERROR : " + remote + "/k@blob failed\n"

	for offset := 0; offset <= len(remote)+2; offset++ {
		offset := offset
		t.Run("cut at "+strconv.Itoa(offset), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// The capture keeps the last `capture` bytes; it begins `offset` bytes
			// into the first remote below.
			// A second whole copy follows at once, so for offsets near the end of
			// the first it begins INSIDE the capture's first margin bytes: only
			// redacting before dropping that margin removes it whole.
			filler := strings.Repeat(".", capture-(len(remote)-offset)-len(remote)-len(trailer))
			stderr := "head " + remote + "\n" + remote + remote + filler + trailer
			stderrPath := filepath.Join(dir, "stderr")
			if err := os.WriteFile(stderrPath, []byte(stderr), 0o600); err != nil {
				t.Fatalf("write stderr: %v", err)
			}
			bin := writeFakeRclone(t, dir, fakeSpec{argvPath: filepath.Join(dir, "argv"), stderrPath: stderrPath, exitCode: 5})
			r := &runner{binary: bin, redact: red}
			runErr := r.run(context.Background(), "lsf", nil, []string{remote}, nil, nil)
			var re *RcloneError
			if !errors.As(runErr, &re) {
				t.Fatalf("run = %v, want *RcloneError", runErr)
			}
			if len(re.Stderr) > stderrTailBytes {
				t.Fatalf("Stderr tail is %d bytes, bound %d", len(re.Stderr), stderrTailBytes)
			}
			if !strings.HasSuffix(re.Stderr, "ERROR : :s3,<redacted>:bkt/k@blob failed\n") {
				t.Fatalf("tail lost its redacted trailer: %q", re.Stderr[max(0, len(re.Stderr)-80):])
			}
			for _, text := range []string{re.Stderr, re.Error()} {
				if strings.ContainsAny(text, "QZXJWKV") {
					t.Fatalf("a fragment of the secret survives: %q", text[:min(len(text), 120)])
				}
			}
		})
	}
}

// TestRunnerRedactedTailBound pins the retained length with a redactor in place:
// secret-free stderr longer than the bound keeps exactly its last
// stderrTailBytes bytes, whether or not the (margin-widened) capture truncated.
func TestRunnerRedactedTailBound(t *testing.T) {
	t.Parallel()
	parsed, err := parseRemote(":s3,access_key_id=" + strings.Repeat("S", 200) + ":bkt")
	if err != nil {
		t.Fatalf("parseRemote: %v", err)
	}
	red := newRedactor(parsed, "")
	for _, size := range []int{stderrTailBytes + red.margin()/2, stderrTailBytes + red.margin() + 100} {
		size := size
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var b strings.Builder
			for i := 0; b.Len() < size; i++ {
				b.WriteString(strconv.Itoa(i % 10))
			}
			stderr := b.String()[:size]
			stderrPath := filepath.Join(dir, "stderr")
			if err := os.WriteFile(stderrPath, []byte(stderr), 0o600); err != nil {
				t.Fatalf("write stderr: %v", err)
			}
			bin := writeFakeRclone(t, dir, fakeSpec{argvPath: filepath.Join(dir, "argv"), stderrPath: stderrPath, exitCode: 5})
			runErr := (&runner{binary: bin, redact: red}).run(context.Background(), "lsf", nil, nil, nil, nil)
			var re *RcloneError
			if !errors.As(runErr, &re) {
				t.Fatalf("run = %v, want *RcloneError", runErr)
			}
			if want := stderr[len(stderr)-stderrTailBytes:]; re.Stderr != want {
				t.Fatalf("Stderr = %d bytes, want the last %d bytes of stderr", len(re.Stderr), stderrTailBytes)
			}
		})
	}
}

// TestNewProbeErrorRedactsInlineSecrets: a probe failure whose stderr echoes the
// connection string (as rclone's CRITICAL line does) must not carry any inline
// parameter value into the *ProbeError.
func TestNewProbeErrorRedactsInlineSecrets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	remote := ":s3,access_key_id=AKSECRET,secret_access_key=SKSECRET:bkt"
	stderrPath := filepath.Join(dir, "stderr")
	line := `CRITICAL: Failed to create file system for ` + strconv.Quote(remote) + `: "SKSECRET" rejected` + "\n" + remote + "\n"
	if err := os.WriteFile(stderrPath, []byte(line), 0o600); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	bin := writeFakeRclone(t, dir, fakeSpec{argvPath: filepath.Join(dir, "argv"), stderrPath: stderrPath, exitCode: 1})
	_, err := New(Options{Remote: remote, Binary: bin})
	var pe *ProbeError
	if !errors.As(err, &pe) {
		t.Fatalf("New = %v, want *ProbeError", err)
	}
	for _, secret := range []string{"AKSECRET", "SKSECRET"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("ProbeError leaks %q: %q", secret, err.Error())
		}
	}
	if !strings.Contains(err.Error(), ":s3,<redacted>:bkt") {
		t.Fatalf("ProbeError lost the redacted remote form: %q", err.Error())
	}
}
