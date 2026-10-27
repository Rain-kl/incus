package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// PayIncus: complete enrollment through one token and preserve existing Incus trust entries.
func (b *backend) enroll(ctx context.Context, panel, token string) error {
	logOperation(ctx, "[1/5] 检查连接器服务和本地监听")
	err := b.connectorReady(ctx)
	if err != nil {
		return err
	}
	err = b.checkListener(ctx)
	if err != nil {
		return err
	}

	logOperation(ctx, "[2/5] 兑换一次性 token 与 Panel 公钥证书")
	config, err := b.exchange(ctx, panel, token)
	if err != nil {
		return fmt.Errorf("兑换接入信息失败；已发出的 token 请求不自动重试：%w", err)
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"[3/5] 信任 Panel 客户端公钥证书", func() error { return b.ensureTrust(ctx, config.Certificate, config.Fingerprint) }},
		{"[4/5] 配置本地监听 127.0.0.1:8443", func() error { return b.listener(ctx) }},
		{"[5/5] 保存连接凭据并启动连接器", func() error {
			logOperation(ctx, "保存 Panel 连接配置：%s (0600)", b.path)
			err := b.saveConnection(config.connection)
			if err != nil {
				return err
			}
			_, err = b.run(ctx, "systemctl", "enable", "incudal-agent")
			if err != nil {
				return err
			}
			return b.service(ctx, "incudal-agent", "restart")
		}},
	}
	for _, step := range steps {
		logOperation(ctx, "%s", step.name)
		err = step.run()
		if err != nil {
			return fmt.Errorf("%s 失败；token 已消费，已完成步骤保留，可重新生成 token 重试：%w", step.name, err)
		}
	}
	logOperation(ctx, "面板接入配置完成，连接器已启动；可在面板验证节点连接")
	return nil
}

func (b *backend) ensureTrust(ctx context.Context, certificate []byte, fingerprint string) error {
	data, err := b.run(ctx, "/usr/bin/incus", "query", "/1.0/certificates?recursion=1")
	if err != nil {
		return err
	}
	var entries []struct {
		Fingerprint string `json:"fingerprint"`
		Restricted  bool   `json:"restricted"`
	}
	err = json.Unmarshal(data, &entries)
	if err != nil {
		return fmt.Errorf("解析 Incus 信任列表：%w", err)
	}
	if entries == nil {
		return fmt.Errorf("Incus 信任列表缺少数组响应")
	}
	for _, entry := range entries {
		if entry.Fingerprint == fingerprint {
			if entry.Restricted {
				return fmt.Errorf("Panel 证书已存在但权限受限，请在高级维护中检查信任配置")
			}
			logOperation(ctx, "Panel 证书已受信任，无需重复导入")
			return nil
		}
	}
	return b.trust(ctx, certificate, fingerprint)
}
