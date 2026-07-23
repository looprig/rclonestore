.PHONY: test test-integration fmt fmt-check vet gosec staticcheck vuln check secure

# Every Go command runs with GOWORK=off so the parent go.work never captures this
# module (see CLAUDE.md).

# Module's own package dirs, resolved with GOWORK=off for the same reason as every
# other Go command here (see CLAUDE.md).
GO_DIRS := $(shell GOWORK=off go list -f '{{.Dir}}' ./...)

test:
	GOWORK=off go test -race ./...

test-integration:
	GOWORK=off go test -tags integration -race ./...

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

check: fmt-check vet gosec test

secure: fmt-check vet staticcheck gosec vuln
