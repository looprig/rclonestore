# rclonestore

`rclonestore` implements storekit's **Blobs** primitive — content-addressed immutable byte
objects — by **exec'ing the external `rclone` binary** (`rcat`/`cat`/`deletefile`/`lsf`),
giving looprig's workspace store a cloud-agnostic backend across any of rclone's many local
and cloud remotes without ever linking librclone (whose dependency tree would defeat the
point of the extraction). It drives rclone via an argv list only — never a shell string —
with `--` inserted before positional path arguments so no key can be mistaken for a flag,
every call bounded by a `context.Context`, and rclone's config (which may embed remote
credentials) referenced by path only: never parsed, copied, logged, or placed in an error.
It depends only on the Go standard library and `github.com/ciram-co/storekit`.
