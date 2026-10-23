#!/usr/bin/env python3
"""Fetch the pinned Xray release, verify both digests, embed one architecture.

Only release archives are downloaded. No installer, shell script or remote
configuration is executed. Source-only go test does not require this step.
"""
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import urllib.request
import zipfile

root = Path(__file__).resolve().parents[2]
assets = root / "internal" / "server" / "payincus" / "egress" / "assets"
manifest = json.loads((assets / "manifest.json").read_text())
arch = sys.argv[1] if len(sys.argv) == 2 else ""
if arch not in manifest["files"]:
    raise SystemExit("Usage: prepare-xray.py amd64|arm64")
entry = manifest["files"][arch]
cache = Path(__file__).resolve().parent / ".cache" / "xray"
cache.mkdir(parents=True, exist_ok=True)
archive = cache / f"{manifest['version']}-{entry['asset']}"
if not archive.exists() or hashlib.sha256(archive.read_bytes()).hexdigest() != entry["archiveSha256"]:
    url = f"https://github.com/XTLS/Xray-core/releases/download/{manifest['version']}/{entry['asset']}"
    with urllib.request.urlopen(url, timeout=120) as response:
        data = response.read(128 * 1024 * 1024)
    if hashlib.sha256(data).hexdigest() != entry["archiveSha256"]:
        raise SystemExit("Xray release archive SHA-256 mismatch")
    temporary = archive.with_suffix(".tmp")
    temporary.write_bytes(data)
    os.replace(temporary, archive)
with zipfile.ZipFile(io.BytesIO(archive.read_bytes())) as bundle:
    if bundle.getinfo("xray").file_size > 128 * 1024 * 1024:
        raise SystemExit("Xray binary exceeds size limit")
    binary = bundle.read("xray")
if hashlib.sha256(binary).hexdigest() != entry["binarySha256"]:
    raise SystemExit("Xray executable SHA-256 mismatch")
for payload in assets.glob("xray-linux-*.gz"):
    payload.unlink()  # Only this script's generated, ignored payloads.
(assets / f"xray-linux-{arch}.gz").write_bytes(gzip.compress(binary, mtime=0))
print(f"Prepared Xray {manifest['version']} ({arch}), both SHA-256 checks passed")
