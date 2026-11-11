package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const nodeUpgradePath = "/usr/libexec/incus/node-upgrade.sh"

const nodeInstallerPath = "/usr/libexec/incus/node-install.sh"

func updateChannelName(channel string) string {
	switch channel {
	case "canary":
		return "开发版"
	case "latest":
		return "稳定版"
	default:
		return ""
	}
}

type updateServiceState struct {
	active  bool
	enabled string
}

func (b *backend) updateServiceState(ctx context.Context, unit string) (updateServiceState, error) {
	data, err := b.run(ctx, "systemctl", "show", unit, "--property=ActiveState,UnitFileState", "--no-pager")
	if err != nil {
		return updateServiceState{}, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[key] = value
		}
	}
	if (values["ActiveState"] != "active" && values["ActiveState"] != "inactive" && values["ActiveState"] != "failed") || values["UnitFileState"] == "" {
		return updateServiceState{}, fmt.Errorf("%s 状态不完整或正在切换，拒绝更新：%s", unit, strings.TrimSpace(string(data)))
	}
	return updateServiceState{active: values["ActiveState"] == "active", enabled: values["UnitFileState"]}, nil
}

func (b *backend) lockNodeUpdate() (*os.File, error) {
	lockPath := b.updateLockPath
	if lockPath == "" {
		lockPath = "/run/lock/incus-tui-update.lock"
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开节点更新锁失败：%w", err)
	}
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		closeErr := lock.Close()
		return nil, fmt.Errorf("无法独占更新锁 %s；请检查其它更新任务：%w", lockPath, errors.Join(err, closeErr))
	}
	return lock, nil
}

// Incus: package selection and SHA256 checks share the installer shipped with this package.
func (b *backend) updateNode(ctx context.Context, channel string) (err error) {
	name := updateChannelName(channel)
	if name == "" {
		return fmt.Errorf("不支持的更新渠道 %q", channel)
	}
	lock, err := b.lockNodeUpdate()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	states := map[string]updateServiceState{}
	for _, unit := range []string{"incus", "incus-agent"} {
		state, err := b.updateServiceState(ctx, unit)
		if err != nil {
			return err
		}
		states[unit] = state
	}
	if states["incus-agent"].active && !states["incus"].active {
		return fmt.Errorf("连接器运行但 Incus 未运行，请先处理服务状态")
	}

	err = b.connectorReady(ctx)
	if err != nil {
		return err
	}
	logOperation(ctx, "更新渠道：%s (%s)，从 Rain-kl/incus Release 下载并校验软件包", name, channel)
	_, err = b.run(ctx, "/bin/bash", nodeInstallerPath, channel, "--update")
	if err != nil {
		return fmt.Errorf("节点升级失败：%w", err)
	}
	installed, err := b.run(ctx, "/usr/bin/incus-tui", "--version")
	if err != nil {
		return fmt.Errorf("软件包安装后读取 TUI 版本失败：%w", err)
	}
	logOperation(ctx, "已安装：%s", strings.TrimSpace(string(installed)))
	return b.checkUpdatedServices(ctx, states)
}

func (b *backend) checkUpdatedServices(ctx context.Context, states map[string]updateServiceState) error {
	for unit, before := range states {
		after, err := b.updateServiceState(ctx, unit)
		if err != nil {
			return err
		}
		if before != after {
			return fmt.Errorf("%s 更新后服务状态不一致", unit)
		}
		if after.active && unit == "incus" {
			if err = b.waitUpdatedNode(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *backend) waitUpdatedNode(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		_, err := b.run(ctx, "/usr/bin/incus", "query", "/1.0/agent")
		if err == nil {
			return nil
		}
		logOperation(ctx, "等待节点 API：%v", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待节点 API 就绪超时或取消：%w；最后错误：%w", ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}

func nodeUpdateCommand(b *backend, channel string) tea.Cmd {
	// Do not kill apt/dpkg midway through a package transaction on an Escape/Ctrl+C.
	ctx := context.WithValue(context.Background(), operationLogKey{}, b.logs)
	return func() tea.Msg {
		logOperation(ctx, "节点更新开始")
		var err error
		if channel == "installed" {
			err = b.applyInstalledUpdate(ctx)
		} else {
			err = b.updateNode(ctx, channel)
		}
		if err != nil {
			logOperation(ctx, "节点更新失败：%v", err)
		}
		return resultMsg{err: err, restartTUI: err == nil}
	}
}
