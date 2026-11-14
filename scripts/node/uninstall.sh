#!/usr/bin/env bash
# Uninstall the Incus node connector or the full node package safely.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: node-uninstall.sh (--agent-only | --full) [--purge-data] [--dry-run] [--yes]

Modes:
  --agent-only   Disable incus-agent.service and remove /etc/incus/agent/config.yaml;
                 keep incus-lts, incus.service, and all instances/data.
  --full         Stop/disable all node services, purge incus-lts, and remove agent/cache files.

Options:
  --purge-data   With --full, also remove /var/lib/incus, /var/lib/incus-tui,
                 /var/log/incus-tui, and /etc/incus (destroys local instance/TUI state).
  --dry-run      Print planned actions without modifying services, packages, or files.
  --yes, -y      Skip interactive confirmation prompt.
  -h, --help     Show this help message.
USAGE
}

mode=""
purge_data=0
dry_run=0
assume_yes=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --agent-only)
      [[ -z "$mode" || "$mode" == agent-only ]] || { echo 'Cannot combine --agent-only and --full.' >&2; exit 2; }
      mode=agent-only
      ;;
    --full)
      [[ -z "$mode" || "$mode" == full ]] || { echo 'Cannot combine --agent-only and --full.' >&2; exit 2; }
      mode=full
      ;;
    --purge-data) purge_data=1 ;;
    --dry-run) dry_run=1 ;;
    --yes|-y) assume_yes=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [[ -z "$mode" ]]; then
  usage >&2
  exit 2
fi
if [[ "$purge_data" == 1 && "$mode" != full ]]; then
  echo '--purge-data requires --full.' >&2
  exit 2
fi

[[ $EUID -eq 0 ]] || { echo 'Run as root.' >&2; exit 1; }

export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l

confirm_uninstall() {
  if [[ "$dry_run" == 1 || "$assume_yes" == 1 ]]; then
    return 0
  fi
  if [[ ! -t 0 ]]; then
    echo 'Non-interactive shell detected; pass --yes to confirm or --dry-run to preview.' >&2
    exit 1
  fi
  if [[ "$mode" == agent-only ]]; then
    echo 'This will disable incus-agent.service and remove /etc/incus/agent/config.yaml.'
  elif [[ "$purge_data" == 1 ]]; then
    echo 'WARNING: This will stop all node services, purge incus-lts, and DELETE /var/lib/incus and /etc/incus.'
  else
    echo 'This will stop all node services, purge incus-lts, and remove /etc/incus/agent and install cache.'
  fi
  read -r -p 'Continue? [y/N]: ' reply
  case "$reply" in
    y|Y|yes|YES) return 0 ;;
    *) echo 'Uninstall cancelled.'; exit 0 ;;
  esac
}

stop_disable_unit() {
  local unit="$1"
  command -v systemctl >/dev/null || return 0
  local load_state
  load_state="$(systemctl show "$unit" --property=LoadState --value 2>/dev/null || true)"
  if [[ -z "$load_state" || "$load_state" == not-found ]]; then
    return 0
  fi
  if [[ "$dry_run" == 1 ]]; then
    echo "[dry-run] systemctl disable --now $unit"
    return 0
  fi
  systemctl disable --now "$unit" || true
}

discover_egress_units() {
  command -v systemctl >/dev/null || return 0
  {
    systemctl list-units --all --type=service --no-legend --plain 'incus-egress-*.service' 2>/dev/null | awk '{print $1}'
    systemctl list-unit-files --type=service --no-legend --plain 'incus-egress-*.service' 2>/dev/null | awk '{print $1}'
  } | grep -E '^incus-egress-[a-zA-Z0-9_.@:-]+\.service$' | sort -u || true
}

remove_path() {
  local target="$1"
  if [[ ! -e "$target" && ! -L "$target" ]]; then
    return 0
  fi
  if [[ "$dry_run" == 1 ]]; then
    echo "[dry-run] rm -rf $target"
    return 0
  fi
  rm -rf "$target"
}

confirm_uninstall

if [[ "$mode" == agent-only ]]; then
  stop_disable_unit incus-agent.service
  remove_path /etc/incus/agent/config.yaml
  if [[ "$dry_run" == 1 ]]; then
    echo '[dry-run] Agent connector uninstall preview complete.'
  else
    echo 'Agent connector disabled and configuration removed.'
    echo 'Note: remove the panel trust certificate via "incus config trust remove <fingerprint>" if needed.'
  fi
  exit 0
fi

# Full node uninstall: stop connector/dependants, egress workers, then daemon.
mapfile -t egress_units < <(discover_egress_units)
for unit in incus-agent.service incus-package-guard.service incus-xray.service incus-network.service incus-user.service; do
  stop_disable_unit "$unit"
done
for unit in "${egress_units[@]}"; do
  [[ -n "$unit" ]] || continue
  stop_disable_unit "$unit"
done
stop_disable_unit incus.service

pkg_status="$(dpkg-query -W -f='${Status}' incus-lts 2>/dev/null || true)"
if [[ "$pkg_status" == *"installed"* ]]; then
  if [[ "$dry_run" == 1 ]]; then
    echo '[dry-run] apt-get purge -y incus-lts'
    echo '[dry-run] apt-get autoremove -y'
  else
    apt-get purge -y incus-lts
    apt-get autoremove -y
  fi
else
  echo 'Package incus-lts is not installed; skipping apt purge.'
fi

if [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null; then
  if [[ "$dry_run" == 1 ]]; then
    echo '[dry-run] systemctl daemon-reload'
  else
    systemctl daemon-reload
  fi
fi

remove_path /etc/incus/agent
remove_path /var/cache/incus/node-install
remove_path /run/incus-upgrade

if [[ "$purge_data" == 1 ]]; then
  remove_path /var/lib/incus
  remove_path /var/lib/incus-tui
  remove_path /var/log/incus-tui
  remove_path /etc/incus
fi

if [[ "$dry_run" == 1 ]]; then
  echo '[dry-run] Full node uninstall preview complete.'
else
  echo 'Node uninstall complete.'
fi
