package rclonestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
			// Defence in depth: the raw spelling (doubled quotes intact), alone and
			// Go-escaped, is itself a needle — its content is not the unquoted value.
			name:    "raw quoted spelling echoed alone, verbatim and Go-escaped",
			remote:  `:local,a='IT''SX',b="DQ""RAW":/d`,
			in:      `raw 'IT''SX' and "DQ""RAW" esc ` + strconv.Quote(`"DQ""RAW"`),
			want:    `raw <redacted> and <redacted> esc "<redacted>"`,
			secrets: []string{"IT", "SX", "DQ", "RAW"},
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

// TestRunnerOneByteTruncation: a capture that lost exactly ONE byte from its
// start must still be treated as truncated, or a secret whose first byte was cut
// survives minus that byte.
func TestRunnerOneByteTruncation(t *testing.T) {
	t.Parallel()
	const secret = "QZXJWKVQZXJWKVQZXJWKV"
	parsed, err := parseRemote(":s3,secret_access_key=" + secret + ":bkt")
	if err != nil {
		t.Fatalf("parseRemote: %v", err)
	}
	red := newRedactor(parsed, "")
	dir := t.TempDir()
	capacity := stderrTailBytes + red.margin()
	// capacity+1 bytes, so only secret[0] is lost. Whole copies at the end make
	// redaction SHRINK the output by more than margin, so the final length bound
	// alone cannot hide an unmarked remnant at the start.
	copies := strings.Repeat(" "+secret, red.margin()/(len(secret)-len(redactedMark))+2)
	stderr := secret + strings.Repeat(".", capacity+1-len(secret)-len(copies)) + copies
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
	if strings.ContainsAny(re.Stderr, "QZXJWKV") {
		t.Fatalf("a fragment of the one-byte-cut secret survives: %q", re.Stderr[:min(len(re.Stderr), 60)])
	}
}

// TestRedactTailExactBound pins the retained length at exactly limit for inputs
// of limit+1 and limit+2 bytes, truncated or not.
func TestRedactTailExactBound(t *testing.T) {
	t.Parallel()
	red := newRedactor(parsedRemote{}, "")
	for _, extra := range []int{1, 2} {
		for _, truncated := range []bool{false, true} {
			in := strings.Repeat("a", 10) + strings.Repeat("b", extra)
			got := red.redactTail([]byte(in), truncated, 10)
			if len(got) != 10 {
				t.Fatalf("extra=%d truncated=%v: len %d, want 10 (%q)", extra, truncated, len(got), got)
			}
		}
	}
}

// TestRedactMasksOverlappingNeedles: the suffix of one value is the prefix of
// another; masking the original bytes redacts the union, where sequential
// replacement would leave the second value's tail.
func TestRedactMasksOverlappingNeedles(t *testing.T) {
	t.Parallel()
	parsed, err := parseRemote(":s3,a=QQQXXX,b=XXXZZZ:bkt")
	if err != nil {
		t.Fatalf("parseRemote: %v", err)
	}
	got := newRedactor(parsed, "").apply("see QQQXXXZZZ end")
	if got != "see <redacted> end" {
		t.Fatalf("apply = %q", got)
	}
	// Overlapping occurrences of ONE needle: "ABAB" twice in "ABABAB".
	parsed, err = parseRemote(":s3,a=ABAB:bkt")
	if err != nil {
		t.Fatalf("parseRemote: %v", err)
	}
	if got := newRedactor(parsed, "").apply("x ABABAB y"); got != "x <redacted> y" {
		t.Fatalf("apply = %q", got)
	}
}

// TestChildEnvDeniesRcloneByDefault: rclone reads EVERY global flag from
// RCLONE_<FLAG>, and several change the data path or output (measured:
// RCLONE_PROGRESS writes stats into Get's stdout and defeats Put's conflict
// probe; RCLONE_DRY_RUN and RCLONE_INTERACTIVE make Put a silent no-op; the log
// family changes the grammar redaction relies on). So every RCLONE_* variable is
// dropped except an explicit allowlist — config location/decryption, env-defined
// remotes (RCLONE_CONFIG_<REMOTE>_*), and transport tuning. Names compare
// case-insensitively (Windows environment names are). Non-RCLONE variables pass.
func TestChildEnvDeniesRcloneByDefault(t *testing.T) {
	t.Parallel()
	kept := []string{
		"PATH=/bin", "HOME=/h", "HTTPS_PROXY=http://p:3128", "http_proxy=http://p", "AWS_ACCESS_KEY_ID=A",
		"GOOGLE_APPLICATION_CREDENTIALS=/c.json", "_RCLONE_CONFIG_KEY_FILE=/k", "NOT_RCLONE_PROGRESS=1", "=C:=C:\\x", "RCLONE",
		"RCLONE_CONFIG=/etc/rclone.conf", "RCLONE_CONFIG_PASS=p", "RCLONE_CONFIG_DIR=/etc",
		"RCLONE_CONFIG_MYS3_TYPE=s3", "RCLONE_CONFIG_MYS3_ACCESS_KEY_ID=A", "rclone_config_win_type=local",
		"RCLONE_PASSWORD_COMMAND=pass rclone", "RCLONE_ASK_PASSWORD=false",
		"RCLONE_CONTIMEOUT=10s", "RCLONE_TIMEOUT=1m", "RCLONE_EXPECT_CONTINUE_TIMEOUT=1s",
		"RCLONE_RETRIES=5", "RCLONE_RETRIES_SLEEP=1s", "RCLONE_LOW_LEVEL_RETRIES=20",
		"RCLONE_CA_CERT=/ca.pem", "RCLONE_CLIENT_CERT=/c.pem", "RCLONE_CLIENT_KEY=/k.pem", "RCLONE_CLIENT_PASS=x",
		"RCLONE_NO_CHECK_CERTIFICATE=true", "RCLONE_BWLIMIT=10M", "RCLONE_BWLIMIT_FILE=1M",
		"RCLONE_TPSLIMIT=10", "RCLONE_TPSLIMIT_BURST=2", "RCLONE_USER_AGENT=ua", "RCLONE_BIND=0.0.0.0",
		"RCLONE_DISABLE_HTTP2=true", "RCLONE_DISABLE_HTTP_KEEP_ALIVES=true", "RCLONE_DSCP=AF21",
	}
	dropped := []string{
		"RCLONE_PROGRESS=true", "RCLONE_PROGRESS_TERMINAL_TITLE=true", "RCLONE_DRY_RUN=true", "RCLONE_INTERACTIVE=true",
		"RCLONE_RC=true", "RCLONE_RC_ADDR=:5572", "RCLONE_MAX_DELETE=0", "RCLONE_DISABLE=ListR",
		"RCLONE_HEADER=X-A: b", "RCLONE_HEADER_UPLOAD=X: y", "RCLONE_METADATA_SET=a=b", "RCLONE_S3_REGION=us",
		"RCLONE_S3_VERSION_AT=2020-01-01", "RCLONE_LOCAL_ENCODING=None", "RCLONE_CONFIGX=1", "RCLONE_CONFIG_=1",
		"RCLONE_VERBOSE=2", "RCLONE_QUIET=true", "RCLONE_LOG_LEVEL=DEBUG", "RCLONE_LOG_FORMAT=json",
		"RCLONE_USE_JSON_LOG=true", "RCLONE_LOG_FILE=/tmp/l", "RCLONE_SYSLOG=true", "RCLONE_STATS_LOG_LEVEL=NOTICE",
		"RCLONE_DUMP=headers", "RCLONE_DUMP_HEADERS=true", "RCLONE_TEMP_DIR=/t", "RCLONE_=1",
		"Rclone_Progress=true", "rclone_dry_run=true", "RcLoNe_InTeRaCtIvE=true",
	}
	got := childEnv(append(append([]string(nil), kept...), dropped...))
	if strings.Join(got, "|") != strings.Join(kept, "|") {
		t.Fatalf("childEnv =\n  %v\nwant\n  %v", got, kept)
	}
}

// TestProbeContextHasDefaultBound: with no Options.Timeout, New's reachability
// probe is still bounded (rclone retries an unreachable endpoint for minutes);
// with a Timeout the runner's own bound applies and the probe adds none.
func TestProbeContextHasDefaultBound(t *testing.T) {
	t.Parallel()
	ctx, cancel := probeContext(0, defaultProbeTimeout)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("probe context has no deadline with Timeout 0")
	}
	if left := time.Until(deadline); left <= defaultProbeTimeout-time.Minute || left > defaultProbeTimeout {
		t.Fatalf("probe deadline in %v, want about %v", left, defaultProbeTimeout)
	}
	ctx2, cancel2 := probeContext(time.Second, defaultProbeTimeout)
	defer cancel2()
	if _, ok := ctx2.Deadline(); ok {
		t.Fatalf("probe context adds its own deadline when Timeout is set")
	}
}

// TestProbeHonoursFallbackBound: with no per-call Timeout, a hung rclone probe is
// killed at the fallback bound and reported as a *ProbeError carrying the
// deadline, instead of hanging New.
func TestProbeHonoursFallbackBound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := writeFakeRclone(t, dir, fakeSpec{argvPath: filepath.Join(dir, "argv"), sleepSecs: 30})
	bs := newBlobStore(&runner{binary: bin}, "myremote", "", nil)
	start := time.Now()
	err := probe(bs, 300*time.Millisecond)
	var pe *ProbeError
	if !errors.As(err, &pe) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe = %v, want *ProbeError wrapping context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("probe ran %v past its 300ms fallback", elapsed)
	}
}
