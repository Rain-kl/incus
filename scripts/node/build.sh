#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
go_bin="${GO:-go}"
arch="${GOARCH:-$($go_bin env GOARCH)}"
dest="${BUILD_DIR:-$root/scripts/node/dist/$arch}"
mkdir -p "$dest"
# Incus: internal distribution identity is independent of the upstream Incus version.
internal_version="$(cat scripts/node/VERSION)"
source_sha="${SOURCE_SHA:-$(git rev-parse HEAD)}"
ref="${RELEASE_REF:-canary}"
display_version="v${internal_version}"
if [[ "$ref" != "v${internal_version}" ]]; then display_version+="-${source_sha:0:12}"; fi
python3 scripts/node/prepare-xray.py "$arch"
# Use the official native build dependencies and preserve upstream packaging and guest agent layout.
CGO_ENABLED=1 "$go_bin" build -trimpath -tags libsqlite3 -o "$dest/incusd" ./cmd/incusd
CGO_ENABLED=1 "$go_bin" build -trimpath -tags libsqlite3 -o "$dest/incus" ./cmd/incus
CGO_ENABLED=1 "$go_bin" build -trimpath -tags libsqlite3 -o "$dest/incus-user" ./cmd/incus-user
CGO_ENABLED=0 "$go_bin" build -trimpath -o "$dest/incus-agent" ./cmd/incus-agent
CGO_ENABLED=0 "$go_bin" build -trimpath -ldflags "-X github.com/lxc/incus/v7/internal/server/agent/connector.Version=$display_version" -o "$dest/incus-node-agent" ./cmd/incus-node-agent
CGO_ENABLED=0 "$go_bin" build -trimpath -o "$dest/network-worker" ./cmd/network-worker
# Incus: isolate Bubble Tea dependencies in a separate module.
(cd tools/incus-tui && CGO_ENABLED=0 "$go_bin" build -trimpath -ldflags "-X main.version=$display_version" -o "$dest/incus-tui" .)
(cd tools/incus-tui && "$go_bin" list -m -f '{{if .Dir}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' all) > "$dest/tui-modules.txt"
python3 scripts/node/tui-licenses.py "$dest/tui-modules.txt" "$dest/NOTICE.tui"
rm "$dest/tui-modules.txt"
cp internal/server/agent/LICENSE.payincus "$dest/LICENSE.payincus"
cp internal/server/agent/NOTICE "$dest/NOTICE.payincus"
cp internal/server/agent/egress/assets/LICENSE "$dest/LICENSE.xray"
cp internal/server/agent/egress/assets/README.txt "$dest/NOTICE.xray"
printf 'Binaries staged in %s; assemble with the native Incus package layout documented in doc/packaging.md.\n' "$dest"
