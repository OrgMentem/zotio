.PHONY: build test test-race lint vet audit secrets verify ci tidy format cross-build docs-drift notices notices-drift lockstep install clean build-mcp install-mcp build-all docs-deps docs-gen docs-build docs-serve demos hooks

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

# ci.yml: notices-drift. Regenerates the third-party attribution file first, so
# it fails only on real drift — i.e. when a dependency was added, removed, or
# bumped without regenerating. Every release channel ships this file, so drift
# means shipping incomplete attribution.
notices-drift: notices
	git diff --exit-code THIRD_PARTY_LICENSES.txt

# ci.yml: tidy, lockstep step. modernc.org/sqlite carries a transpiled C
# runtime whose ABI is coupled to one exact modernc.org/libc revision; a
# `require` line is a floor, not an equality, so MVS happily selects a higher
# libc that builds cleanly and then faults at run time (upstream reports SIGBUS
# in the WAL index under concurrent access). See gitlab.com/cznic/sqlite#177.
lockstep:
	@selected="$$(go list -m -f '{{.Version}}' modernc.org/libc)"; \
	pinned="$$(awk '$$1=="modernc.org/libc"{print $$2; exit} $$1=="require" && $$2=="modernc.org/libc"{print $$3; exit}' "$$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)")"; \
	if [ "$$selected" != "$$pinned" ]; then \
		echo "modernc.org/libc lockstep broken: selected $$selected, modernc.org/sqlite pins $$pinned"; \
		echo "bump modernc.org/sqlite and modernc.org/libc together in one commit"; \
		exit 1; \
	fi; \
	echo "modernc.org/libc lockstep ok ($$selected)"

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

# The MCP Registry rejects a description over 100 characters with a 422, and it
# does so in the publish_registry job — after GoReleaser has already shipped the
# GitHub release, Homebrew, Scoop and WinGet. v0.22.0 failed exactly there, and a
# tagged commit cannot be re-published, so 0.22.0 is permanently absent from the
# registry. The string lives in scripts/gen_server_json.py and was grown from 89
# to 111 characters by a docs commit that touched 14 unrelated files. This target
# runs the generator's own validation here instead, where a fix is still free.
registry-manifest:
	@python3 -c "import importlib.util, sys; \
	spec = importlib.util.spec_from_file_location('g', 'scripts/gen_server_json.py'); \
	g = importlib.util.module_from_spec(spec); spec.loader.exec_module(g); \
	n = len(g.DESCRIPTION); \
	sys.exit('registry description is %d characters; limit is %d' % (n, g.DESCRIPTION_LIMIT)) \
	  if n > g.DESCRIPTION_LIMIT else print('registry description ok (%d/%d chars)' % (n, g.DESCRIPTION_LIMIT))"

# Every ci.yml gate, in roughly ascending cost. Run before pushing.
ci: tidy lockstep format lint test-race cross-build docs-drift notices-drift registry-manifest secrets

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

# Regenerate THIRD_PARTY_LICENSES.txt from the modules the released binaries
# link, across every release target. Drift-gated in CI — run after any
# dependency change.
notices:
	go run ./cmd/notices-gen

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
