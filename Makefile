.PHONY: test test-integration fmt fmt-check vet gosec staticcheck vuln check secure

# Every Go command runs with GOWORK=off so the parent go.work never captures this
# module (see CLAUDE.md).

# Module's own package dirs, resolved with GOWORK=off for the same reason as every
# other Go command here (see CLAUDE.md).
GO_DIRS := $(shell GOWORK=off go list -f '{{.Dir}}' ./...)

# Most unit tests execute a short-lived fake rclone process. Keep both package
# and in-package test scheduling serial so a high-core or PID-constrained runner
# cannot turn that intentional subprocess coverage into fork/exec EAGAIN flakes.
GO_TEST_CONCURRENCY := -p=1 -parallel=1

test:
	GOWORK=off go test -race $(GO_TEST_CONCURRENCY) ./...

test-integration:
	GOWORK=off go test -tags integration -race $(GO_TEST_CONCURRENCY) ./...

fmt:
	gofmt -w $(GO_DIRS)

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

vet:
	GOWORK=off go vet ./...

# gosec scans for security issues (notably the G204 argv exec in runner.go, which
# carries a reviewed #nosec). It is NOT a module dependency — CLAUDE.md forbids
# adding anything beyond storage to go.mod — so it is invoked as an external
# binary resolved from PATH or GOPATH/bin. If neither has it, the target warns and
# skips so `make check` stays green where gosec is not installed; install with:
#   go install github.com/securego/gosec/v2/cmd/gosec@latest
gosec:
	@GOSEC=$$(command -v gosec || echo "$$(go env GOPATH)/bin/gosec"); \
	if [ -x "$$GOSEC" ]; then \
		echo "gosec: $$GOSEC"; GOWORK=off "$$GOSEC" -quiet ./...; \
	else \
		echo "gosec not installed; skipping (go install github.com/securego/gosec/v2/cmd/gosec@latest)"; \
	fi

# staticcheck lints for bugs and style issues. It is NOT a module dependency —
# CLAUDE.md forbids adding anything beyond storage to go.mod — so it is invoked
# as an external binary resolved from PATH or GOPATH/bin. If neither has it, the
# target warns and skips so `make secure` stays green where staticcheck is not
# installed; install with:
#   go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck:
	@STATICCHECK=$$(command -v staticcheck || echo "$$(go env GOPATH)/bin/staticcheck"); \
	if [ -x "$$STATICCHECK" ]; then \
		echo "staticcheck: $$STATICCHECK"; GOWORK=off "$$STATICCHECK" ./...; \
	else \
		echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; \
	fi

# govulncheck scans for known vulnerabilities in the dependency graph. It is NOT
# a module dependency — CLAUDE.md forbids adding anything beyond storage to
# go.mod — so it is invoked as an external binary resolved from PATH or
# GOPATH/bin. If neither has it, the target warns and skips so `make secure`
# stays green where govulncheck is not installed; install with:
#   go install golang.org/x/vuln/cmd/govulncheck@latest
vuln:
	@GOVULNCHECK=$$(command -v govulncheck || echo "$$(go env GOPATH)/bin/govulncheck"); \
	if [ -x "$$GOVULNCHECK" ]; then \
		echo "govulncheck: $$GOVULNCHECK"; GOWORK=off "$$GOVULNCHECK" ./...; \
	else \
		echo "govulncheck not installed; skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
	fi


secure: fmt-check vet staticcheck gosec vuln

# --- standardized check surface -------------------------------------------
# One target, the same set of checks, in every module. CI calls exactly this,
# so a check can no longer pass locally and be silently absent in CI (or the
# reverse). The lint/security tools are run at a pinned version with `go run`, which adds nothing to go.mod:
# this module's CLAUDE.md forbids any go.mod dependency beyond its one
# import, and sanctions dev-tool BINARIES instead.
#
# CHECK_GO_DIRS scopes gosec: gosec is NOT module-aware, so a bare ./... is a
# filesystem walk that descends into nested .worktrees/ checkouts, which are
# separate modules. go vet and staticcheck are module-aware and need no scope.
CHECK_GO_DIRS = $(shell GOWORK=off go list -f '{{.Dir}}' ./...)
# CHECK_GO_FILES is what gofmt gets. Never hand it CHECK_GO_DIRS: gofmt RECURSES
# into directory operands, so for a module with a root package it would walk the
# whole tree, nested .worktrees/ checkouts included.
CHECK_GO_FILES = $(foreach dir,$(CHECK_GO_DIRS),$(wildcard $(dir)/*.go))

check-staticcheck:
	GOWORK=off go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...

check-gosec:
	GOWORK=off go run github.com/securego/gosec/v2/cmd/gosec@v2.28.0 -quiet $(CHECK_GO_DIRS)

check-vuln:
	GOWORK=off go mod verify
	GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

build:
	GOWORK=off go build ./...

check: fmt-check vet check-staticcheck check-gosec check-vuln test build

.PHONY: check check-staticcheck check-gosec check-vuln fmt fmt-check vet test build
