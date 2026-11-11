#!/usr/bin/env bash
# PayIncus: native Debian build inside the same target used by the installer.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
bash scripts/node/ci-apt.sh
apt-get update
apt-get install -y --no-install-recommends ca-certificates python3 build-essential pkg-config \
  libcowsql-dev lxc-dev libsqlite3-dev libcap-dev libudev-dev libacl1-dev \
  libseccomp-dev libselinux1-dev dpkg-dev git
# GitHub checkout is owned by the host runner, not the container's root user.
git config --global --add safe.directory /source
export PATH="/opt/go/bin:$PATH"
go version
bash scripts/node/build.sh
bash scripts/node/package.sh

for package in scripts/node/dist/release/*.deb; do python3 scripts/node/test-package.py "$package"; done
