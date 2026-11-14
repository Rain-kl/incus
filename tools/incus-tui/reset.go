package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// resetManagedFiles are node-local artifacts holding panel identity or egress
// firewall/routing state. Image cache under /var/lib/incus is intentionally kept.
var resetManagedFiles = []string{
	"/etc/incus/agent/config.yaml",
	"/etc/incus/agent/config.yaml.next",
	"/var/lib/incus/agent/network/desired.json",
	"/var/lib/incus/agent/network/applied.json",
	"/var/lib/incus/agent/network/xray.json",
	"/var/lib/incus/agent/network/boot.nft",
	"/var/lib/incus/agent/network/groups/registry.json",
	"/var/lib/incus/agent/network/groups/owner",
	"/var/lib/incus/agent/network/owner",
	"/var/lib/incus/state/index",
}

var resetManagedDirs = []string{
	"/var/lib/incus/agent/network/groups",
	"/var/lib/incus/state",
	upgradeRunDir,
}

func isOfflineSocketErr(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// reset wipes every control-plane artifact and residual egress kernel state so a
// reinstalled panel cannot adopt stale identity, even when incus.service is down.
func (b *backend) reset(ctx context.Context) error {
	var failures []error

	// Stop dynamic and static egress units plus orphaned helpers before touching files.
	if err := b.removeEgressUnitsAndDropIns(ctx); err != nil {
		failures = append(failures, err)
	}
	cleanupStaleBridgeDnsmasq(ctx, b.runner)
	snap := b.inspectKernelEgress(ctx)
	b.cleanupStaleAgentXray(ctx, snap.StaleXrayPIDs)
	if _, err := b.cleanupKernelEgressState(ctx); err != nil {
		failures = append(failures, fmt.Errorf("清理内核出口残留：%w", err))
	}

	apiResetDone := false
	if b.localSocketExists() {
		c := newLocalClient()
		_, err := c.call(ctx, "POST", "/1.0/agent/reset", nil)
		c.http.CloseIdleConnections()
		if err != nil {
			if isOfflineSocketErr(err) {
				logOperation(ctx, "Incus API 不可达（离线模式继续清理本地与内核残留）：%v", err)
			} else {
				return fmt.Errorf("通过节点 API 清空实例与存储失败：%w", err)
			}
		} else {
			apiResetDone = true
			logOperation(ctx, "已清空实例、数据卷、存储池、托管网络与多余 profile/项目；系统镜像缓存保留")
		}
	} else {
		logOperation(ctx, "未检测到运行中的 Incus socket（离线模式）：跳过实例 API 重置，继续清理本地与内核残留")
	}
	if !apiResetDone {
		if count := b.inspectAgentNetworkState().ContainerCount; count > 0 {
			logOperation(ctx, "注意：Incus 未运行，检测到本地容器目录仍有 %d 个条目未通过 API 清空；完成依赖解锁后可再执行一次清空或检查存储池", count)
		}
	}

	removeTarget := func(hostTarget, display string) {
		err := os.Remove(hostTarget)
		if err != nil && !os.IsNotExist(err) {
			failures = append(failures, fmt.Errorf("删除 %s：%w", display, err))
		}
	}
	for _, path := range resetManagedFiles {
		removeTarget(b.hostPath(path), path)
	}
	if b.path != "" && filepath.Clean(b.path) != "/etc/incus/agent/config.yaml" {
		removeTarget(b.path, b.path)
		removeTarget(b.path+".next", b.path+".next")
	}
	for _, dir := range resetManagedDirs {
		if err := os.RemoveAll(b.hostPath(dir)); err != nil {
			failures = append(failures, fmt.Errorf("删除 %s：%w", dir, err))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("清空数据未完全完成，请检查以下项目：%w", errors.Join(failures...))
	}
	logOperation(ctx, "已清理连接器配置、出口策略、防火墙、内核路由规则与服务单元；可安全重装控制端并重新接入")
	return nil
}
