# Container image for zotio-mcp, the Zotero MCP server (stdio transport).
# Used by Glama to build, security-scan, introspect tools, and let users deploy.
# Tool listing needs no ZOTERO_API_KEY (reads are keyless; the key only unlocks
# writes and group libraries), so introspection works out of the box.
FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binary (matches the release build): no libc, runs on distroless/static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zotio-mcp ./cmd/zotio-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/zotio-mcp /usr/local/bin/zotio-mcp
# Default transport is stdio — the channel Glama (and Claude Desktop) speak.
ENTRYPOINT ["/usr/local/bin/zotio-mcp"]
