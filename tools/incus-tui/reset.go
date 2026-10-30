package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resetPaths are every node-local artifact that keeps control-plane identity or
// firewall state. Image cache lives under /var/lib/incus and is intentionally
// excluded: re-downloading images is slow and carries no stale panel identity.
var resetManagedFiles = []string{
	"/etc/incudal-agent/config.yaml",
	"/etc/incudal-agent/config.yaml.next",
	"/var/lib/incudal-agent/network/desired.json",
	"/var/lib/incudal-agent/network/applied.json",
	"/var/lib/incudal-agent/network/xray.json",
	"/var/lib/incudal-agent/network/groups/registry.json",
	"/var/lib/incudal-agent/network/groups/owner",
	"/var/lib/incudal-agent/network/owner",
	"/var/lib/incus/payincus-security/state.json",
}

var resetManagedDirs = []string{
	"/var/lib/incudal-agent/network/groups",
	"/etc/netns",
}

var resetManagedUnits = []string{
	"incudal-xray.service",
	"incudal-network.service",
	"incudal-package-guard.service",
	"incudal-egress-v2.service",
}

// reset wipes every control-plane artifact so a reinstalled panel cannot adopt
// stale node identity, and clears instance/storage state through the node API.
func (b *backend) reset(ctx context.Context) error {
	// Stop tenant workloads before touching their storage so nothing holds a
	// mount open while pools are removed.
	for _, unit := range resetManagedUnits {
		if err := b.manageService(ctx, unit, "stop"); err != nil {
			logOperation(ctx, "停止 %s 未完成（可忽略）：%v", unit, err)
		}
	}
	for _, unit := range resetManagedUnits {
		_, _ = b.run(ctx, "/usr/bin/systemctl", "disable", unit)
	}

	c := newLocalClient()
	defer c.http.CloseIdleConnections()
	if _, err := c.call(ctx, "POST", "/1.0/payincus/reset", nil); err != nil {
		return fmt.Errorf("通过节点 API 清空实例与存储失败：%w", err)
	}
	logOperation(ctx, "已清空实例、数据卷、存储池、托管网络与多余 profile/项目；系统镜像缓存保留")

	var failures []error
	for _, path := range resetManagedFiles {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			failures = append(failures, fmt.Errorf("删除 %s：%w", path, err))
		}
	}
	for _, dir := range resetManagedDirs {
		if err := os.RemoveAll(dir); err != nil {
			failures = append(failures, fmt.Errorf("删除 %s：%w", dir, err))
		}
	}
	for _, unit := range resetManagedUnits {
		path := filepath.Join("/etc/systemd/system", unit)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, fmt.Errorf("删除 %s：%w", path, err))
		}
	}
	// Drop the drop-ins that pin incus.service to the now-removed egress units.
	for _, dir := range []string{"/etc/systemd/system/incus.service.d", "/etc/systemd/system/incus-containers.service.d"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				failures = append(failures, fmt.Errorf("读取 %s：%w", dir, err))
			}
			continue
		}
		for _, entry := range entries {
			if !strings.Contains(entry.Name(), "incudal") {
				continue
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				failures = append(failures, fmt.Errorf("删除 %s：%w", entry.Name(), err))
			}
		}
	}
	{
		_, err := b.run(ctx, "/usr/bin/systemctl", "daemon-reload")
		if err != nil {
			failures = append(failures, fmt.Errorf("重载 systemd：%w", err))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("清空数据未完全完成，请检查以下项目：%w", errors.Join(failures...))
	}
	logOperation(ctx, "已清理连接器配置、出口策略、防火墙与服务单元；可安全重装控制端并重新接入")
	return nil
}
