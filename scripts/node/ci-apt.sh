#!/usr/bin/env bash
# Configure only disposable CI build environments; this script is not installed on nodes.
set -euo pipefail
. /etc/os-release
if [[ "$ID" != ubuntu ]]; then exit 0; fi
case "$(dpkg --print-architecture)" in
  amd64) archive=ubuntu ;;
  arm64) archive=ubuntu-ports ;;
  *) echo 'Unsupported Ubuntu build architecture.' >&2; exit 1 ;;
esac
case "$VERSION_CODENAME" in noble|resolute) ;; *) echo 'Unsupported Ubuntu build release.' >&2; exit 1 ;; esac
keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg
[[ -r "$keyring" ]] || { echo 'Ubuntu archive signing keyring is missing.' >&2; exit 1; }
# Ubuntu's signed Release metadata and package hashes authenticate the mirror's contents.
cat > /etc/apt/sources.list.d/ubuntu.sources <<SOURCES
Types: deb
URIs: http://mirrors.mit.edu/$archive
Suites: $VERSION_CODENAME $VERSION_CODENAME-updates $VERSION_CODENAME-backports $VERSION_CODENAME-security
Components: main restricted universe multiverse
Signed-By: $keyring
SOURCES
if [[ -f /etc/apt/sources.list ]]; then
  sed -i -E '/^[[:space:]]*deb(-src)?[[:space:]].*(ubuntu|mirror\+file:)/d' /etc/apt/sources.list
fi
cat > /etc/apt/apt.conf.d/80-incus-ci-network <<'CONFIG'
Acquire::Retries "2";
Acquire::http::Timeout "20";
Acquire::https::Timeout "20";
APT::Update::Error-Mode "any";
CONFIG
