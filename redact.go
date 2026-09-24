package rclonestore

import (
	"strconv"
	"strings"
)

// redactedMark replaces every run of secret-bearing bytes removed from captured
// stderr.
const redactedMark = "<redacted>"

// redactor removes credential-bearing text from rclone's stderr before any of it
// reaches an *RcloneError (and so any error string or log line built from one).
// It only shapes the error SURFACE: classification never reads stderr (isNotFound
// uses rclone's exit codes alone), so nothing redaction does can change whether a
// failure reads as "absent".
//
// rclone echoes the remote it was given in its diagnostics — verbatim ("ERROR :
// :s3,access_key_id=…,secret_access_key=…:bkt/k is a directory…") and Go-quoted
// (the CRITICAL "Failed to create file system for \"…\"" line and -vv output) —
// and it re-prints a rejected option value on its own. An inline
// ":backend,name=value,…:" connection string carries credentials in those values.
// The needles are therefore:
//
//   - the whole inline spec ":backend,params:", verbatim and Go-quoted; only its
//     parameter part is masked, so it reads ":backend,<redacted>:" and the backend
//     name and path survive;
//   - every parameter value in exactly the spellings rclone can print: raw (as
//     written, quotes included), as rclone uses it (unquoted, doubled quotes
//     collapsed) and Go-escaped (%q) forms of both. Values are parsed with
//     rclone's own grammar (parseConnectionString). A value that also occurs as
//     ordinary text (e.g. "true") is over-redacted; stderr is diagnostic only;
//   - the --config path, raw and Go-escaped.
//
// Any other transformation of a secret (URL-encoding, reordered query
// parameters, case folding, JSON escaping) is NOT matched; runner scrubs
// rclone's logging environment so its output stays in the text grammar these
// needles describe.
//
// Masking works on the ORIGINAL bytes: every occurrence of every needle is marked
// (overlaps merge), and one redactedMark is emitted per marked run. Nothing a
// replacement emits can therefore lengthen or uncover a remnant. A named remote
// carries no inline parameter (its grammar has no ','), so only the config path
// applies to it. The zero redactor is a no-op.
type redactor struct {
	spec  []specNeedle // whole-spec spellings: only the parameter part is masked
	plain []string     // values and the config path
}

type specNeedle struct {
	find string
	keep int // bytes of find to leave unmasked at its start (":backend,")
}

// newRedactor derives the redaction set from the parsed remote and config path.
func newRedactor(remote parsedRemote, configPath string) redactor {
	var r redactor
	if remote.connectionString && len(remote.params) > 0 {
		head := ":" + remote.backend + ","
		full := ":" + remote.spec + ":"
		r.spec = append(r.spec, specNeedle{find: full, keep: len(head)})
		if quoted := goEscaped(full); quoted != full {
			// The backend is lowercase alphanumeric, so ":backend," escapes to itself.
			r.spec = append(r.spec, specNeedle{find: quoted, keep: len(head)})
		}
		for _, p := range remote.params {
			r.plain = append(r.plain, p.raw, goEscaped(p.raw), p.value, goEscaped(p.value))
		}
	}
	if configPath != "" {
		r.plain = append(r.plain, configPath, goEscaped(configPath))
	}
	r.plain = uniqueNonEmpty(r.plain)
	return r
}

// apply returns s with every needle occurrence masked.
func (r redactor) apply(s string) string {
	return r.mask([]byte(s), false)
}

// margin is the longest needle. A bounded stderr capture that began mid-needle
// holds at most margin-1 bytes of it, at its very start.
func (r redactor) margin() int {
	m := 0
	for _, n := range r.spec {
		m = max(m, len(n.find))
	}
	for _, n := range r.plain {
		m = max(m, len(n))
	}
	return m
}

// redactTail redacts a bounded stderr capture and bounds the result to limit
// bytes. truncated reports that the capture lost bytes at its start, in which case
// a needle may have been cut there: its remnant lies within the first margin
// ORIGINAL bytes, so those are masked too, before anything is emitted.
func (r redactor) redactTail(captured []byte, truncated bool, limit int) string {
	s := r.mask(captured, truncated)
	if len(s) > limit {
		s = s[len(s)-limit:]
	}
	return s
}

// mask marks every needle occurrence in b (and, if cutStart, the first margin
// bytes), then emits the unmarked bytes with one redactedMark per marked run.
func (r redactor) mask(b []byte, cutStart bool) string {
	if len(b) == 0 {
		return ""
	}
	s := string(b)
	marked := make([]bool, len(b))
	for _, n := range r.spec {
		markAll(s, n.find, n.keep, 1, marked)
	}
	for _, n := range r.plain {
		markAll(s, n, 0, 0, marked)
	}
	if cutStart {
		for i := 0; i < min(r.margin(), len(b)); i++ {
			marked[i] = true
		}
	}
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		if !marked[i] {
			out.WriteByte(b[i])
			i++
			continue
		}
		out.WriteString(redactedMark)
		for i < len(b) && marked[i] {
			i++
		}
	}
	return out.String()
}

// markAll marks every (possibly overlapping) occurrence of find in s, except its
// first keepHead and last keepTail bytes.
func markAll(s, find string, keepHead, keepTail int, marked []bool) {
	for from := 0; from <= len(s)-len(find); {
		j := strings.Index(s[from:], find)
		if j < 0 {
			return
		}
		start := from + j
		for k := start + keepHead; k < start+len(find)-keepTail; k++ {
			marked[k] = true
		}
		from = start + 1
	}
}

// goEscaped is s as it appears inside a Go %q string (without the quotes).
func goEscaped(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

func uniqueNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
