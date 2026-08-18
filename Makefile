.PHONY: build test test-race lint vet audit secrets verify ci tidy format cross-build docs-drift install clean build-mcp install-mcp build-all docs-deps docs-gen docs-build docs-serve demos hooks

# Stamp local builds the way GoReleaser stamps releases (.goreleaser.yaml uses
# -X zotio/internal/cli.version). Without this every locally built binary
# reports "dev", so `zotio version` cannot tell you what you are running and a
# stale install is invisible.
# The leading "v" is stripped because GoReleaser's {{ .Version }} has none:
# a release binary says "0.19.0", so a local one must not say "v0.19.0".
# "-dirty" is kept deliberately - an uncommitted build should admit it.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -s -w -X zotio/internal/cli.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/zotio ./cmd/zotio

# Fast inner loop. Structurally cannot see data races - see test-race.
test:
	go test ./...

lint:
	golangci-lint run

vet:
	go vet ./...

# Dependency vulnerability scan (deterministic; the source of truth for dep-risk).
audit:
	govulncheck ./...

# Secret scan; no-op-skips when betterleaks is absent so local `verify` never blocks
# on a missing tool (CI installs it explicitly).
secrets:
	@if command -v betterleaks >/dev/null 2>&1; then betterleaks git . --no-banner --redact; \
	else echo "betterleaks not installed; skipping (CI still checks)"; fi

# --- CI parity -------------------------------------------------------------
# Each target below mirrors one job in .github/workflows/ci.yml. A gate that
# exists only in CI is one you discover after pushing: v0.19.0 shipped three
# defects this machine could not see, including a data race and a Linux-only
# path, because the local suite was a strict subset of the remote one.

# ci.yml: tidy. Rewrites go.mod/go.sum, then fails if that changed anything.
tidy:
	go mod tidy
	git diff --exit-code go.mod go.sum
	go mod verify

# ci.yml: format
format:
	@out="$$(gofmt -l .)"; test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

# ci.yml: docs-drift. Regenerates first, so it fails only on real drift.
docs-drift: docs-gen
	git diff --exit-code docs/reference

# ci.yml: test, ubuntu leg. The race detector is the one gate whose absence
# locally is not merely slower but blind: a green plain suite proves nothing
# about concurrent access.
test-race:
	go test -race ./...

# ci.yml: cross-build. GoReleaser ships 6 targets; the host build never
# compiles the other platforms' files, so a platform-only break reaches the
# release instead of CI. Subsumes `vet` for the host platform too.
cross-build:
	@set -e; for goos in darwin linux windows; do \
	  for goarch in amd64 arm64; do \
	    echo "==> $$goos/$$goarch"; \
	    GOOS=$$goos GOARCH=$$goarch go build ./...; \
	    GOOS=$$goos GOARCH=$$goarch go vet ./...; \
	  done; \
	done

# Every ci.yml gate, in roughly ascending cost. Run before pushing.
ci: tidy format lint test-race cross-build docs-drift secrets

# The full local gate: CI plus the dependency scan from vuln.yml. `vet` is not
# listed because cross-build already runs it for all six targets.
verify: ci audit

# Enable the committed git hooks (.githooks/) for this clone. One-time setup;
# pre-commit = identity guard + gofmt + staged secret scan, pre-push = vet.
# CI remains the authority.
hooks:
	git config core.hooksPath .githooks

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/zotio

clean:
	rm -rf bin/

build-mcp:
	go build -ldflags "$(LDFLAGS)" -o bin/zotio-mcp ./cmd/zotio-mcp

install-mcp:
	go install -ldflags "$(LDFLAGS)" ./cmd/zotio-mcp

build-all: build build-mcp

# --- Documentation site (Zensical; reads mkdocs.yml) ----------------------

# Install the pinned docs toolchain (Zensical). Use a venv in real setups.
docs-deps:
	pip install -r docs/requirements.txt

# Regenerate the code-generated reference pages (docs/reference/*) from the
# binary. Drift-gated in CI — run after any command/flag change.
docs-gen:
	go run ./cmd/docs-gen

# Build the static site into ./site (regenerates reference first).
docs-build: docs-gen
	zensical build

# Live-preview the site locally (regenerates reference first).
docs-serve: docs-gen
	zensical serve

# --- Demo GIFs (VHS; https://github.com/charmbracelet/vhs) -----------------

# Re-record the demo GIFs and the wrapped share card against the
# deterministic demo sandbox. Requires `vhs` (brew install vhs) and network
# for the retract-check tape. Card year pinned to the fixture's data spread.
demos: build
	ZOTIO_DEMO=1 ./bin/zotio demo --reset > /dev/null
	mkdir -p docs/assets/demos
	ZOTIO_DEMO=1 ./bin/zotio library wrapped --year 2026 --card docs/assets/demos/wrapped-card.svg --card-style cycle > /dev/null
	cd docs/tapes && for t in *.tape; do vhs $$t; done
