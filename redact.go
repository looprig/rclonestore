package rclonestore

import (
	"sort"
	"strconv"
	"strings"
)

// redactedMark replaces every secret-bearing string removed from captured stderr.
const redactedMark = "<redacted>"

// redactor removes credential-bearing text from rclone's stderr before any of it
// reaches an *RcloneError (and so any error string or log line built from one).
//
// rclone echoes the remote it was given in its diagnostics — verbatim ("ERROR :
// :s3,access_key_id=…,secret_access_key=…:bkt/k is a directory…") and Go-quoted
// (the CRITICAL "Failed to create file system for \"…\"" line and -vv output) —
// and it re-prints a rejected option value on its own. An inline
// ":backend,key=value,…:" connection string carries credentials in those values,
// so the redactor:
//
//   - replaces the whole inline spec ":backend,params:" (verbatim and Go-quoted)
//     with ":backend,<redacted>:", keeping the backend name and the path;
//   - replaces every parameter value, wherever it appears on its own, in its raw
//     (possibly quoted), unquoted and Go-escaped spellings. This over-redacts a
//     value that also occurs as ordinary text (e.g. "true"); stderr is
//     diagnostic only, so losing a word is the right side to err on;
//   - replaces the --config path, which is never to be surfaced.
//
// A named remote carries no inline parameter (the named-remote grammar has no
// ','), so only the config path applies to it. The zero redactor is a no-op.
type redactor struct {
	spec  []specNeedle // whole-spec spellings, replaced first
	plain []string     // values and the config path, longest first
}

type specNeedle struct {
	find    string
	replace string
}

// newRedactor derives the redaction set from the parsed remote and config path.
func newRedactor(remote parsedRemote, configPath string) redactor {
	var r redactor
	if remote.connectionString {
		if params, ok := strings.CutPrefix(remote.spec, remote.backend+","); ok && params != "" {
			full := ":" + remote.spec + ":"
			replacement := ":" + remote.backend + "," + redactedMark + ":"
			r.spec = append(r.spec, specNeedle{find: full, replace: replacement})
			if quoted := goEscaped(full); quoted != full {
				r.spec = append(r.spec, specNeedle{find: quoted, replace: replacement})
			}
			r.plain = append(r.plain, paramValues(params)...)
		}
	}
	if configPath != "" {
		r.plain = append(r.plain, configPath, goEscaped(configPath))
	}
	r.plain = uniqueNonEmptyLongestFirst(r.plain)
	return r
}

// apply returns s with every needle replaced: whole-spec spellings first (so the
// backend name survives), then values and the config path, longest first.
func (r redactor) apply(s string) string {
	for _, n := range r.spec {
		s = strings.ReplaceAll(s, n.find, n.replace)
	}
	for _, n := range r.plain {
		s = strings.ReplaceAll(s, n, redactedMark)
	}
	return s
}

// margin is the longest needle. A bounded stderr capture that began mid-needle
// holds at most margin-1 bytes of it at its very start; captureStderr drops that
// many leading bytes so no fragment of a secret survives truncation.
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

// redactTail redacts a bounded stderr capture. truncated reports that the
// capture lost bytes at its start, in which case a secret may have been cut: the
// first margin bytes are dropped AFTER redaction (a cut needle's remnant sits
// there and is shorter than margin; complete needles were already replaced),
// then the result is bounded to limit bytes.
func (r redactor) redactTail(captured []byte, truncated bool, limit int) string {
	s := r.apply(string(captured))
	if truncated {
		s = s[min(r.margin(), len(s)):]
	}
	if len(s) > limit {
		s = s[len(s)-limit:]
	}
	return s
}

// paramValues returns every spelling of each "key=value" parameter value in an
// inline spec's parameter list: the raw value, and for a quoted value its
// unquoted content. Parameters are separated by unquoted commas; a value is
// single- or double-quoted with the active quote escaped by doubling (the same
// grammar parseRemote scans). A bare "key" parameter has no value. Each value is
// also returned Go-escaped, the form rclone's %q diagnostics print.
func paramValues(params string) []string {
	var values []string
	for _, param := range splitUnquoted(params, ',') {
		_, raw, ok := strings.Cut(param, "=")
		if !ok || raw == "" {
			continue
		}
		values = append(values, raw, goEscaped(raw))
		if q := raw[0]; (q == '\'' || q == '"') && len(raw) >= 2 && raw[len(raw)-1] == q {
			inner := strings.ReplaceAll(raw[1:len(raw)-1], string([]byte{q, q}), string(q))
			values = append(values, inner, goEscaped(inner))
		}
	}
	return values
}

// splitUnquoted splits s on sep outside single- or double-quoted runs (a doubled
// active quote stays inside the run).
func splitUnquoted(s string, sep byte) []string {
	var parts []string
	start, quote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			if ch == quote {
				if i+1 < len(s) && s[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// goEscaped is s as it appears inside a Go %q string (without the quotes).
func goEscaped(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

func uniqueNonEmptyLongestFirst(in []string) []string {
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
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
