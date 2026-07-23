# Contributing to looprig/rclonestore

Thanks for considering a contribution. `rclonestore` implements `storage.Blobs` by driving
the external `rclone` binary as a subprocess. It is a thin, security-hardened exec adapter —
nothing more — and it is part of the looprig multi-module ecosystem. This file is the short
guide for working in *this* repository.

## Before you write code

1. Read [`CLAUDE.md`](CLAUDE.md) (a.k.a. `AGENTS.md`). It is the authoritative source for the
   design, security, dependency, build, and code rules this module follows. PRs that
   contradict it will be asked to change.
2. Skim recent files in [`docs/plans/`](docs/plans/) (e.g.
   [`2026-07-12-storage-path-reporting-design.md`](docs/plans/2026-07-12-storage-path-reporting-design.md))
   for the design-doc style the project uses. Non-trivial work should get a short design doc
   in `docs/plans/` before implementation, named `YYYY-MM-DD-<topic>-design.md`.
3. Open an issue for anything non-trivial so we can agree on direction before you spend the
   time.

## Design and security rules (the short version)

- **stdlib + `github.com/looprig/storage` ONLY. No other third-party dependency, ever.**
  This is not a soft preference — it is a hard, permanent constraint on this module. Do not
  `go get` anything. Do not add a `require` line beyond `storage`. Do not modify `go.mod` at
  all without prior agreement, and even then the answer is almost always no. If a task seems
  to need an external package, the task is wrong for this module.
- **Never link librclone or use cgo.** rclone is driven purely as a subprocess (argv exec).
  Linking the library would pull in a large dependency tree and defeat the point of the
  extraction.
- **argv exec only — never a shell string.** Build commands with `exec.CommandContext` and
  pass every argument as a separate slice element. No `sh -c`, no string interpolation of any
  external value into a command line.
- **`--` before positionals.** Insert `--` exactly once, immediately before positional (path)
  arguments, so no key or remote path that happens to start with `-` can be mistaken for a
  flag.
- **Every call is `context.Context`-bounded.** No unbounded blocking on the subprocess; on
  ctx timeout/cancel the subprocess is killed.
- **No secrets in errors or logs.** rclone's config file may embed remote credentials.
  Reference it by path only — never parse, copy, or log its contents. Capture only a bounded
  tail of stderr into typed errors, and never put the config path, the remote name, or any
  credential-bearing value into an error message or log line (only the rclone subcommand and,
  at the `Blobs` layer, the storage key are safe to surface).
- **All errors are typed** concrete structs with `Error()` (and `Unwrap()` when they carry a
  cause). Callers classify with `errors.As`, never by string matching.
- **Strict typing.** No `any`/`interface{}` except at serialization boundaries, narrowed
  immediately. No untyped magic numbers or strings.
- Return errors explicitly; never swallow with `_`. Contracts first, then implementation.

## Build, test, and secure

Run these before pushing. CI runs the same.

```sh
make fmt             # gofmt the module in place
make fmt-check       # fail if any tracked file isn't gofmt-clean
make vet             # go vet ./...
make test            # go test -race ./...            (always -race)
make test-integration # go test -tags integration -race ./... (needs a real rclone binary)
make gosec           # gosec security scan
make staticcheck     # staticcheck lint
make vuln            # govulncheck vulnerability scan
make check           # fmt-check + vet + gosec + test        (pre-commit baseline)
make secure          # fmt-check + vet + staticcheck + gosec + vuln (full security suite)
```

`staticcheck`, `gosec`, and `govulncheck` are **not** module dependencies — the "stdlib +
storage ONLY, ever" rule above forbids adding anything to `go.mod` for them. Instead, each is
invoked as an external binary resolved from `PATH` or `$(go env GOPATH)/bin` at `make` time.
If a binary isn't installed, its target prints a short notice and skips cleanly rather than
failing, so `make check`/`make secure` stay green on machines that don't have every tool
installed. Install what you're missing with:

```sh
go install github.com/securego/gosec/v2/cmd/gosec@latest
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

Every Go command in the `Makefile` runs with `GOWORK=off` so the `go.work` at the workspace
root never captures this module — keep that when adding or editing targets.

## Tests

- **Table-driven tests, mandatory**, each subtest calling `t.Parallel()`. Cover the happy
  path, boundary values, error cases, and domain edge cases.
- `make test` runs the unit suite with `-race`; a test that needs `-race` to fail is not
  passing.
- `make test-integration` runs the `Blobs` conformance suite against a real `rclone` binary
  and a temp remote, under the `//go:build integration` tag. It needs `rclone` on `PATH` (or
  configured via the usual `rclonestore.Options`) and is not part of the default `make test`
  or `make check` run.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR. If a change spans modules, open a PR per module; the `replace`
  directive in `go.mod` lets this module build against a local `storage` checkout.
- Write a clear description: what, why, the design alternative you rejected, and how you
  verified. `make check` (or `make secure`) output is welcome in the PR body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, credentials, or a real rclone config. Don't add a new
  external dependency — see the dependency rule above; it applies with no exceptions.
- Don't update `CLAUDE.md`, `Makefile`, or `go.mod` unless the change is the point of the PR.

## Reporting security issues

There is no `SECURITY.md` in this repository yet. This module execs an external binary with
attacker-adjacent input (paths, remote names) and its config may embed remote credentials, so
if you find an exec-safety issue (e.g. a way to smuggle a flag or shell metacharacter into an
`rclone` invocation) or a credential-handling issue (a path where config contents or secrets
could reach a log or error message), please report it privately rather than opening a public
issue, until a formal `SECURITY.md` exists.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful; personal attacks,
harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the Apache License
2.0, as described in [`LICENSE`](LICENSE).
