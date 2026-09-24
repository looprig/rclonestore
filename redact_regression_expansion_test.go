package rclonestore

import (
	"strings"
	"testing"
)

// TestRedactExpansionLeak (adopted from the v0.5.0 gate, finding M2): a realistic
// Azure SAS remote whose short value ("2") occurs many times inside the long one.
// Sequential replacement lengthened a cut remnant past the dropped margin and
// leaked 26 bytes of the sig; masking the original bytes leaks none.
func TestRedactExpansionLeak(t *testing.T) {
	t.Parallel()
	secret := "sig=Xb7QmK9pLr3TzW8vNc2HfYs5JdA1gE6uRo0iBkPa4t%3D"
	sas := "https://acct.blob.core.windows.net/c?sv=2022-11-02&ss=b&srt=co&sp=rwdlac&se=2026-12-22T22:22:22Z&st=2026-01-02T02:02:02Z&spr=https&" + secret
	remote := ":azureblob,upload_concurrency=2,sas_url='" + sas + "':c/p"
	pr, err := parseRemote(remote)
	if err != nil {
		t.Fatal(err)
	}
	r := newRedactor(pr, "")
	line := "2026/09/24 06:00:00 ERROR : " + remote + "/k@blob: Failed to copy: ... retrying\n"
	worst := ""
	for pad := 0; pad < len(line)+10; pad++ {
		full := strings.Repeat(line, 80) + strings.Repeat("x", pad) + "\n"
		cap := stderrTailBytes + r.margin()
		captured := full[len(full)-cap:]
		out := r.redactTail([]byte(captured), true, stderrTailBytes)
		for n := len(secret); n >= 6; n-- {
			for i := 0; i+n <= len(secret); i++ {
				if strings.Contains(out, secret[i:i+n]) && n > len(worst) {
					worst = secret[i : i+n]
				}
			}
		}
	}
	if worst != "" {
		t.Fatalf("longest secret fragment in a redacted RcloneError.Stderr: %q (of %d-byte secret)", worst, len(secret))
	}
}
