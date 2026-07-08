#!/usr/bin/env python3
# Package zotio-mcp as per-platform MCPB bundles.
#
# Invoked by goreleaser as a post-build hook for the zotio-mcp build:
#   python3 scripts/mcpb_pack.py <binary_path> <goos> <goarch> <version>
#
# An .mcpb is a zip with manifest.json at the root and the server binary at
# the manifest's entry_point. Each platform gets its own bundle whose
# manifest pins the release version, the platform, and (on Windows) the
# .exe entry point.
import json
import os
import sys
import zipfile

GOOS_TO_MCPB = {"darwin": "darwin", "linux": "linux", "windows": "win32"}


def main() -> None:
    binary_path, goos, goarch, version = sys.argv[1:5]
    platform = GOOS_TO_MCPB[goos]
    exe = ".exe" if goos == "windows" else ""

    with open("manifest.json", encoding="utf-8") as f:
        manifest = json.load(f)

    manifest["version"] = version
    manifest["server"]["entry_point"] = f"bin/zotio-mcp{exe}"
    manifest["server"]["mcp_config"]["command"] = "${__dirname}/bin/zotio-mcp" + exe
    manifest["compatibility"]["platforms"] = [platform]

    out_dir = os.path.join("dist", "mcpb")
    os.makedirs(out_dir, exist_ok=True)
    out_path = os.path.join(out_dir, f"zotio-mcp_{version}_{goos}_{goarch}.mcpb")

    with zipfile.ZipFile(out_path, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.writestr("manifest.json", json.dumps(manifest, indent=2) + "\n")
        info = zipfile.ZipInfo(f"bin/zotio-mcp{exe}")
        info.external_attr = 0o755 << 16  # executable bit survives extraction
        info.compress_type = zipfile.ZIP_DEFLATED
        with open(binary_path, "rb") as bf:
            zf.writestr(info, bf.read())

    print(f"packed {out_path}")


if __name__ == "__main__":
    main()
