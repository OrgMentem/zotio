#!/usr/bin/env python3
# Generate server.json for the Official MCP Registry from the built MCPB
# bundles.
#
#   python3 scripts/gen_server_json.py <version> [--mcpb-dir dist/mcpb] [--out server.json]
#
# Each dist/mcpb/zotio-mcp_<version>_<goos>_<goarch>.mcpb becomes an `mcpb`
# package whose identifier is its release download URL and whose fileSha256 is
# computed here — so the registry entry always matches the assets that actually
# shipped. Run this after `goreleaser release` (which packs dist/mcpb via the
# zotio-mcp post-build hook) and before `mcp-publisher publish`.
import argparse
import hashlib
import json
import re
import sys
from pathlib import Path

SCHEMA = "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json"
NAMESPACE = "io.github.OrgMentem/zotio"
REPO_URL = "https://github.com/OrgMentem/zotio"
DL_BASE = "https://github.com/OrgMentem/zotio/releases/download"

# Optional for read-only local-desktop use (the local API needs no key); set it
# to enable writes and reach group libraries. Mirrors the mcpb manifest's
# ZOTERO_API_KEY user_config.
ENV_VARS = [
    {
        "name": "ZOTERO_API_KEY",
        "description": (
            "Zotero Web API key. Optional for read-only use against the local "
            "Zotero desktop app; required to enable writes and reach group "
            "libraries."
        ),
        "isRequired": False,
        "isSecret": True,
    }
]

MCPB_RE = re.compile(r"^zotio-mcp_(?P<version>.+)_(?P<goos>[a-z0-9]+)_(?P<goarch>[a-z0-9]+)\.mcpb$")


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("version", help="release version without leading v, e.g. 0.7.0")
    ap.add_argument("--mcpb-dir", default="dist/mcpb")
    ap.add_argument("--out", default="server.json")
    args = ap.parse_args()

    version = args.version.lstrip("v")
    mcpb_dir = Path(args.mcpb_dir)
    bundles = sorted(mcpb_dir.glob("*.mcpb"))
    if not bundles:
        sys.exit(f"no .mcpb bundles in {mcpb_dir} — run goreleaser first")

    packages = []
    for path in bundles:
        m = MCPB_RE.match(path.name)
        if not m:
            sys.exit(f"unexpected mcpb name: {path.name}")
        if m["version"] != version:
            sys.exit(f"{path.name} version {m['version']} != release {version}")
        packages.append(
            {
                "registryType": "mcpb",
                "identifier": f"{DL_BASE}/v{version}/{path.name}",
                "fileSha256": sha256(path),
                "transport": {"type": "stdio"},
                "environmentVariables": ENV_VARS,
            }
        )

    doc = {
        "$schema": SCHEMA,
        "name": NAMESPACE,
        "title": "zotio",
        "description": "Zotero MCP server: preview-first, journaled writes and keyless local reads for AI agents.",
        "websiteUrl": REPO_URL,
        "repository": {"url": REPO_URL, "source": "github"},
        "version": version,
        "packages": packages,
    }
    Path(args.out).write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
    print(f"wrote {args.out}: v{version}, {len(packages)} packages")


if __name__ == "__main__":
    main()
