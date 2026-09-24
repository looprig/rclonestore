package rclonestore

import (
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

const secretAlpha = "QXZJ"

func genValue(rng *rand.Rand) (spelled string) {
	n := 1 + rng.Intn(12)
	var b strings.Builder
	quoted := rng.Intn(2) == 0
	for i := 0; i < n; i++ {
		if quoted && rng.Intn(4) == 0 {
			b.WriteByte(",:'\"\\"[rng.Intn(5)])
		} else {
			b.WriteByte(secretAlpha[rng.Intn(len(secretAlpha))])
		}
	}
	v := b.String()
	if !quoted {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// TestRedactTruncationFuzz (adopted from the v0.5.0 gate, finding M2): secrets are built only from QXZJ (plus punctuation inside
// quotes); everything else is lowercase filler. Any surviving uppercase Q/X/Z/J in
// the redacted, truncated tail is a leaked fragment.
func TestRedactTruncationFuzz(t *testing.T) {
	t.Parallel()
	// The gate's full sweep (3000 iterations, every pad) takes ~3 minutes under
	// -race; the default is a representative slice. Set
	// RCLONESTORE_REDACT_FUZZ_FULL=1 for the full sweep.
	iterations, padStride := 400, 3
	if os.Getenv("RCLONESTORE_REDACT_FUZZ_FULL") == "1" {
		iterations, padStride = 3000, 1
	}
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < iterations; iter++ {
		np := 1 + rng.Intn(4)
		var params []string
		var inner []string
		for i := 0; i < np; i++ {
			sp := genValue(rng)
			params = append(params, "k"+strconv.Itoa(i)+"="+sp)
			if sp[0] == '\'' {
				inner = append(inner, strings.ReplaceAll(sp[1:len(sp)-1], "''", "'"))
			} else {
				inner = append(inner, sp)
			}
		}
		remote := ":s3," + strings.Join(params, ",") + ":bkt/p"
		pr, err := parseRemote(remote)
		if err != nil {
			t.Fatalf("parse %q: %v", remote, err)
		}
		cfg := "/cfg/path"
		r := newRedactor(pr, cfg)
		// compose stderr
		var sb strings.Builder
		for j := 0; j < 14; j++ {
			switch rng.Intn(5) {
			case 0:
				sb.WriteString("error : " + remote + "/k@blob is a directory\n")
			case 1:
				sb.WriteString("critical: failed for " + strconv.Quote(remote) + "\n")
			case 2:
				v := inner[rng.Intn(len(inner))]
				sb.WriteString("parsing " + strconv.Quote(v) + " as bool\n")
			case 3:
				sb.WriteString("value " + inner[rng.Intn(len(inner))] + " raw\n")
			case 4:
				sb.WriteString(strings.Repeat("filler ", rng.Intn(20)) + "\n")
			}
		}
		base := sb.String()
		for pad := 0; pad < 90; pad += padStride {
			full := base + strings.Repeat("f", pad) + "\n"
			for _, limit := range []int{5, 17, 40, 64, 150, 400} {
				cap := limit + r.margin()
				for cut := 0; cut <= len(full); cut++ {
					if cut < len(full) && full[cut] != '\n' {
						continue
					}
					s := full[:cut]
					captured := s
					if len(captured) > cap {
						captured = captured[len(captured)-cap:]
					}
					truncated := len(s) > len(captured)

					out := r.redactTail([]byte(captured), truncated, limit)
					if strings.ContainsAny(out, secretAlpha) {
						t.Fatalf("leak: remote=%q limit=%d cut=%d out=%q", remote, limit, cut, out)
					}
					if len(out) > limit {
						t.Fatalf("bound breach")
					}
				}
			}
		}
	}
}

// TestRedactTruncationFuzzSAS stresses the masking with realistic SAS-style
// credentials: a long value built from '&'/'='-joined fields, plus SHORT values
// drawn from inside it (the shape that made sequential replacement lengthen a cut
// remnant), echoed on repeated retry lines and cut at every line position under
// the real stderrTailBytes bound. Secrets use only Q/X/Z/J; nothing else does.
func TestRedactTruncationFuzzSAS(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(2))
	field := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(secretAlpha[rng.Intn(len(secretAlpha))])
		}
		return b.String()
	}
	for iter := 0; iter < 60; iter++ {
		var fields []string
		for i := 0; i < 3+rng.Intn(6); i++ {
			fields = append(fields, strings.ToLower("k"+strconv.Itoa(i))+"="+field(4+rng.Intn(40)))
		}
		long := "https://acct.example/c?" + strings.Join(fields, "&") + "&sig=" + field(40+rng.Intn(60)) + "%3D"
		short1 := long[strings.Index(long, "=")+1:][:1+rng.Intn(3)] // a prefix of a field value: occurs inside long
		short2 := field(1 + rng.Intn(2))
		remote := ":azureblob,account=" + short1 + ",sas_url='" + long + "',tier=" + short2 + ":c/p"
		pr, err := parseRemote(remote)
		if err != nil {
			t.Fatalf("parse %q: %v", remote, err)
		}
		r := newRedactor(pr, "")
		lines := []string{
			"2026/09/24 06:00:00 ERROR : " + remote + "/k@blob: Failed to copy: retrying\n",
			"2026/09/24 06:00:00 CRITICAL: Failed to create file system for " + strconv.Quote(remote) + ": bad\n",
			"2026/09/24 06:00:00 ERROR : value " + strconv.Quote(short1) + " and " + short2 + " rejected\n",
		}
		var sb strings.Builder
		for sb.Len() < 3*stderrTailBytes {
			sb.WriteString(lines[rng.Intn(len(lines))])
		}
		full := sb.String()
		capacity := stderrTailBytes + r.margin()
		for pad := 0; pad < 64; pad++ {
			s := full + strings.Repeat("y", pad*7) + "\n"
			captured := s[len(s)-capacity:]
			out := r.redactTail([]byte(captured), true, stderrTailBytes)
			if strings.ContainsAny(out, secretAlpha) {
				i := strings.IndexAny(out, secretAlpha)
				t.Fatalf("leak at %d: %q", i, out[max(0, i-40):min(len(out), i+40)])
			}
			if len(out) > stderrTailBytes {
				t.Fatalf("bound breach: %d", len(out))
			}
		}
	}
}
