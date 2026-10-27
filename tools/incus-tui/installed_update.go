package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PayIncus: a reachable API can still belong to a replaced daemon with a stale cached idmap.
func (b *backend) installedUpdatePending(ctx context.Context) (bool, error) {
	data, err := b.run(ctx, "systemctl", "show", "incus", "--property=MainPID", "--value")
	if err != nil {
		return false, err
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return false, fmt.Errorf("解析 Incus MainPID：%w", err)
	}
	if pid == 0 {
		return false, nil
	}
	data, err = b.run(ctx, "readlink", fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(data)) == "/usr/libexec/incus/incusd (deleted)", nil
}

func (b *backend) applyInstalledUpdate(ctx context.Context) (err error) {
	lock, err := b.lockNodeUpdate()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	states := map[string]updateServiceState{}
	for _, unit := range []string{"incus", "incudal-agent"} {
		state, stateErr := b.updateServiceState(ctx, unit)
		if stateErr != nil {
			return stateErr
		}
		states[unit] = state
	}
	if states["incudal-agent"].active && !states["incus"].active {
		return errors.New("连接器正在运行但 Incus 未运行，请先处理服务状态后再应用更新")
	}
	err = b.connectorReady(ctx)
	if err != nil {
		return err
	}
	_, err = b.run(ctx, "/usr/libexec/incus/prepare-idmap", "--check")
	if err != nil {
		return fmt.Errorf("已安装软件包的 root ID 映射检查失败，未重启服务：%w", err)
	}
	logOperation(ctx, "应用磁盘上已安装的软件包；不下载、不重新安装，不修改 ID 映射")
	return b.restartUpdatedServices(ctx, states)
}
