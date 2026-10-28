#!/usr/bin/env bash
# PayIncus: download a verified release package; configuration belongs to incus-tui.
set -euo pipefail
channel="${1:-latest}"
case "$channel" in latest|canary) ;; -h|--help) echo 'Usage: install.sh [latest|canary] [--no-tui|--update]'; exit 0 ;; *) echo 'Expected latest or canary' >&2; exit 1 ;; esac
[[ $# -le 2 && ( $# -lt 2 || "$2" == --no-tui || "$2" == --update ) ]] || { echo 'Usage: install.sh [latest|canary] [--no-tui|--update]' >&2; exit 1; }
if [[ "${2:-}" == --update ]]; then
  # PayIncus: update runs unattended; service restarts are controlled by the TUI.
  export DEBIAN_FRONTEND=noninteractive
  export NEEDRESTART_MODE=l
fi
[[ $EUID -eq 0 ]] || { echo 'Run this installer as root.' >&2; exit 1; }
# Only local, OS-managed metadata is sourced; remote content is never executed.
# shellcheck source=/dev/null
. /etc/os-release
case " ${ID:-} ${ID_LIKE:-} " in *debian*|*ubuntu*) ;; *) echo 'Only Debian-family systems are supported.' >&2; exit 1 ;; esac
case "${ID:-}:${VERSION_ID:-}" in
  ubuntu:24.04|debian:13) target="${ID}-${VERSION_ID}" ;;
  *) echo "No validated package target for ${ID:-unknown} ${VERSION_ID:-unknown}; supported: Ubuntu 24.04, Debian 13." >&2; exit 1 ;;
esac
arch="$(dpkg --print-architecture)"
case "$arch" in amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
if ! command -v curl >/dev/null || ! command -v python3 >/dev/null; then
  apt-get update
  apt-get install -y --no-install-recommends ca-certificates curl python3
fi
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
# apt's unprivileged downloader must be able to read the verified local package.
chmod 0755 "$stage"
api='https://api.github.com/repos/Rain-kl/incus/releases'
if [[ "$channel" == latest ]]; then api="$api/latest"; else api="$api?per_page=100"; fi
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --connect-timeout 15 --max-time 120 "$api" -o "$stage/releases.json" || {
    echo "Unable to find $channel release. latest requires a stable PayIncus release; use canary explicitly for integration builds." >&2; exit 1;
  }
python3 - "$stage" "$channel" "$target" "$arch" <<'PY'
import json, re, sys
from pathlib import Path
folder, channel, target, arch = sys.argv[1:]
folder = Path(folder)
data = json.loads((folder / 'releases.json').read_text())
releases = data if isinstance(data, list) else [data]
releases = [r for r in releases if not r.get('draft') and (
    (channel == 'latest' and not r.get('prerelease') and re.fullmatch(r'payincus-v\d+\.\d+\.\d+', r.get('tag_name', '')))
    or (channel == 'canary' and r.get('prerelease') and re.fullmatch(r'integration-\d+\.\d+\.\d+-[0-9a-f]{12}', r.get('tag_name', ''))))]
if not releases:
    sys.exit(f'No {channel} PayIncus release available; no other channel will be installed.')
release = max(releases, key=lambda r: r.get('published_at', ''))
assets = release.get('assets', [])
suffix = f'-linux-{target}-{arch}.deb'
packages = [a for a in assets if a['name'].startswith('payincus-incus-') and a['name'].endswith(suffix)]
if len(packages) != 1:
    sys.exit(f"Release {release['tag_name']} has no unique package for {target}/{arch}; no incompatible package will be substituted.")
checks = [a for a in assets if a['name'] == f'SHA256SUMS-{target}-{arch}']
if len(checks) != 1:
    sys.exit('Release is missing platform checksums.')
selected = [packages[0], checks[0]]
for asset in selected:
    name = asset['name']
    url = asset['browser_download_url']
    if not re.fullmatch(r'[A-Za-z0-9._-]+', name) or not url.startswith('https://github.com/Rain-kl/incus/releases/download/'):
        sys.exit('Invalid release asset metadata.')
(folder / 'assets.txt').write_text('\n'.join(a['browser_download_url'] for a in selected) + '\n')
(folder / 'package.txt').write_text(packages[0]['name'])
size = packages[0].get('size')
size_label = f" ({size / 1048576:.1f} MiB)" if isinstance(size, int) and size > 0 else ""
print(f"Installing {release['tag_name']} for {target}/{arch}")
print(f"Downloading {packages[0]['name']}{size_label}")
PY
mapfile -t urls < "$stage/assets.txt"
package="$(cat "$stage/package.txt")"
# PayIncus: keep the progress bar within an 80-column TUI log row after the RFC3339 prefix.
COLUMNS=48 curl --fail --progress-bar --show-error --location --proto '=https' --proto-redir '=https' \
  --connect-timeout 15 --max-time 900 "${urls[0]}" -o "$stage/$package"
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  --connect-timeout 15 --max-time 120 "${urls[1]}" -o "$stage/SHA256SUMS"
python3 - "$stage" "$package" <<'PY'
import hashlib, re, sys
from pathlib import Path
folder, name = Path(sys.argv[1]), sys.argv[2]
matches = []
for line in (folder / 'SHA256SUMS').read_text().splitlines():
    match = re.fullmatch(r'([0-9a-f]{64}) [ *](.+)', line)
    if match and match[2] == name:
        matches.append(match[1])
if len(matches) != 1 or hashlib.file_digest((folder / name).open('rb'), 'sha256').hexdigest() != matches[0]:
    sys.exit('Package checksum verification failed; installation aborted.')
print(f"Verified SHA256 checksum for {name}")
PY
apt-get update
if [[ "${2:-}" == --update ]]; then
  # Channel selection authorizes switching versions; SHA suffixes are not chronological.
  apt-get install -y --reinstall --allow-downgrades --no-remove "$stage/$package"
  echo "Node package installed: $package"
  exit 0
fi
apt-get install -y "$stage/$package"
if command -v systemctl >/dev/null && systemctl is-active --quiet incus; then
  echo '软件包已安装；正在运行的 Incus 尚未由本安装器重启，新二进制和 ID 映射可能未生效。'
  echo '请在维护窗口进入 TUI：更新 → 应用已安装更新（无需下载）。Incus 重启可能停止实例。'
fi
if [[ "${2:-}" == --no-tui ]]; then exit 0; fi
if ! ( : </dev/tty ) 2>/dev/null; then
  echo 'Installation complete. Run sudo incus-tui in an interactive terminal.'
  exit 0
fi
# Reopen the terminal so curl | bash does not consume the TUI input stream.
rm -rf "$stage"
trap - EXIT
exec /usr/bin/incus-tui </dev/tty >/dev/tty 2>&1
