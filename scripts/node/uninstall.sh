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

run_or_dry() {
  if [[ "$dry_run" == 1 ]]; then
    echo "[dry-run] $*"
    return 0
  fi
  "$@" || true
}

cleanup_egress_unit_files() {
  local unit_dir="${INCUS_SYSTEMD_UNIT_DIR:-/etc/systemd/system}"
  local netns_dir="${INCUS_NETNS_DIR:-/etc/netns}"
  local unit=""
  local file=""
  for unit in incus-package-guard.service incus-xray.service incus-network.service incus-egress-v2.service incus-user.service "${egress_units[@]}"; do
    [[ -n "$unit" ]] || continue
    remove_path "$unit_dir/$unit"
  done
  if [[ -d "$unit_dir" ]]; then
    for file in "$unit_dir"/incus-egress-*.service; do
      [[ -e "$file" || -L "$file" ]] || continue
      remove_path "$file"
    done
    for dir_name in incus.service.d incus-containers.service.d; do
      [[ -d "$unit_dir/$dir_name" ]] || continue
      for file in "$unit_dir/$dir_name"/incus*.conf; do
        [[ -e "$file" || -L "$file" ]] || continue
        remove_path "$file"
      done
    done
  fi
  if [[ -d "$netns_dir" ]]; then
    remove_path "$netns_dir/incus-egress"
    for file in "$netns_dir"/incus-eg-*; do
      [[ -e "$file" || -L "$file" ]] || continue
      remove_path "$file"
    done
  fi
}

cleanup_egress_kernel_state() {
  local owned_tables=" "
  local family=""
  if command -v ip >/dev/null 2>&1; then
    for family in -4 -6; do
      while IFS= read -r line; do
        [[ -n "$line" ]] || continue
        local prio="" fwmark="" table=""
        prio="$(printf '%s\n' "$line" | awk -F: '/^[0-9]+:/{print $1; exit}')"
        fwmark="$(printf '%s\n' "$line" | sed -nE 's/.*[[:space:]]fwmark[[:space:]]+([^[:space:]]+).*/\1/p')"
        table="$(printf '%s\n' "$line" | sed -nE 's/.*[[:space:]](lookup|table)[[:space:]]+([^[:space:]]+).*/\2/p')"
        [[ -n "$prio" ]] || continue
        local is_mark=0 is_tbl=0 is_prio=0
        if [[ "$fwmark" =~ ^0x0*4[24][0-9a-fA-F]{4}(/|$) ]]; then
          is_mark=1
        fi
        if [[ "$table" == 7420 || ( "$table" =~ ^[0-9]+$ && "$table" -ge 8001 && "$table" -le 8063 ) ]]; then
          is_tbl=1
        fi
        if [[ "$prio" == 1100 || "$prio" == 17420 || ( "$prio" -ge 1001 && "$prio" -le 1063 ) || ( "$prio" -ge 17001 && "$prio" -le 17063 ) ]]; then
          is_prio=1
        fi
        if [[ ( "$is_mark" == 1 && ( "$is_tbl" == 1 || "$is_prio" == 1 ) ) || ( "$is_tbl" == 1 && "$is_prio" == 1 ) ]]; then
          if [[ "$is_tbl" == 1 ]]; then
            owned_tables+="${family}:${table} "
          fi
          run_or_dry ip "$family" rule del priority "$prio"
        fi
      done < <(ip "$family" rule show 2>/dev/null || true)
    done

    for family in -4 -6; do
      local candidate_tables=""
      candidate_tables="$(ip "$family" route show table all 2>/dev/null | sed -nE 's/.*[[:space:]]table[[:space:]]+([0-9]+).*/\1/p' | sort -u || true)"
      for table in $candidate_tables; do
        [[ "$table" == 7420 || ( "$table" -ge 8001 && "$table" -le 8063 ) ]] || continue
        local flush=0
        if [[ "$owned_tables" == *" ${family}:${table} "* ]]; then
          flush=1
        elif ip "$family" route show table "$table" 2>/dev/null | grep -Eq 'unreachable default.*metric 42760|dev (igw-host|ipgh[1-9])|169\.254\.24[04]\.|fd42:696e:6375::'; then
          flush=1
        fi
        if [[ "$flush" == 1 ]]; then
          run_or_dry ip "$family" route flush table "$table"
        fi
      done
    done

    while IFS= read -r link_name; do
      [[ -n "$link_name" ]] || continue
      if [[ "$link_name" == igw-host || "$link_name" =~ ^ipgh([1-9]|[1-5][0-9]|6[0-3])$ ]]; then
        run_or_dry ip link del "$link_name"
      fi
    done < <(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | cut -d@ -f1 || true)

    while IFS= read -r ns_name; do
      [[ -n "$ns_name" ]] || continue
      if [[ "$ns_name" == incus-egress || "$ns_name" == incus-eg-* ]]; then
        run_or_dry ip netns del "$ns_name"
      fi
    done < <(ip netns list 2>/dev/null | awk '{print $1}' || true)
  fi

  if command -v nft >/dev/null 2>&1; then
    while IFS= read -r tbl_line; do
      local tbl_family="" tbl_name=""
      tbl_family="$(printf '%s\n' "$tbl_line" | awk '{print $2}')"
      tbl_name="$(printf '%s\n' "$tbl_line" | awk '{print $3}')"
      [[ -n "$tbl_family" && -n "$tbl_name" ]] || continue
      local drop_tbl=0
      case "${tbl_family}:${tbl_name}" in
        inet:incus_egress|bridge:incus_package_ports|bridge:incus_package_boot|inet:incus_package_egress)
          drop_tbl=1
          ;;
        inet:*_egress|inet:*_package_egress|bridge:*_package_ports|bridge:*_package_boot)
          local body=""
          body="$(nft list table "$tbl_family" "$tbl_name" 2>/dev/null || true)"
          if printf '%s\n' "$body" | grep -Eq '"ipg\*"|"ipgh|"igw-host"|0x00420000|0x420000|0x00440000|169\.254\.24[24]\.|fd42:696e:6375::' || (printf '%s\n' "$body" | grep -q 'set ready' && printf '%s\n' "$body" | grep -q 'type mark'); then
            drop_tbl=1
          fi
          ;;
      esac
      if [[ "$drop_tbl" == 1 ]]; then
        run_or_dry nft delete table "$tbl_family" "$tbl_name"
      fi
    done < <(nft list tables 2>/dev/null || true)
  fi
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

cleanup_egress_unit_files
cleanup_egress_kernel_state

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
