FROM ubuntu:24.04
COPY ci-apt.sh /tmp/incus-ci-apt.sh
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then \
      printf 'Acquire::https::CaInfo "/run/secrets/proxy_ca";\n' > /etc/apt/apt.conf.d/99-fixture-ca; \
    fi; \
    bash /tmp/incus-ci-apt.sh && apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus iproute2 nftables ca-certificates && \
    rm -f /etc/apt/apt.conf.d/99-fixture-ca && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
