#!/bin/bash
# PayIncus: real systemd recovery in a disposable container, with no host namespace or cgroup mounts.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
fixture_name="payincus-egress-systemd-$$"
fixture_image="${PAYINCUS_EGRESS_SYSTEMD_IMAGE:-${fixture_name}:test}"
go_bin="${GO:-go}"
cleanup() {
  docker rm -f "$fixture_name" >/dev/null 2>&1 || true
  if [[ -z "${PAYINCUS_EGRESS_SYSTEMD_IMAGE:-}" ]]; then
    docker image rm "$fixture_image" >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT
cd "$root_dir"
CGO_ENABLED=0 "$go_bin" test -c -o "$scratch/egress.test" ./internal/server/payincus/egress
CGO_ENABLED=0 "$go_bin" build -o "$scratch/payincus-network-worker" ./cmd/payincus-network-worker
if [[ -z "${PAYINCUS_EGRESS_SYSTEMD_IMAGE:-}" ]]; then
  build_args=()
  if [[ -n "${CODEX_PROXY_CERT:-}" ]]; then
    build_args+=(--secret "id=proxy_ca,src=$CODEX_PROXY_CERT")
  fi
  docker build "${build_args[@]}" -t "$fixture_image" -f scripts/payincus/fixtures/egress-systemd.Dockerfile scripts/payincus/fixtures
fi
docker run -d -t --name "$fixture_name" \
  --cap-drop ALL --cap-add SYS_ADMIN --cap-add NET_ADMIN --cap-add NET_RAW \
  --cap-add NET_BIND_SERVICE --cap-add SETUID --cap-add SETGID --cap-add CHOWN \
  --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETPCAP --cap-add SYS_CHROOT \
  --cap-add KILL --cap-add AUDIT_WRITE --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --cgroupns=private --network=none -e container=docker -e SYSTEMD_LOG_TARGET=console \
  --mount "type=bind,src=$scratch,dst=/fixture,readonly" --tmpfs /run --tmpfs /tmp "$fixture_image" \
  sh -ec 'mount -o remount,rw /sys/fs/cgroup; exec /sbin/init' >/dev/null
for attempt in {1..60}; do
  if docker exec "$fixture_name" systemctl is-active --quiet dbus.service; then break; fi
  if [[ "$attempt" == 60 ]]; then docker logs --tail 40 "$fixture_name"; exit 1; fi
  sleep 0.2
done
docker exec "$fixture_name" touch /run/incudal-systemd-test-authorized
run_fixture() {
  docker exec "$fixture_name" systemd-run --unit="payincus-egress-fixture-$1" \
    --collect --wait --pipe -p User=root -p NoNewPrivileges=yes -p PrivateTmp=yes \
    -p ProtectHome=yes --setenv=PATH=/fixture:/usr/sbin:/usr/bin:/sbin:/bin \
    --setenv="INCUDAL_SYSTEMD_NAMESPACE_TEST=$1" \
    /fixture/egress.test -test.run '^TestSystemdNamespaceStartup$' -test.v -test.timeout=60s
}
run_fixture occupied
run_fixture legacy
run_fixture shadow
run_fixture reuse
for mode in fresh stale; do
  docker exec "$fixture_name" sh -ec 'systemctl stop incudal-xray.service; ip netns delete incudal-egress; nft delete table inet incudal_egress'
  run_fixture "$mode"
  run_fixture reuse
done
echo 'Real systemd, native Xray API, queued restart and namespace recovery passed.'
