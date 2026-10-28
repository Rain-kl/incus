#!/bin/bash
# PayIncus: exercise real TCP/UDP/DNS isolation inside QEMU without host module or disk access.
set -euo pipefail
if [[ $# != 1 || ! -f "$1" ]]; then
  echo 'Usage: test-egress-vm.sh /path/to/egress.test' >&2
  exit 1
fi
source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
root="$work/root"
mkdir -p "$root"/{bin,sbin,proc,sys,dev,run,tmp,etc,var/lib,usr/sbin,lib/modules}
cp /bin/busybox "$root/bin/busybox"
for app in $(busybox --list); do
  [[ "$app" == busybox ]] || ln -sf busybox "$root/bin/$app"
done
copy_binary() {
  local path
  if ! path="$(command -v "$1")"; then
    printf 'Missing VM build dependency: %s\n' "$1" >&2
    return 1
  fi
  mkdir -p "$root$(dirname "$path")"
  cp -L "$path" "$root$path"
  ldd "$path" | awk '{for(i=1;i<=NF;i++)if($i~/^\//)print $i}' | while read -r library; do
    mkdir -p "$root$(dirname "$library")"
    cp -L "$library" "$root$library"
  done
}
for app in ip nft nsenter mount sysctl kmod; do copy_binary "$app"; done
kernel_path="$(find /boot -name 'vmlinuz-*' -print -quit)"
[[ -n "$kernel_path" ]]
kernel="${kernel_path##*/vmlinuz-}"
cp -a "/lib/modules/$kernel" "$root/lib/modules/"
ln -sf /usr/bin/kmod "$root/sbin/modprobe"
printf 'icmp 1 ICMP\ntcp 6 TCP\nudp 17 UDP\nipv6-icmp 58 IPv6-ICMP\n' > "$root/etc/protocols"
cp "$1" "$root/test"
python3 - "$source_root/internal/server/payincus/egress/assets" "$root/xray" <<'PY'
import gzip, hashlib, json, pathlib, sys
assets = pathlib.Path(sys.argv[1])
manifest = json.loads((assets / 'manifest.json').read_text())
body = gzip.decompress((assets / 'xray-linux-amd64.gz').read_bytes())
assert hashlib.sha256(body).hexdigest() == manifest['files']['amd64']['binarySha256']
output = pathlib.Path(sys.argv[2]); output.write_bytes(body); output.chmod(0o700)
PY
cat > "$root/init" <<'INIT'
#!/bin/busybox sh
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /run /tmp /etc/netns
touch /etc/resolv.conf
mount -t devpts devpts /dev/pts
for module in veth bridge nf_tables nf_tables_bridge nf_conntrack nf_conntrack_bridge br_netfilter nft_counter nft_ct nft_chain_nat nft_nat nft_masq nft_tproxy nft_socket; do modprobe "$module" || true; done
: > /run/incudal-datapath-test-authorized
export INCUDAL_PACKAGE_DATAPATH_TEST=1 PACKAGE_EGRESS_XRAY=/xray
/test -test.run='^TestPackageEgressDatapath$' -test.v -test.timeout=180s
echo PACKAGE_VM_TEST_EXIT=$?
poweroff -f
INIT
chmod +x "$root/init"
(cd "$root"; find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -1 > "$work/initrd.gz")
vm_status=0
timeout 240 qemu-system-x86_64 -accel tcg -m 2048 -smp 2 -nic none -nographic -no-reboot \
  -kernel "$kernel_path" -initrd "$work/initrd.gz" -append 'console=ttyS0 rdinit=/init panic=-1 quiet' > "$work/result.log" 2>&1 || vm_status=$?
cat "$work/result.log"
if [[ "$vm_status" != 0 ]]; then exit "$vm_status"; fi
grep -q 'PACKAGE_VM_TEST_EXIT=0' "$work/result.log"
