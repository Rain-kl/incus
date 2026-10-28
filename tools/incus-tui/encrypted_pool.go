package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
)

type encryptedPoolRequest struct{ name, device, size string }

// The receipt is transient UI state; it must never be passed to the operation logger.
type encryptedPoolReceipt struct {
	dataset  string
	key      string
	complete bool
}

func (r encryptedPoolReceipt) view() string {
	heading := "加密 ZFS 存储池已创建"
	if !r.complete {
		heading = "加密初始化未确认完成，请保留密钥并检查操作日志"
	}
	guidance := "在控制端导入上述数据集时输入此密钥。"
	if !r.complete {
		guidance = "先检查数据集是否已创建并登记，再导入；不要删除已有数据或丢弃此密钥。"
	}
	return heading + "\n\n数据集（控制端导入时填写）：" + r.dataset +
		"\n加密方式：AES-256-GCM\n\n密钥（请复制保存）：\n" + r.key +
		"\n\n密钥仅在此页展示，不写入日志或密钥文件。\n请保存密钥；终端录屏与剪贴板可能保留副本。\n" +
		guidance + "\n\n保存后按 Enter 返回存储菜单（离开后不再显示密钥）。"
}

func encryptedPoolOperation(ctx context.Context, cancel context.CancelFunc, b *backend, request encryptedPoolRequest) tea.Cmd {
	return func() tea.Msg {
		defer cancel()
		client := newLocalClient()
		defer client.http.CloseIdleConnections()
		receipt, err := b.createEncryptedPool(ctx, client, request)
		if err != nil {
			logOperation(ctx, "加密存储池初始化失败：%v", err)
		} else {
			logOperation(ctx, "加密存储池初始化完成；密钥仅在结果页展示")
		}
		return resultMsg{text: "加密存储池初始化完成", err: err, pool: receipt}
	}
}

func (b *backend) createEncryptedPool(ctx context.Context, client *localClient, request encryptedPoolRequest) (*encryptedPoolReceipt, error) {
	if !poolPattern.MatchString(request.name) || (request.device == "") == (request.size == "") {
		return nil, errors.New("请输入有效池名称，并在专用磁盘与测试文件容量中选择一项")
	}
	var sizeGiB uint64
	if request.size != "" {
		var err error
		sizeGiB, err = strconv.ParseUint(request.size, 10, 64)
		if err != nil || sizeGiB < 1 || sizeGiB > 65536 {
			return nil, errors.New("文件池容量必须为 1–65536 GiB")
		}
	}
	// Refuse unsupported or locked nodes before allocating a disk or creating a pool.
	data, err := client.call(ctx, "GET", "/1.0/payincus", nil)
	if err != nil {
		return nil, err
	}
	var info struct {
		Extensions []string `json:"api_extensions"`
	}
	if json.Unmarshal(data, &info) != nil || !slices.Contains(info.Extensions, "payincus_storage_encryption_v1") {
		return nil, errors.New("节点不支持独立加密存储初始化，请先更新节点集成包；未创建存储池")
	}
	data, err = client.call(ctx, "GET", "/1.0/payincus/security", nil)
	if err != nil {
		return nil, err
	}
	var state struct {
		Locked *bool `json:"locked"`
	}
	if json.Unmarshal(data, &state) != nil || state.Locked == nil || *state.Locked {
		return nil, errors.New("节点已锁定或状态不可用，未创建存储池")
	}
	var material [32]byte
	_, err = rand.Read(material[:])
	if err != nil {
		return nil, errors.New("随机密钥生成失败，未创建存储池")
	}
	key := hex.EncodeToString(material[:])
	clear(material[:])
	contextLog(ctx).secret(key)
	receipt := &encryptedPoolReceipt{dataset: request.name + "/incus", key: key}
	data, err = client.call(ctx, "POST", "/1.0/payincus/storage/pool", map[string]any{"name": request.name, "device": request.device, "sizeGiB": sizeGiB, "key": key})
	if err != nil {
		return receipt, fmt.Errorf("池 %s 的加密初始化未确认完成；请保存结果页密钥并检查存储状态：%w", request.name, err)
	}
	var registered struct {
		Targets []struct {
			Dataset     string `json:"dataset"`
			Fingerprint string `json:"fingerprint"`
		} `json:"targets"`
	}
	if json.Unmarshal(data, &registered) != nil {
		return receipt, errors.New("节点响应无效；请保存密钥并检查数据集注册状态")
	}
	for _, target := range registered.Targets {
		if target.Dataset == receipt.dataset && target.Fingerprint == keyFingerprint(key) {
			receipt.complete = true
			return receipt, nil
		}
	}
	return receipt, errors.New("节点未确认加密数据集身份；请保存密钥并检查注册状态")
}
