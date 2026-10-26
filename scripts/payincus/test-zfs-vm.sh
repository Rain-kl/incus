#!/bin/bash
# PayIncus: exercise real ZFS inside QEMU; never load modules or use disks on the host.
set -euo pipefail
if [[ $# != 1 || ! -f "$1" ]]; then
  echo 'Usage: test-zfs-vm.sh /path/to/security.test' >&2
  exit 1
fi
module=$(find /lib/modules -path '*/updates/dkms/zfs.ko*' -print -quit)
if [[ -z "$module" ]]; then
  echo 'Install a matching Debian kernel, headers and ZFS DKMS in the disposable build container.' >&2
  exit 1
fi
kernel=${module#/lib/modules/}
kernel=${kernel%%/*}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root="$work/root"
mkdir -p "$root"/{bin,sbin,etc/zfs,lib/modules,tmp,proc,sys,dev,run}
cp /bin/busybox "$root/bin/busybox"
for cmd in sh mount mkdir poweroff; do ln -s /bin/busybox "$root/bin/$cmd"; done
for bin in /usr/sbin/zpool /usr/sbin/zfs /usr/bin/kmod; do
  cp "$bin" "$root/sbin/$(basename "$bin")"
  ldd "$bin" | awk '{for(i=1;i<=NF;i++)if($i~/^\//)print $i}' | while read -r lib; do
    mkdir -p "$root$(dirname "$lib")"
    cp -L "$lib" "$root$lib"
  done
done
ln -s kmod "$root/sbin/modprobe"
ln -s /proc/self/mounts "$root/etc/mtab"
cp -a "/lib/modules/$kernel" "$root/lib/modules/"
cp "$1" "$root/security.test"
cat > "$root/init" <<'INIT'
#!/bin/sh
export PATH=/bin:/sbin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs -o size=1g tmpfs /tmp
if /sbin/modprobe zfs; then
  export RUN_NODE_SECURITY_ZFS_TEST=1 GOMAXPROCS=2
  /security.test -test.v -test.run '^TestZFSNativeLifecycle$'
  echo ZFS_TEST_EXIT=$?
else
  echo ZFS_MODULE_FAILED
fi
poweroff -f
INIT
chmod +x "$root/init"
(cd "$root"; find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -1 > "$work/initramfs.gz")
timeout 180 qemu-system-x86_64 -machine q35 -accel tcg -m 2048 -smp 2 \
  -kernel "/boot/vmlinuz-$kernel" -initrd "$work/initramfs.gz" \
  -append 'console=ttyS0 quiet loglevel=3 panic=-1 payincus_zfs_test_vm=1' \
  -nographic -no-reboot -nic none > "$work/result.log" 2>&1
cat "$work/result.log"
grep -q 'ZFS_TEST_EXIT=0' "$work/result.log"
