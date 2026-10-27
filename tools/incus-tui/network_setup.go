package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const panelBridge = "incusbr0"
const panelGateway = "10.10.0.1/22"

func panelBridgeConfig() map[string]string {
	return map[string]string{"ipv4.address": panelGateway, "ipv4.nat": "true", "ipv4.dhcp": "true", "ipv6.address": "none"}
}

type nodeNetwork struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Managed bool              `json:"managed"`
	Config  map[string]string `json:"config"`
	UsedBy  []string          `json:"used_by"`
}

func (c *localClient) panelNetwork(ctx context.Context) (*nodeNetwork, error) {
	data, err := c.call(ctx, "GET", "/1.0/networks?recursion=1", nil)
	if err != nil {
		return nil, err
	}
	var networks []nodeNetwork
	err = json.Unmarshal(data, &networks)
	if err != nil || networks == nil {
		return nil, fmt.Errorf("解析网络列表失败：%s；%v", safePayload(ctx, data), err)
	}
	for _, network := range networks {
		if network.Name == panelBridge {
			return &network, nil
		}
	}
	return nil, nil
}

func compatiblePanelNetwork(network *nodeNetwork) bool {
	return network != nil && network.Managed && network.Type == "bridge" && network.Config["ipv4.address"] == panelGateway &&
		network.Config["ipv4.nat"] == "true" && network.Config["ipv4.dhcp"] != "false"
}

// PayIncus: credentials alone are insufficient for nodes using the panel's fixed IPv4 pool.
func (c *localClient) ensurePanelNetwork(ctx context.Context) error {
	network, err := c.panelNetwork(ctx)
	if err != nil {
		return err
	}
	if network != nil {
		if !compatiblePanelNetwork(network) {
			return errors.New("incusbr0 已存在，但不是匹配 10.10.0.0/22 的托管 NAT 网络；请使用“修复 → 修复网桥”，不会在接入过程中覆盖现有网络")
		}
		logOperation(ctx, "NAT 网桥已就绪：%s，%s", panelBridge, panelGateway)
		return nil
	}
	_, err = c.call(ctx, "POST", "/1.0/networks", map[string]any{"name": panelBridge, "type": "bridge", "config": panelBridgeConfig()})
	if err != nil {
		return fmt.Errorf("创建 NAT 网桥 %s 失败：%w", panelBridge, err)
	}
	return nil
}

func initialDevices() map[string]map[string]string {
	return map[string]map[string]string{
		"root": {"type": "disk", "path": "/", "pool": "default"},
		"eth0": {"type": "nic", "name": "eth0", "network": panelBridge},
	}
}

func compatibleInitialProfile(config map[string]string, devices map[string]map[string]string) bool {
	if config == nil || devices == nil || len(config) != 0 {
		return false
	}
	expected := initialDevices()
	for name, device := range devices {
		want, ok := expected[name]
		if !ok || len(device) != len(want) {
			return false
		}
		for key, value := range device {
			if want[key] != value {
				return false
			}
		}
	}
	return true
}

func (c *localClient) initializeNode(ctx context.Context) ([]byte, error) {
	// Validate reusable partial resources before making further changes.
	data, err := c.call(ctx, "GET", "/1.0/storage-pools", nil)
	if err != nil {
		return nil, err
	}
	var pools []string
	err = json.Unmarshal(data, &pools)
	if err != nil || pools == nil || len(pools) > 1 || (len(pools) == 1 && pools[0] != "/1.0/storage-pools/default") {
		return nil, errors.New("存在不属于基础初始化的存储池，拒绝覆盖；网络缺失请使用“修复 → 修复网桥”")
	}
	if len(pools) == 1 {
		data, err = c.call(ctx, "GET", pools[0], nil)
		if err != nil {
			return nil, err
		}
		var pool struct {
			Driver string `json:"driver"`
		}
		err = json.Unmarshal(data, &pool)
		if err != nil || pool.Driver != "dir" {
			return nil, errors.New("已有 default 存储池不是 dir，拒绝基础初始化")
		}
	}
	data, err = c.call(ctx, "GET", "/1.0/profiles/default", nil)
	if err != nil {
		return nil, err
	}
	var profile struct {
		Config      map[string]string            `json:"config"`
		Devices     map[string]map[string]string `json:"devices"`
		Description string                       `json:"description"`
	}
	err = json.Unmarshal(data, &profile)
	if err != nil || !compatibleInitialProfile(profile.Config, profile.Devices) {
		return nil, errors.New("默认 profile 已有其它配置，拒绝覆盖；网络修复请使用“修复 → 修复网桥”")
	}
	logOperation(ctx, "初始化 [1/3] 检查并创建固定网段 NAT 网桥")
	err = c.ensurePanelNetwork(ctx)
	if err != nil {
		return nil, err
	}
	logOperation(ctx, "初始化 [2/3] 创建或复用 default(dir) 存储池")
	if len(pools) == 0 {
		_, err = c.call(ctx, "POST", "/1.0/storage-pools", map[string]any{"name": "default", "driver": "dir", "config": map[string]string{}})
		if err != nil {
			return nil, err
		}
	}
	logOperation(ctx, "初始化 [3/3] 补齐 default profile")
	return c.call(ctx, "PUT", "/1.0/profiles/default", map[string]any{"config": profile.Config, "description": profile.Description, "devices": initialDevices()})
}
