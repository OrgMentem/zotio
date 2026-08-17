# Container image for zotio-mcp, the Zotero MCP server (stdio transport).
# Used by Glama to build, security-scan, introspect tools, and let users deploy.
# Tool listing needs no ZOTERO_API_KEY (reads are keyless; the key only unlocks
# writes and group libraries), so introspection works out of the box.
FROM golang:1.26-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
ARG VERSION=dev
WORKDIR /src
# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binary (matches the release build): no libc, runs on distroless/static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X zotio/internal/cli.version=${VERSION}" -o /out/zotio-mcp ./cmd/zotio-mcp

FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
COPY --from=build /out/zotio-mcp /usr/local/bin/zotio-mcp
# Default transport is stdio — the channel Glama (and Claude Desktop) speak.
ENTRYPOINT ["/usr/local/bin/zotio-mcp"]
