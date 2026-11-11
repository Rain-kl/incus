#!/usr/bin/env bash
set -euo pipefail
umask 022
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
case "$target" in ubuntu-24.04|ubuntu-26.04|debian-13) ;; *) echo "Unsupported build distribution: $target" >&2; exit 1 ;; esac
internal_version="$(cat scripts/node/VERSION)"
[[ "$internal_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid internal version' >&2; exit 1; }
channel=canary
if [[ "$ref" == v* ]]; then
  [[ "$ref" == "v$internal_version" ]] || { echo 'Release tag must match VERSION' >&2; exit 1; }
  channel=stable
  version="$internal_version"
  deb_version="$version"
else
  version="${internal_version}-canary.${source_sha:0:12}"
  deb_version="${internal_version}~canary.${source_sha:0:12}"
fi
binaries="${BUILD_DIR:-$root/scripts/node/dist/$arch}"
out="${RELEASE_DIR:-$root/scripts/node/dist/release}"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/rootfs/usr/share/incus" "$out" "$stage/rootfs/usr/libexec/incus" "$stage/rootfs/usr/bin" \
  "$stage/rootfs/usr/lib/systemd/system" "$stage/rootfs/usr/share/doc/incus" \
  "$stage/rootfs/etc/incus/agent" "$stage/debian"
for binary in incusd incus-user incus-agent network-worker; do
  install -m 0755 "$binaries/$binary" "$stage/rootfs/usr/libexec/incus/$binary"
done
install -m 0755 "$binaries/incus" "$stage/rootfs/usr/bin/incus"
install -m 0755 "$binaries/incus-tui" "$stage/rootfs/usr/bin/incus-tui"
mkdir -p "$stage/rootfs/usr/libexec/incus/migrations"
shopt -s nullglob
for migration in scripts/node/migrations/*.sh; do
  install -m 0755 "$migration" "$stage/rootfs/usr/libexec/incus/migrations/$(basename "$migration")"
done
install -m 0755 scripts/node/upgrade.sh "$stage/rootfs/usr/libexec/incus/node-upgrade.sh"
install -m 0755 scripts/node/install.sh "$stage/rootfs/usr/libexec/incus/node-install.sh"
install -m 0755 scripts/node/prepare-idmap.py "$stage/rootfs/usr/libexec/incus/prepare-idmap"
install -D -m 0755 "$binaries/incus-node-agent" "$stage/rootfs/usr/libexec/incus/agent/incus-agent"
for unit in incus.service incus-user.service incus-agent.service; do
  install -m 0644 "deploy/node/$unit" "$stage/rootfs/usr/lib/systemd/system/$unit"
done
cat COPYING "$binaries/LICENSE.payincus" "$binaries/LICENSE.xray" "$binaries/NOTICE.xray" "$binaries/NOTICE.tui" > "$stage/rootfs/usr/share/doc/incus/copyright"
chmod 0644 "$stage/rootfs/usr/share/doc/incus/copyright"
install -m 0644 deploy/node/config.example.yaml "$stage/rootfs/etc/incus/agent/config.example.yaml"
# This package provides its own daemon/user units and intentionally omits optional socket activation.
cat > "$stage/debian/control" <<'CONTROL'
Source: incus
Section: admin
Priority: optional
Maintainer: Incus maintainers <noreply@localhost>

Package: incus
Architecture: any
Description: Incus LTS with Incus node extensions and authenticated connector
CONTROL
shlibs="$(cd "$stage" && dpkg-shlibdeps -O \
  -e"$stage/rootfs/usr/libexec/incus/incusd" -e"$stage/rootfs/usr/libexec/incus/incus-user" \
  -e"$stage/rootfs/usr/bin/incus" | sed -n 's/^shlibs:Depends=//p')"
test -n "$shlibs"
mkdir -p "$stage/rootfs/DEBIAN"
cat > "$stage/rootfs/DEBIAN/control" <<CONTROL
Package: incus
Version: $deb_version
Architecture: $arch
Section: admin
Priority: optional
Maintainer: Incus maintainers <noreply@localhost>
Depends: $shlibs, liblxc-common, adduser, ca-certificates, iproute2, rsync, attr, tar, squashfs-tools, xz-utils, nftables, iptables, dnsmasq-base, uidmap, passwd, python3, systemd, util-linux, procps
Recommends: virtiofsd
Provides: incus-base, incus-client
Conflicts: incus-base, incus-client
Replaces: incus-base, incus-client
Description: Incus 7.0 LTS with Incus node API and reverse connector
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
filename="incus-${version}-linux-${target}-${arch}"
# Archives contain the same file layout and scripts as the deb, plus an explicit manifest.
export version source_sha arch channel target
python3 - "$stage/rootfs/usr/share/incus/manifest.json" <<'PY'
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
    "apiExtensions": ["incus_node_v1"],
}, indent=2) + "\n")
PY
# Include the manifest in both distribution formats.
dpkg-deb --root-owner-group --build "$stage/rootfs" "$out/$filename.deb"
cp "$stage/rootfs/usr/share/incus/manifest.json" "$out/$filename.manifest.json"
tar --owner=0 --group=0 --numeric-owner -czf "$out/$filename.tar.gz" -C "$stage/rootfs" .
(cd "$out" && sha256sum "$filename.deb" "$filename.tar.gz" "$filename.manifest.json" > SHA256SUMS)
