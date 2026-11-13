#!/usr/bin/env bash
# Upgrade a verified package, retaining boot-local checkpoints for safe retries.
set -euo pipefail
if [[ $# != 2 || ( "$1" != --dry-run && "$1" != --apply ) ]]; then
  echo 'Usage: node-upgrade.sh --dry-run|--apply /absolute/path/package.deb|installed' >&2
  exit 2
fi
[[ $EUID -eq 0 ]] || { echo 'Run as root.' >&2; exit 1; }
mode="$1"
package="$2"
exec 9< /run/lock
flock -n 9 || { echo 'Another node upgrade is running.' >&2; exit 1; }
umask 077
stage="$(mktemp -d)"
journal=/run/incus-upgrade
phase=preflight
trap 'rm -rf "$stage"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
chmod 0755 "$stage"
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l

valid_unit() {
  case "$1" in incus.service|incus-agent.service|incus-user.service|incus-xray.service|incus-network.service|incus-package-guard.service) return 0;; esac
  [[ "$1" =~ ^incus-egress-[a-zA-Z0-9_.@:-]+\.service$ ]]
}

# not-found is an absent optional unit; bus/permission errors remain real errors.
unit_state() {
  local data result key value attempt
  for ((attempt=1; attempt<=15; attempt++)); do
    result=0
    data="$(systemctl show "$1" --property=LoadState,ActiveState --no-pager)" || result=$?
    load_state= state=
    while IFS='=' read -r key value; do
      case "$key" in LoadState) load_state="$value";; ActiveState) state="$value";; esac
    done <<< "$data"
    if [[ "$load_state" == not-found ]]; then return 4; fi
    if [[ "$result" != 0 || "$load_state" != loaded && "$load_state" != masked ]]; then
      echo "Cannot inspect service $1 (load=${load_state:-unknown}, exit=$result)." >&2
      return 1
    fi
    case "$state" in active|inactive|failed) return 0;; activating|deactivating|reloading)
      echo "Waiting for service $1 to settle ($state)." >&2
      sleep 1;;
      *) echo "Unknown service state for $1: ${state:-empty}." >&2; return 1;;
    esac
  done
  echo "Service $1 has not reached a stable state; retry after inspecting systemd." >&2
  return 1
}

service_action() {
  local action="$1" unit="$2" attempt result inspected
  for ((attempt=1; attempt<=3; attempt++)); do
    result=0
    systemctl "$action" "$unit" || result=$?
    inspected=0
    unit_state "$unit" || inspected=$?
    if [[ "$inspected" == 4 ]]; then
      if [[ "$action" == stop || ( "$unit" != incus.service && "$unit" != incus-agent.service ) ]]; then
        echo "Skipping absent service $unit during $action." >&2
        return 0
      fi
      echo "Required service $unit is missing after installation." >&2
      return 1
    fi
    if [[ "$inspected" == 0 ]]; then
      if [[ "$action" == stop && ( "$state" == inactive || "$state" == failed ) ]] || [[ "$action" == start && "$state" == active ]]; then
        if [[ "$result" != 0 ]]; then echo "Service $unit reached the requested state despite systemctl exit $result." >&2; fi
        return 0
      fi
    fi
    echo "Service $action not complete for $unit; retry $attempt/3." >&2
    sleep 1
  done
  echo "Cannot $action $unit; inspect its journal." >&2
  return 1
}

restore_services() {
  local unit failed=0
  # Network workers precede Incus; dependants follow a successfully restored daemon.
  for unit in "${active[@]}"; do
    case "$unit" in incus.service|incus-user.service|incus-agent.service) continue;; esac
    service_action start "$unit" || failed=1
  done
  if [[ "$failed" != 0 ]]; then
    echo 'Network worker restoration incomplete; Incus and dependants remain stopped.' >&2
    return 1
  fi
  for unit in incus.service incus-user.service incus-agent.service; do
    if printf '%s\n' "${active[@]}" | grep -Fxq "$unit"; then
      if [[ "$unit" != incus.service && "$daemon_restored" == 0 ]]; then
        echo "Not starting $unit while Incus restoration is incomplete." >&2
        failed=1
        continue
      fi
      if ! service_action start "$unit"; then
        failed=1
        [[ "$unit" != incus.service ]] || daemon_restored=0
      fi
    fi
  done
  return "$failed"
}

checkpoint() {
  local temporary
  temporary="$(mktemp "$journal/.phase.XXXXXX")"
  printf '%s\n' "$1" > "$temporary"
  mv "$temporary" "$journal/phase"
  phase="$1"
}

on_exit() {
  local result=$?
  trap - EXIT
  if [[ "$result" != 0 ]]; then
    if [[ "$phase" == shutdown ]]; then
      echo 'Shutdown failed before migration/install; restoring the original running services.' >&2
      daemon_restored=1
      if restore_services; then
        rm -rf "$journal"
        echo 'Original services restored; the upgrade can be retried.' >&2
      else
        echo "Service restoration incomplete; original state retained in $journal. Retry the same upgrade." >&2
      fi
    elif [[ "$phase" == migration || "$phase" == install || "$phase" == restore ]]; then
      echo "Upgrade paused at $phase; original running state and completed steps retained in $journal. Retry the same package." >&2
      echo 'Services are not started on potentially migrated or partially installed files.' >&2
    fi
  fi
  rm -rf "$stage"
  exit "$result"
}

if [[ "$package" == installed ]]; then
  target_root=""
  /usr/libexec/incus/prepare-idmap --check
  target_id="installed:$(sha256sum /usr/libexec/incus/incusd | cut -d' ' -f1)"
else
  package="$(realpath -- "$package")"
  [[ -f "$package" ]] || { echo 'Package not found.' >&2; exit 1; }
  [[ "$(dpkg-deb --field "$package" Package)" == incus ]] || { echo 'Expected the incus package.' >&2; exit 1; }
  dpkg-deb --extract "$package" "$stage"
  target_root="$stage"
  target_id="$(sha256sum "$package" | cut -d' ' -f1)"
fi
[[ -x "$target_root/usr/libexec/incus/incusd" && -x "$target_root/usr/bin/incus-tui" && -x "$target_root/usr/libexec/incus/agent/incus-agent" ]] || { echo 'Incomplete node package.' >&2; exit 1; }
active=()
resume=0
if [[ -e "$journal" || -L "$journal" ]]; then
  [[ -d "$journal" && ! -L "$journal" && -O "$journal" ]] || { echo 'Unsafe upgrade checkpoint directory.' >&2; exit 1; }
  for name in target phase active completed; do
    [[ -f "$journal/$name" && ! -L "$journal/$name" && -O "$journal/$name" ]] || { echo 'Incomplete upgrade checkpoint; inspect before retrying.' >&2; exit 1; }
  done
  [[ "$(cat "$journal/target")" == "$target_id" ]] || { echo 'An interrupted upgrade targets another package; resume that verified package first.' >&2; exit 1; }
  phase="$(cat "$journal/phase")"
  case "$phase" in shutdown|migration|install|restore) ;; *) echo 'Invalid upgrade checkpoint phase.' >&2; exit 1;; esac
  mapfile -t active < "$journal/active"
  for unit in "${active[@]}"; do valid_unit "$unit" || { echo 'Invalid service in upgrade checkpoint.' >&2; exit 1; }; done
  resume=1
  echo "Resuming interrupted node upgrade. Phase: $phase."
fi
if [[ "$package" != installed && "$phase" != install && "$phase" != restore ]]; then
  apt-get -o DPkg::Lock::Timeout=60 --simulate install --reinstall --allow-downgrades --no-remove "$package"
fi
shopt -s nullglob
migrations=("$target_root"/usr/libexec/incus/migrations/*.sh)
if [[ "$phase" != install && "$phase" != restore ]]; then
  for migration in "${migrations[@]}"; do
    step="$(sha256sum "$migration" | cut -d' ' -f1) $(basename "$migration")"
    if [[ "$resume" == 1 ]] && grep -Fxq "$step" "$journal/completed"; then continue; fi
    bash "$migration" --dry-run
  done
fi
units=(incus-agent.service incus-user.service incus.service)
listed="$(systemctl list-units --all --plain --no-legend 'incus-egress-*.service' incus-xray.service incus-network.service incus-package-guard.service)"
while read -r unit _; do
  if valid_unit "$unit"; then
    case "$unit" in incus.service|incus-agent.service|incus-user.service) continue;; esac
    units+=("$unit")
  fi
done <<< "$listed"
loaded=()
current_active=()
for unit in "${units[@]}"; do
  inspected=0
  unit_state "$unit" || inspected=$?
  if [[ "$inspected" == 4 ]]; then echo "Skipping absent service $unit."; continue; fi
  [[ "$inspected" == 0 ]] || exit 1
  loaded+=("$unit")
  [[ "$state" != active ]] || current_active+=("$unit")
done
if [[ "$resume" == 0 ]]; then
  active=("${current_active[@]}")
  if printf '%s\n' "${active[@]}" | grep -Eq '^incus-(agent|user)\.service$' && ! printf '%s\n' "${active[@]}" | grep -Fxq incus.service; then
    echo 'A connector or listener is active while Incus is inactive; resolve service states before upgrading.' >&2
    exit 1
  fi
fi
echo 'Package, services and migration preflight passed.'
if [[ "$mode" == --dry-run ]]; then exit 0; fi
if [[ "$package" != installed && "$phase" != install && "$phase" != restore ]]; then
  # Fetch dependencies before stopping any services; APT handles transient network errors.
  apt-get -o DPkg::Lock::Timeout=60 -o Acquire::Retries=3 --download-only install -y --reinstall --allow-downgrades --no-remove "$package"
fi
if [[ "$resume" == 0 ]]; then
  pending="$(mktemp -d "$journal.new.XXXXXX")"
  printf '%s\n' "$target_id" > "$pending/target"
  printf '%s\n' shutdown > "$pending/phase"
  : > "$pending/active"
  if [[ ${#active[@]} != 0 ]]; then printf '%s\n' "${active[@]}" > "$pending/active"; fi
  : > "$pending/completed"
  mv "$pending" "$journal"
  phase=shutdown
fi
trap on_exit EXIT
if [[ "$phase" != restore ]]; then
  # A resumed transaction retains its unsafe phase if stopping services fails again.
  for unit in "${loaded[@]}"; do
    inspected=0
    unit_state "$unit" || inspected=$?
    [[ "$inspected" != 4 ]] || continue
    [[ "$inspected" == 0 ]] || exit 1
    if [[ "$state" == active ]]; then service_action stop "$unit"; fi
  done
  if pgrep -x incusd >/dev/null; then echo 'An incusd process is still running; migration/install refused.' >&2; exit 1; fi
  if [[ "$phase" == shutdown || "$phase" == migration ]]; then
    for migration in "${migrations[@]}"; do
      step="$(sha256sum "$migration" | cut -d' ' -f1) $(basename "$migration")"
      if grep -Fxq "$step" "$journal/completed"; then continue; fi
      bash "$migration" --dry-run
      checkpoint migration
      bash "$migration" --apply
      printf '%s\n' "$step" >> "$journal/completed"
    done
    checkpoint install
  fi
  if [[ "$package" != installed ]]; then
    installed=0
    for attempt in 1 2 3; do
      result=0
      apt-get -o DPkg::Lock::Timeout=60 install -y --reinstall --allow-downgrades --no-remove "$package" || result=$?
      if [[ "$result" == 0 ]]; then installed=1; break; fi
      echo "Package installation failed (exit $result); retry $attempt/3. No package locks will be removed." >&2
      sleep 2
    done
    [[ "$installed" == 1 ]] || exit "$result"
  fi
  systemctl daemon-reload
  checkpoint restore
fi
daemon_restored=1
restore_services
rm -rf "$journal"
phase=done
echo 'Node upgrade applied.'
