.PHONY: build test lint vet audit secrets verify install clean build-mcp install-mcp build-all

build:
	go build -o bin/zotio ./cmd/zotio

test:
	go test ./...

lint:
	golangci-lint run

vet:
	go vet ./...

# Dependency vulnerability scan (deterministic; the source of truth for dep-risk).
audit:
	govulncheck ./...

# Secret scan; no-op-skips when gitleaks is absent so local `verify` never blocks
# on a missing tool (CI installs it explicitly).
secrets:
	@if command -v gitleaks >/dev/null 2>&1; then gitleaks detect --no-banner --redact; \
	else echo "gitleaks not installed; skipping (CI still checks)"; fi

# One reproducible quality gate shared by humans, CI, and glean's verify gate.
verify: vet lint test audit secrets

install:
	go install ./cmd/zotio

clean:
	rm -rf bin/

build-mcp:
	go build -o bin/zotio-mcp ./cmd/zotio-mcp

install-mcp:
	go install ./cmd/zotio-mcp

build-all: build build-mcp
