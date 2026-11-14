#!/usr/bin/env bash
# Configure only disposable CI build environments; this script is not installed on nodes.
set -euo pipefail
. /etc/os-release
if [[ "$ID" != ubuntu ]]; then exit 0; fi
case "$(dpkg --print-architecture)" in
  amd64)
    mirrors=(
      http://azure.archive.ubuntu.com/ubuntu
      http://archive.ubuntu.com/ubuntu
      http://us.archive.ubuntu.com/ubuntu
      http://mirrors.edge.kernel.org/ubuntu
      http://mirrors.ocf.berkeley.edu/ubuntu
      http://mirrors.mit.edu/ubuntu
    )
    ;;
  arm64)
    mirrors=(
      http://azure.ports.ubuntu.com/ubuntu-ports
      http://ports.ubuntu.com/ubuntu-ports
      http://mirrors.ocf.berkeley.edu/ubuntu-ports
      http://mirrors.mit.edu/ubuntu-ports
    )
    ;;
  *) echo 'Unsupported Ubuntu build architecture.' >&2; exit 1 ;;
esac
case "$VERSION_CODENAME" in noble|resolute) ;; *) echo 'Unsupported Ubuntu build release.' >&2; exit 1 ;; esac
keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg
[[ -r "$keyring" ]] || { echo 'Ubuntu archive signing keyring is missing.' >&2; exit 1; }
if [[ -f /etc/apt/sources.list ]]; then
  sed -i -E '/^[[:space:]]*deb(-src)?[[:space:]].*(ubuntu|mirror\+file:)/d' /etc/apt/sources.list
fi
cat > /etc/apt/apt.conf.d/80-incus-ci-network <<'CONFIG'
Acquire::Retries "3";
Acquire::http::Timeout "20";
Acquire::https::Timeout "20";
APT::Update::Error-Mode "any";
CONFIG

# Ubuntu's signed Release metadata and package hashes authenticate the mirror's contents.
for uri in "${mirrors[@]}"; do
  cat > /etc/apt/sources.list.d/ubuntu.sources <<SOURCES
Types: deb
URIs: $uri
Suites: $VERSION_CODENAME $VERSION_CODENAME-updates $VERSION_CODENAME-backports $VERSION_CODENAME-security
Components: main restricted universe multiverse
Signed-By: $keyring
SOURCES
  rm -rf /var/lib/apt/lists/partial/*
  if apt-get update -qq -o Acquire::Retries=1 -o Acquire::http::Timeout=10 -o Acquire::https::Timeout=10; then
    echo "Configured Ubuntu APT mirror: $uri ($VERSION_CODENAME)"
    exit 0
  fi
  echo "Ubuntu APT mirror probe failed for $uri ($VERSION_CODENAME); trying next fallback..." >&2
done

echo "All configured Ubuntu APT mirrors failed for $VERSION_CODENAME ($(dpkg --print-architecture))." >&2
exit 1
