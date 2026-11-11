#!/usr/bin/env bash
# Upgrade a verified node package using target-version offline migrations.
set -euo pipefail
if [[ $# != 2 || ( "$1" != --dry-run && "$1" != --apply ) ]]; then
  echo 'Usage: node-upgrade.sh --dry-run|--apply /absolute/path/package.deb|installed' >&2
  exit 2
fi
[[ $EUID -eq 0 ]] || { echo 'Run as root.' >&2; exit 1; }
mode="$1"
package="$2"
# Lock applies to preflight and apply; dry-run creates no persistent lock file.
exec 9< /run/lock
flock -n 9 || { echo 'Another node upgrade is running.' >&2; exit 1; }
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
chmod 0755 "$stage"
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l
if [[ "$package" == installed ]]; then
  target_root=""
  /usr/libexec/incus/prepare-idmap --check
else
  package="$(realpath -- "$package")"
  [[ -f "$package" ]] || { echo 'Package not found.' >&2; exit 1; }
  [[ "$(dpkg-deb --field "$package" Package)" == incus ]] || { echo 'Expected the incus package.' >&2; exit 1; }
  dpkg-deb --extract "$package" "$stage"
  target_root="$stage"
  apt-get --simulate install --reinstall --allow-downgrades --no-remove "$package"
fi
[[ -x "$target_root/usr/libexec/incus/incusd" && -x "$target_root/usr/bin/incus-tui" && -x "$target_root/usr/libexec/incus/agent/incus-agent" ]] || { echo 'Incomplete node package.' >&2; exit 1; }
shopt -s nullglob
migrations=("$target_root"/usr/libexec/incus/migrations/*.sh)
for migration in "${migrations[@]}"; do
  bash "$migration" --dry-run
done
# State migrations are version-specific scripts, never startup-time compatibility branches.
units=(incus incus-agent incus-user)
listed="$(systemctl list-units --all --plain --no-legend 'incus-egress-*.service' incus-xray.service incus-network.service incus-package-guard.service)"
while read -r unit _; do
  [[ "$unit" == incus-egress-*.service || "$unit" == incus-xray.service || "$unit" == incus-network.service || "$unit" == incus-package-guard.service ]] || continue
  units+=("$unit")
done <<< "$listed"
active=()
for unit in "${units[@]}"; do
  state="$(systemctl show "$unit" --property=ActiveState --value)"
  case "$state" in active) active+=("$unit");; inactive|failed) ;; *) echo "Service is transitioning: $unit" >&2; exit 1;; esac
done
daemon_active=0
listener_active=0
for unit in "${active[@]}"; do
  [[ "$unit" != incus ]] || daemon_active=1
  [[ "$unit" != incus-agent && "$unit" != incus-user ]] || listener_active=1
done
if [[ "$listener_active" == 1 && "$daemon_active" == 0 ]]; then
  echo 'A connector or listener is active while Incus is inactive; resolve service states before upgrading.' >&2
  exit 1
fi
echo 'Package, services and migration preflight passed.'
if [[ "$mode" == --dry-run ]]; then exit 0; fi
phase=shutdown
on_failure() {
  status=$?
  if [[ "$status" != 0 ]]; then
    case "$phase" in
      migration|install) echo 'Upgrade failed after shutdown; services remain stopped. Resolve the reported error before starting Incus.' >&2 ;;
      shutdown) echo 'Service shutdown failed; migration has not started. Inspect service states.' >&2 ;;
      restore) echo 'Service restoration failed. Inspect service states and resolve the reported error.' >&2 ;;
    esac
  fi
  rm -rf "$stage"
  exit "$status"
}
trap on_failure EXIT
# Stop listeners first, then the daemon and network workers; do not restart after a migration failure.
systemctl stop incus-agent incus-user incus
for unit in "${units[@]:3}"; do systemctl stop "$unit"; done
if systemctl is-active --quiet incus; then echo 'Incus is still active; migration refused.' >&2; exit 1; fi
if pgrep -x incusd >/dev/null; then echo 'An incusd process is still running; migration refused.' >&2; exit 1; fi
phase=migration
for migration in "${migrations[@]}"; do
  bash "$migration" --dry-run
  bash "$migration" --apply
done
phase=install
if [[ "$package" != installed ]]; then
  apt-get install -y --reinstall --allow-downgrades --no-remove "$package"
fi
systemctl daemon-reload
phase=restore
# Restore workers before the daemon, then the unprivileged listener.
for unit in "${active[@]}"; do
  [[ "$unit" == incus || "$unit" == incus-agent || "$unit" == incus-user ]] && continue
  systemctl start "$unit"
done
for unit in incus incus-user incus-agent; do
  for previous in "${active[@]}"; do
    if [[ "$unit" == "$previous" ]]; then systemctl start "$unit"; fi
 done
done
echo 'Node upgrade applied.'
