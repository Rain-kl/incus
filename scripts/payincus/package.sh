#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
arch="${GOARCH:-$(dpkg --print-architecture)}"
case "$arch" in amd64|arm64) ;; *) echo 'Unsupported package architecture' >&2; exit 1 ;; esac
ref="${RELEASE_REF:-integration}"
source_sha="${SOURCE_SHA:-$(git rev-parse HEAD)}"
# Platform-specific packages must be built on their declared target distribution.
# shellcheck source=/dev/null
. /etc/os-release
target="${ID}-${VERSION_ID}"
case "$target" in ubuntu-24.04|debian-13) ;; *) echo "Unsupported build distribution: $target" >&2; exit 1 ;; esac
channel=integration
if [[ "$ref" == payincus-v* ]]; then
  version="${ref#payincus-v}"
  [[ "$version" =~ ^7\.0\.2(-rc\.[0-9]+)?$ ]] || { echo 'Expected a 7.0.2 stable or RC tag' >&2; exit 1; }
  if [[ "$version" == 7.0.2 ]]; then channel=stable; fi
  deb_version="${version/-rc./~rc.}"
else
  version="7.0.2-integration.${source_sha:0:12}"
  deb_version="7.0.2~integration.${source_sha:0:12}"
fi
binaries="${BUILD_DIR:-$root/scripts/payincus/dist/$arch}"
out="${RELEASE_DIR:-$root/scripts/payincus/dist/release}"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
mkdir -p "$out" "$stage/rootfs/usr/libexec/incus" "$stage/rootfs/usr/bin" \
  "$stage/rootfs/usr/lib/systemd/system" "$stage/rootfs/usr/share/doc/payincus-incus" \
  "$stage/rootfs/etc/incudal-agent" "$stage/debian"
for binary in incusd incus-user incus-agent payincus-agent payincus-network-worker; do
  install -m 0755 "$binaries/$binary" "$stage/rootfs/usr/libexec/incus/$binary"
done
install -m 0755 "$binaries/incus" "$stage/rootfs/usr/bin/incus"
install -m 0755 "$binaries/incus-tui" "$stage/rootfs/usr/bin/incus-tui"
install -m 0755 scripts/payincus/install.sh "$stage/rootfs/usr/libexec/incus/node-install.sh"
install -m 0755 scripts/payincus/prepare-idmap.py "$stage/rootfs/usr/libexec/incus/prepare-idmap"
for unit in incus.service incus-user.service incudal-agent.service; do
  install -m 0644 "deploy/payincus/$unit" "$stage/rootfs/usr/lib/systemd/system/$unit"
done
for notice in LICENSE.payincus NOTICE.payincus LICENSE.xray NOTICE.xray NOTICE.tui; do
  install -m 0644 "$binaries/$notice" "$stage/rootfs/usr/share/doc/payincus-incus/$notice"
done
install -m 0644 COPYING "$stage/rootfs/usr/share/doc/payincus-incus/COPYING"
install -m 0644 FEATURES.md "$stage/rootfs/usr/share/doc/payincus-incus/FEATURES.md"
install -m 0644 scripts/payincus/INSTALL.md "$stage/rootfs/usr/share/doc/payincus-incus/INSTALL.md"
install -m 0644 deploy/payincus/config.example.yaml "$stage/rootfs/etc/incudal-agent/config.example.yaml"
# This package provides its own daemon/user units and intentionally omits optional socket activation.
cat > "$stage/debian/control" <<'CONTROL'
Source: payincus-incus
Section: admin
Priority: optional
Maintainer: PayIncus maintainers <noreply@payincus.com>

Package: payincus-incus
Architecture: any
Description: Incus LTS with PayIncus node extensions and authenticated connector
CONTROL
shlibs="$(cd "$stage" && dpkg-shlibdeps -O \
  -e"$stage/rootfs/usr/libexec/incus/incusd" -e"$stage/rootfs/usr/libexec/incus/incus-user" \
  -e"$stage/rootfs/usr/bin/incus" | sed -n 's/^shlibs:Depends=//p')"
test -n "$shlibs"
mkdir -p "$stage/rootfs/DEBIAN"
cat > "$stage/rootfs/DEBIAN/control" <<CONTROL
Package: payincus-incus
Version: $deb_version
Architecture: $arch
Section: admin
Priority: optional
Maintainer: PayIncus maintainers <noreply@payincus.com>
Depends: $shlibs, liblxc-common, adduser, ca-certificates, iproute2, rsync, attr, tar, squashfs-tools, xz-utils, nftables, iptables, dnsmasq-base, uidmap, passwd, python3, systemd
Recommends: virtiofsd
Provides: incus, incus-base, incus-client
Conflicts: incus, incus-base, incus-client
Replaces: incus, incus-base, incus-client
Description: Incus 7.0 LTS with PayIncus node API and reverse connector
 Optional QEMU and UEFI packages are required for virtual machines.
CONTROL
cat > "$stage/rootfs/DEBIAN/postinst" <<'POSTINST'
#!/bin/sh
set -eu
if [ "$1" = configure ]; then
  getent group incus-admin >/dev/null || addgroup --system incus-admin
  getent group incus >/dev/null || addgroup --system incus
  /usr/libexec/incus/prepare-idmap
  if [ -d /run/systemd/system ]; then systemctl daemon-reload; fi
fi
# Starting or restarting instances and replacing the old connector require a maintenance window.
POSTINST
chmod 0755 "$stage/rootfs/DEBIAN/postinst"
filename="payincus-incus-${version}-linux-${target}-${arch}"
# Archives contain the same file layout and scripts as the deb, plus an explicit manifest.
export version source_sha arch channel target
python3 - "$stage/rootfs/usr/share/doc/payincus-incus/manifest.json" <<'PY'
import json
import os
import platform
import sys
from pathlib import Path
Path(sys.argv[1]).write_text(json.dumps({
    "version": os.environ["version"], "sourceCommit": os.environ["source_sha"],
    "architecture": os.environ["arch"], "upstreamTag": "v7.0.2",
    "upstreamCommit": "01f81e5e607bbb5818410318e4b3496ad1820824",
    "buildDistribution": platform.freedesktop_os_release().get("PRETTY_NAME", "unknown"), "channel": os.environ["channel"], "targetDistribution": os.environ["target"],
    "apiExtensions": ["payincus_node_v1"],
}, indent=2) + "\n")
PY
# Include the manifest in both distribution formats.
dpkg-deb --root-owner-group --build "$stage/rootfs" "$out/$filename.deb"
cp "$stage/rootfs/usr/share/doc/payincus-incus/manifest.json" "$out/$filename.manifest.json"
tar --owner=0 --group=0 --numeric-owner -czf "$out/$filename.tar.gz" -C "$stage/rootfs" .
(cd "$out" && sha256sum "$filename.deb" "$filename.tar.gz" "$filename.manifest.json" > SHA256SUMS)
