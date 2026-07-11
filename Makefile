.PHONY: test test-integration fmt-check vet gosec check

# Every Go command runs with GOWORK=off so the parent go.work never captures this
# module (see CLAUDE.md).

test:
	GOWORK=off go test -race ./...

test-integration:
	GOWORK=off go test -tags integration -race ./...

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

check: fmt-check vet gosec test
