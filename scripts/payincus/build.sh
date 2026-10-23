#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
go_bin="${GO:-go}"
arch="${GOARCH:-$($go_bin env GOARCH)}"
dest="${BUILD_DIR:-$root/scripts/payincus/dist/$arch}"
mkdir -p "$dest"
python3 scripts/payincus/prepare-xray.py "$arch"
# Use the official native build dependencies and preserve upstream packaging and guest agent layout.
CGO_ENABLED=1 "$go_bin" build -tags libsqlite3 -o "$dest/incusd" ./cmd/incusd
CGO_ENABLED=1 "$go_bin" build -tags libsqlite3 -o "$dest/incus" ./cmd/incus
CGO_ENABLED=1 "$go_bin" build -tags libsqlite3 -o "$dest/incus-user" ./cmd/incus-user
CGO_ENABLED=0 "$go_bin" build -trimpath -o "$dest/incus-agent" ./cmd/incus-agent
CGO_ENABLED=0 "$go_bin" build -trimpath -o "$dest/payincus-agent" ./cmd/payincus-agent
CGO_ENABLED=0 "$go_bin" build -trimpath -o "$dest/payincus-network-worker" ./cmd/payincus-network-worker
cp internal/server/payincus/LICENSE.payincus "$dest/LICENSE.payincus"
cp internal/server/payincus/NOTICE "$dest/NOTICE.payincus"
cp internal/server/payincus/egress/assets/LICENSE "$dest/LICENSE.xray"
cp internal/server/payincus/egress/assets/README.txt "$dest/NOTICE.xray"
printf 'Binaries staged in %s; assemble with the native Incus package layout documented in doc/packaging.md.\n' "$dest"
