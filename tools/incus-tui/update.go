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

// PayIncus: package selection and SHA256 checks share the installer shipped with this package.
func (b *backend) updateNode(ctx context.Context, channel string) (err error) {
	name := updateChannelName(channel)
	if name == "" {
		return fmt.Errorf("不支持的更新渠道 %q", channel)
	}
	lockPath := b.updateLockPath
	if lockPath == "" {
		lockPath = "/run/lock/incus-tui-update.lock"
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("打开节点更新锁失败：%w", err)
	}
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		closeErr := lock.Close()
		return fmt.Errorf("无法独占更新锁 %s；请检查其它更新任务：%w", lockPath, errors.Join(err, closeErr))
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	states := map[string]updateServiceState{}
	for _, unit := range []string{"incus", "incudal-agent"} {
		state, err := b.updateServiceState(ctx, unit)
		if err != nil {
			return err
		}
		states[unit] = state
	}
	if states["incudal-agent"].active && !states["incus"].active {
		return fmt.Errorf("连接器正在运行但 Incus 未运行；重启连接器会连带启动 Incus，请先处理服务状态后再更新")
	}
	err = b.connectorReady(ctx)
	if err != nil {
		return err
	}
	logOperation(ctx, "更新渠道：%s (%s)，从 Rain-kl/incus Release 下载并校验软件包", name, channel)
	_, err = b.run(ctx, "/bin/bash", nodeInstallerPath, channel, "--update")
	if err != nil {
		return fmt.Errorf("节点软件包更新失败，未执行后续服务重启：%w", err)
	}
	installed, err := b.run(ctx, "/usr/bin/incus-tui", "--version")
	if err != nil {
		return fmt.Errorf("软件包安装后读取 TUI 版本失败：%w", err)
	}
	logOperation(ctx, "已安装：%s", strings.TrimSpace(string(installed)))
	for _, unit := range []string{"incus", "incudal-agent"} {
		if !states[unit].active {
			logOperation(ctx, "%s 原本未运行，保持停止状态", unit)
			continue
		}
		logOperation(ctx, "重启原本运行的服务：%s", unit)
		err = b.service(ctx, unit, "restart")
		if err != nil {
			return fmt.Errorf("软件包已安装，但 %s 重启失败：%w", unit, err)
		}
		if unit == "incus" {
			err = b.waitUpdatedNode(ctx)
			if err != nil {
				return fmt.Errorf("软件包已安装，等待新版节点 API 失败：%w", err)
			}
		}
	}
	for _, unit := range []string{"incus", "incudal-agent"} {
		state, checkErr := b.updateServiceState(ctx, unit)
		if checkErr != nil {
			return fmt.Errorf("更新后检查 %s 状态失败：%w", unit, checkErr)
		}
		if state.active != states[unit].active || state.enabled != states[unit].enabled {
			return fmt.Errorf("更新后 %s 服务状态与更新前不一致（运行=%t，启用=%s），请检查 systemd 日志", unit, state.active, state.enabled)
		}
	}
	logOperation(ctx, "节点更新完成，服务启用状态保持不变，正在重新打开新版 TUI")
	return nil
}

func (b *backend) waitUpdatedNode(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		_, err := b.run(ctx, "/usr/bin/incus", "query", "/1.0/payincus")
		if err == nil {
			return nil
		}
		logOperation(ctx, "等待新版节点 API：%v", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w；最后错误：%w", ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}

func nodeUpdateCommand(b *backend, channel string) tea.Cmd {
	// Do not kill apt/dpkg midway through a package transaction on an Escape/Ctrl+C.
	ctx := context.WithValue(context.Background(), operationLogKey{}, b.logs)
	return func() tea.Msg {
		logOperation(ctx, "节点更新开始")
		err := b.updateNode(ctx, channel)
		if err != nil {
			logOperation(ctx, "节点更新失败：%v", err)
		}
		return resultMsg{err: err, restartTUI: err == nil}
	}
}
