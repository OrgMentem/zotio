.PHONY: build test lint vet audit secrets verify install clean build-mcp install-mcp build-all docs-deps docs-gen docs-build docs-serve demos

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

# Secret scan; no-op-skips when betterleaks is absent so local `verify` never blocks
# on a missing tool (CI installs it explicitly).
secrets:
	@if command -v betterleaks >/dev/null 2>&1; then betterleaks git . --no-banner --redact; \
	else echo "betterleaks not installed; skipping (CI still checks)"; fi

# One reproducible quality gate shared by humans and CI.
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
	ZOTIO_DEMO=1 ./bin/zotio library wrapped --year 2026 --card docs/assets/demos/wrapped-card.svg > /dev/null
	cd docs/tapes && for t in *.tape; do vhs $$t; done
