# CLAUDE.md — rclonestore

`rclonestore` implements `storage.Blobs` by driving the **external `rclone` binary**. It is a
thin, security-hardened exec adapter — nothing more.

## Dependencies

- **stdlib + `github.com/looprig/storage` ONLY. No other third-party dependency, ever.**
  Do not `go get` anything. Do not add a `require` line beyond storage.
  (`make secure`'s staticcheck/gosec/govulncheck are PATH-resolved dev-tool binaries, not
  go.mod dependencies — this does not contradict the rule above.)
- **Never link librclone / cgo.** rclone is driven as a subprocess (argv exec) only. Linking
  the library would pull in a giant dependency tree and defeat the extraction.

## Security (non-negotiable)

- **argv exec only — never a shell string.** Use `exec.CommandContext`; pass every argument as
  a separate element. No `sh -c`, no string interpolation of any external value.
- **`--` before positionals.** Insert `--` exactly once, immediately before the positional
  (path) arguments, so no key or remote path starting with `-` can be mistaken for a flag.
- **Every call is `context.Context`-bounded.** No unbounded blocking; on ctx timeout/cancel the
  subprocess is killed.
- **No secrets in errors or logs.** rclone config may embed remote credentials — reference it by
  path only; never parse, copy, or log it. Capture only a bounded TAIL of stderr into typed
  errors. Never put the config path, the remote, or any credential-bearing value into an error
  message or a log line — only the rclone subcommand (and, at the Blobs layer, the storage key).
- **rclone's stderr DOES echo the remote** — verbatim and Go-quoted, inline `key=value`
  credentials included (measured: `deletefile`, the CRITICAL `Failed to create file system for
  "…"` line, `-vv`). Every captured stderr byte therefore goes through `redactor` (redact.go)
  before it reaches an `RcloneError`: the inline spec becomes `:backend,<redacted>:`, every
  parameter value and the config path become `<redacted>`, and a secret cut by the tail bound is
  dropped. Never surface stderr (or stdout) through any other path.

## Code rules

- **All errors are typed** concrete structs with `Error()` (and `Unwrap()` when they carry a
  cause). Callers classify with `errors.As`, never by string.
- **Strict typing.** No `any`/`interface{}` except at serialization boundaries, narrowed
  immediately. No untyped magic numbers/strings.
- Return errors explicitly; never swallow with `_`. Contracts first, then implementation.

## Testing & build

- **Table-driven tests, mandatory**, each with `t.Parallel()`. Cover happy path, boundaries,
  error cases, and domain edges.
- **Always `-race`:** `GOWORK=off go test -race ./...`. `gofmt`- and `go vet`-clean at all times.
- Blobs conformance runs against a real `rclone` + temp remote under `//go:build integration`.
- **Every Go command runs with `GOWORK=off`** — the `go.work` at `~/code` must not capture this
  module.
- `make check` (fmt-check + vet + `-race` test) before every commit.
