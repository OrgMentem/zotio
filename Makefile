.PHONY: build test lint install clean

build:
	go build -o bin/zotero-pp-cli ./cmd/zotero-pp-cli

test:
	go test ./...

lint:
	golangci-lint run

install:
	go install ./cmd/zotero-pp-cli

clean:
	rm -rf bin/

build-mcp:
	go build -o bin/zotero-pp-mcp ./cmd/zotero-pp-mcp

install-mcp:
	go install ./cmd/zotero-pp-mcp

build-all: build build-mcp
