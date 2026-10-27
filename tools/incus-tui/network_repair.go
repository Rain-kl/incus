package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

type repairObject struct {
	Name            string                       `json:"name"`
	Project         string                       `json:"project"`
	Type            string                       `json:"type,omitempty"`
	Status          string                       `json:"status,omitempty"`
	Ephemeral       bool                         `json:"ephemeral,omitempty"`
	UsedBy          []string                     `json:"used_by,omitempty"`
	Config          map[string]string            `json:"config"`
	Devices         map[string]map[string]string `json:"devices"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices,omitempty"`
}

type repairTarget struct {
	Original   repairObject                 `json:"original"`
	NICs       map[string]map[string]string `json:"nics"`
	IPs        map[string]string            `json:"ip_changes"`
	OldGateway string                       `json:"old_gateway,omitempty"`
}

type bridgeRepair struct {
	Phase     string         `json:"phase"`
	Network   *nodeNetwork   `json:"network"`
	Instances []repairTarget `json:"instances"`
	Profiles  []repairTarget `json:"profiles"`
	Reserved  []string       `json:"reserved_addresses,omitempty"`
}

func repairPath(kind string, object repairObject) string {
	project := object.Project
	if project == "" {
		project = "default"
	}
	return "/1.0/" + kind + "/" + url.PathEscape(object.Name) + "?project=" + url.QueryEscape(project)
}

func usesPanelBridge(device map[string]string) bool {
	return device["type"] == "nic" && (device["network"] == panelBridge || (device["nictype"] == "bridged" && device["parent"] == panelBridge))
}

func repairedNIC(device map[string]string) map[string]string {
	result := maps.Clone(device)
	if result == nil {
		result = map[string]string{"type": "nic", "name": "eth0"}
	}
	delete(result, "parent")
	delete(result, "nictype")
	delete(result, "mtu") // Incus inherits the managed network MTU.
	result["network"] = panelBridge
	return result
}

func detachedNIC(device map[string]string, key string) map[string]string {
	name := device["name"]
	if name == "" {
		name = key
	}
	// Keep the guest interface name, avoiding a cloud-init rerun just for detachment.
	result := map[string]string{"type": "nic", "nictype": "p2p", "name": name}
	if device["hwaddr"] != "" {
		result["hwaddr"] = device["hwaddr"]
	}
	return result
}

func usableRepairIP(address string) bool {
	ip, err := netip.ParseAddr(address)
	return err == nil && netip.MustParsePrefix("10.10.0.0/22").Contains(ip) && address != "10.10.0.0" && address != "10.10.0.1" && address != "10.10.3.255"
}

func (c *localClient) repairInventory(ctx context.Context) (*bridgeRepair, error) {
	data, err := c.call(ctx, "GET", "/1.0", nil)
	if err != nil {
		return nil, err
	}
	var server struct {
		Extensions  []string `json:"api_extensions"`
		Environment struct {
			Clustered *bool `json:"server_clustered"`
		} `json:"environment"`
	}
	err = json.Unmarshal(data, &server)
	if err != nil || server.Environment.Clustered == nil || *server.Environment.Clustered {
		return nil, errors.New("网桥自动修复仅支持非集群节点，服务信息必须完整")
	}
	if !slices.Contains(server.Extensions, "profiles_all_projects") || !slices.Contains(server.Extensions, "instance_all_projects") {
		return nil, errors.New("节点缺少跨项目完整清点能力，拒绝删除网络")
	}
	network, err := c.panelNetwork(ctx)
	if err != nil {
		return nil, err
	}
	if network != nil && (network.Type != "bridge" || network.Config["bridge.external_interfaces"] != "" || network.Config["bridge.mode"] == "fan") {
		return nil, errors.New("incusbr0 不是独立 NAT 网桥，或关联了外部网卡/FAN；拒绝自动删除")
	}
	plan := &bridgeRepair{Network: network}
	if network != nil && network.Managed && slices.Contains(server.Extensions, "network_forward") {
		data, err = c.call(ctx, "GET", "/1.0/networks/"+panelBridge+"/forwards", nil)
		if err != nil {
			return nil, err
		}
		var forwards []string
		if json.Unmarshal(data, &forwards) != nil || forwards == nil {
			return nil, errors.New("网络转发清点失败，拒绝删除网桥")
		}
		if len(forwards) != 0 {
			return nil, errors.New("旧网桥存在网络级转发规则，请先迁移规则；修复不会删除这些规则")
		}
	}
	allInstances := map[string]repairObject{}
	for _, kind := range []string{"instances", "profiles"} {
		data, err = c.call(ctx, "GET", "/1.0/"+kind+"?recursion=1&all-projects=true", nil)
		if err != nil {
			return nil, err
		}
		var objects []repairObject
		err = json.Unmarshal(data, &objects)
		if err != nil || objects == nil {
			return nil, fmt.Errorf("%s 跨项目列表格式不兼容，拒绝修复", kind)
		}
		for _, object := range objects {
			if object.Name == "" || object.Config == nil || object.Devices == nil {
				return nil, fmt.Errorf("%s 资源缺少名称、配置或设备，拒绝修复", kind)
			}
			if object.Project == "" {
				object.Project = "default"
			}
			if kind == "instances" {
				allInstances[repairPath(kind, object)] = object
			}
			devices := object.Devices
			if kind == "instances" {
				devices = object.ExpandedDevices
				if devices == nil {
					devices = object.Devices
				}
			}
			target := repairTarget{Original: object, NICs: map[string]map[string]string{}, IPs: map[string]string{}}
			if network != nil {
				target.OldGateway = strings.Split(network.Config["ipv4.address"], "/")[0]
			}
			hasNIC := false
			for name, device := range devices {
				if device["type"] == "nic" || device["type"] == "none" {
					hasNIC = true
				}
				if usesPanelBridge(device) {
					target.NICs[name] = repairedNIC(device)
					if kind == "instances" && target.NICs[name]["hwaddr"] == "" && object.Config["volatile."+name+".hwaddr"] != "" {
						target.NICs[name]["hwaddr"] = object.Config["volatile."+name+".hwaddr"]
					}
				} else if kind == "instances" && usableRepairIP(device["ipv4.address"]) {
					plan.Reserved = append(plan.Reserved, device["ipv4.address"])
				}
			}
			if (kind == "profiles" && object.Project == "default" && object.Name == "default" && devices["eth0"] == nil) ||
				(kind == "instances" && object.Type == "container" && !hasNIC) {
				target.NICs["eth0"] = repairedNIC(nil)
			}
			if len(target.NICs) == 0 {
				continue
			}
			if kind == "instances" {
				if object.Type != "container" || (object.Status != "Running" && object.Status != "Stopped") || (object.Ephemeral && object.Status == "Running") {
					return nil, fmt.Errorf("%s/%s 不是可安全停机的持久 LXC 容器（type=%s status=%s ephemeral=%t）", object.Project, object.Name, object.Type, object.Status, object.Ephemeral)
				}
				plan.Instances = append(plan.Instances, target)
			} else {
				for _, nic := range target.NICs {
					delete(nic, "ipv4.address")
				}
				plan.Profiles = append(plan.Profiles, target)
			}
		}
	}
	for _, profile := range plan.Profiles {
		for _, reference := range profile.Original.UsedBy {
			parsed, parseErr := url.Parse(reference)
			if parseErr != nil || !strings.HasPrefix(parsed.Path, "/1.0/instances/") {
				return nil, fmt.Errorf("profile 存在未知引用 %q，拒绝修复", reference)
			}
			project := parsed.Query().Get("project")
			if project == "" {
				project = "default"
			}
			instance, found := allInstances[parsed.Path+"?project="+url.QueryEscape(project)]
			if !found {
				return nil, fmt.Errorf("profile 引用未清点的实例 %s，拒绝修复", reference)
			}
			for name := range profile.NICs {
				if instance.Devices[name] == nil && instance.Type != "container" {
					return nil, fmt.Errorf("profile 变更会影响非容器实例 %s，拒绝修复", reference)
				}
			}
		}
	}
	if network != nil {
		known := map[string]bool{}
		for _, target := range plan.Instances {
			known[repairPath("instances", target.Original)] = true
		}
		for _, target := range plan.Profiles {
			known[repairPath("profiles", target.Original)] = true
		}
		for _, reference := range network.UsedBy {
			parsed, parseErr := url.Parse(reference)
			if parseErr != nil {
				return nil, fmt.Errorf("网桥引用无法解析 %q：%w", reference, parseErr)
			}
			project := parsed.Query().Get("project")
			if project == "" {
				project = "default"
			}
			if !known[parsed.Path+"?project="+url.QueryEscape(project)] {
				return nil, fmt.Errorf("网桥存在未纳入修复的引用 %q，尚未停止容器", reference)
			}
		}
	}
	slices.SortFunc(plan.Instances, func(a, b repairTarget) int {
		return strings.Compare(repairPath("instances", a.Original), repairPath("instances", b.Original))
	})
	return plan, c.assignRepairAddresses(ctx, plan)
}

func (c *localClient) assignRepairAddresses(ctx context.Context, plan *bridgeRepair) error {
	// Reserve every surviving address first, so a missing NIC cannot steal a later container's IP.
	owners := map[string]string{}
	for _, address := range plan.Reserved {
		owners[address] = "unaffected"
	}
	for index := range plan.Instances {
		target := &plan.Instances[index]
		keys := slices.Sorted(maps.Keys(target.NICs))
		for _, name := range keys {
			address := target.NICs[name]["ipv4.address"]
			if usableRepairIP(address) && owners[address] == "" {
				owners[address] = repairPath("instances", target.Original) + "/" + name
			}
		}
	}
	for index := range plan.Instances {
		target := &plan.Instances[index]
		for _, name := range slices.Sorted(maps.Keys(target.NICs)) {
			device := target.NICs[name]
			old := device["ipv4.address"]
			if old == "none" {
				continue
			}
			owner := repairPath("instances", target.Original) + "/" + name
			if usableRepairIP(old) && owners[old] == owner {
				continue
			}
			// A missing static lease may still have a valid live address worth preserving.
			if old == "" && target.Original.Status == "Running" {
				data, err := c.call(ctx, "GET", strings.Replace(repairPath("instances", target.Original), "?", "/state?", 1), nil)
				if err != nil {
					return err
				}
				var state struct {
					Network map[string]struct {
						Addresses []struct {
							Family  string `json:"family"`
							Address string `json:"address"`
						} `json:"addresses"`
					} `json:"network"`
				}
				if json.Unmarshal(data, &state) != nil || state.Network == nil {
					return errors.New("容器网络状态格式不兼容")
				}
				guestName := device["name"]
				if guestName == "" {
					guestName = name
				}
				for _, ip := range state.Network[guestName].Addresses {
					if ip.Family == "inet" && usableRepairIP(ip.Address) && owners[ip.Address] == "" {
						device["ipv4.address"] = ip.Address
						owners[ip.Address] = owner
						break
					}
				}
			}
			if device["ipv4.address"] == old {
				device["ipv4.address"] = ""
				last := netip.MustParseAddr("10.10.3.254")
				for ip := netip.MustParseAddr("10.10.1.1"); ip.Compare(last) <= 0; ip = ip.Next() {
					candidate := ip.String()
					if owners[candidate] == "" {
						device["ipv4.address"] = candidate
						owners[candidate] = owner
						break
					}
				}
			}
			if device["ipv4.address"] == "" {
				return errors.New("10.10.1.1—10.10.3.254 地址池已满，尚未停止容器或删除网络")
			}
			target.IPs[name] = device["ipv4.address"]
			logOperation(ctx, "地址计划 %s/%s %s：%s -> %s", target.Original.Project, target.Original.Name, name, old, device["ipv4.address"])
		}
	}
	return nil
}

func copyDevices(devices map[string]map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string, len(devices))
	for name, device := range devices {
		result[name] = maps.Clone(device)
	}
	return result
}

func (c *localClient) patchRepairDevices(ctx context.Context, kind string, target repairTarget, detach bool) error {
	path := repairPath(kind, target.Original)
	data, err := c.call(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	var current repairObject
	if json.Unmarshal(data, &current) != nil || current.Config == nil || current.Devices == nil {
		return fmt.Errorf("%s 设备配置格式不兼容", path)
	}
	if kind == "instances" && current.Status != "Stopped" {
		return fmt.Errorf("%s 当前状态为 %s，拒绝在运行中重绑网卡", path, current.Status)
	}
	devices := copyDevices(current.Devices)
	config := maps.Clone(current.Config)
	for name, nic := range target.NICs {
		expected := target.Original.Devices[name]
		if !detach {
			expected = detachedNIC(nic, name)
		}
		if !reflect.DeepEqual(devices[name], expected) {
			return fmt.Errorf("%s 的设备 %s 在计划后发生变化，拒绝覆盖", path, name)
		}
		if detach {
			devices[name] = detachedNIC(nic, name)
		} else {
			devices[name] = maps.Clone(nic)
		}
	}
	if !detach && kind == "instances" {
		for _, key := range []string{"user.network-config", "cloud-init.network-config"} {
			gateway, parseErr := netip.ParseAddr(target.OldGateway)
			if parseErr == nil && gateway.Is4() && target.OldGateway != "10.10.0.1" && config[key] != "" {
				re := regexp.MustCompile(`\b` + regexp.QuoteMeta(target.OldGateway) + `\b`)
				config[key] = re.ReplaceAllString(config[key], "10.10.0.1")
			}
		}
		for name, address := range target.IPs {
			old := target.Original.ExpandedDevices[name]["ipv4.address"]
			if old == "" {
				old = target.Original.Devices[name]["ipv4.address"]
			}
			netipAddr, parseErr := netip.ParseAddr(old)
			if parseErr != nil || !netipAddr.Is4() {
				continue
			}
			for deviceName, original := range target.Original.ExpandedDevices {
				if original["type"] != "proxy" {
					continue
				}
				device := maps.Clone(original)
				for _, protocol := range []string{"tcp:", "udp:"} {
					device["connect"] = strings.Replace(device["connect"], protocol+old+":", protocol+address+":", 1)
				}
				if device["connect"] != original["connect"] {
					if !reflect.DeepEqual(current.Devices[deviceName], target.Original.Devices[deviceName]) {
						return fmt.Errorf("%s 的转发设备 %s 已变化，拒绝覆盖", path, deviceName)
					}
					devices[deviceName] = device
				}
			}
			for _, key := range []string{"user.network-config", "cloud-init.network-config"} {
				value := config[key]
				if value == "" {
					continue
				}
				re := regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `(?:/\d{1,2})?\b`)
				config[key] = re.ReplaceAllStringFunc(value, func(match string) string {
					if strings.Contains(match, "/") {
						return address + "/22"
					}
					return address
				})
			}
		}
	}
	_, err = c.call(ctx, "PATCH", path, map[string]any{"devices": devices, "config": config})
	return err
}

func (b *backend) repairBridge(ctx context.Context) error {
	c := newLocalClient()
	defer c.http.CloseIdleConnections()
	return c.repairBridge(ctx, b.runner, "/var/lib/incus-tui/repairs")
}

// PayIncus: repair only the panel NAT bridge; preserve disks, profiles, valid IPs and stopped state.
func (c *localClient) repairBridge(ctx context.Context, runner Runner, backupRoot string) (err error) {
	err = os.MkdirAll(backupRoot, 0o700)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(backupRoot, "bridge.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("无法取得网桥修复锁；请检查其它修复进程或未完成操作：%w", err)
	}
	defer func() {
		cleanupErr := errors.Join(lock.Close(), os.Remove(lock.Name()))
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("清理网桥修复锁失败：%w", cleanupErr))
		}
	}()
	_, err = fmt.Fprintf(lock, "pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	err = lock.Sync()
	if err != nil {
		return err
	}
	plan, err := c.repairInventory(ctx)
	if err != nil {
		return err
	}
	if plan.Network != nil && !plan.Network.Managed {
		data, checkErr := runner.Run(ctx, "ip", []string{"-j", "-d", "link", "show", "dev", panelBridge}, "")
		if checkErr != nil {
			return checkErr
		}
		var links []struct {
			Info struct {
				Kind string `json:"info_kind"`
			} `json:"linkinfo"`
		}
		if json.Unmarshal(data, &links) != nil || len(links) != 1 || links[0].Info.Kind != "bridge" {
			return errors.New("同名设备不是 Linux bridge，尚未修改任何配置")
		}
		data, checkErr = runner.Run(ctx, "ip", []string{"-j", "link", "show", "master", panelBridge}, "")
		if checkErr != nil {
			return checkErr
		}
		var ports []struct {
			Name string `json:"ifname"`
		}
		if json.Unmarshal(data, &ports) != nil || ports == nil {
			return errors.New("无法清点非托管网桥端口")
		}
		known := map[string]bool{}
		for _, target := range plan.Instances {
			for name := range target.NICs {
				known[target.Original.Config["volatile."+name+".host_name"]] = true
			}
		}
		for _, port := range ports {
			if port.Name == "" || !known[port.Name] {
				return fmt.Errorf("非托管网桥连接了无法确认的端口 %q，尚未停止容器或修改配置", port.Name)
			}
		}
	}
	dir, err := os.MkdirTemp(backupRoot, "bridge-"+time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return err
	}
	backup := filepath.Join(dir, "state.json")
	checkpoint := func(phase string) error {
		plan.Phase = phase
		data, encodeErr := json.MarshalIndent(plan, "", "  ")
		if encodeErr != nil {
			return encodeErr
		}
		file, writeErr := os.OpenFile(backup+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if writeErr != nil {
			return writeErr
		}
		_, writeErr = file.Write(data)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		writeErr = errors.Join(writeErr, file.Close())
		if writeErr != nil {
			return writeErr
		}
		renameErr := os.Rename(backup+".tmp", backup)
		if renameErr != nil {
			return renameErr
		}
		logOperation(ctx, "网桥修复：%s；备份/进度：%s", phase, backup)
		return nil
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("网桥修复停在 %s；原配置及地址计划：%s；尚未完成的容器不会自动启动：%w", plan.Phase, backup, err)
		}
	}()
	err = checkpoint("已备份，开始停止受影响容器")
	if err != nil {
		return err
	}
	for _, target := range plan.Instances {
		if target.Original.Status != "Running" {
			continue
		}
		path := strings.Replace(repairPath("instances", target.Original), "?", "/state?", 1)
		logOperation(ctx, "停止容器 %s/%s", target.Original.Project, target.Original.Name)
		_, err = c.call(ctx, "PUT", path, map[string]any{"action": "stop", "timeout": 60, "force": false, "stateful": false})
		if err != nil {
			return err
		}
	}
	err = checkpoint("解绑容器与 profile 的旧网桥引用")
	if err != nil {
		return err
	}
	for _, kind := range []string{"instances", "profiles"} {
		targets := plan.Instances
		if kind == "profiles" {
			targets = plan.Profiles
		}
		for _, target := range targets {
			err = c.patchRepairDevices(ctx, kind, target, true)
			if err != nil {
				return err
			}
		}
	}
	err = checkpoint("删除旧网桥")
	if err != nil {
		return err
	}
	if plan.Network != nil && plan.Network.Managed {
		var data []byte
		data, err = c.call(ctx, "GET", "/1.0/networks/"+panelBridge, nil)
		if err != nil {
			return err
		}
		var current nodeNetwork
		if json.Unmarshal(data, &current) != nil || !current.Managed || current.Type != "bridge" || len(current.UsedBy) != 0 || current.Config["bridge.external_interfaces"] != "" || !reflect.DeepEqual(current.Config, plan.Network.Config) {
			return errors.New("旧网桥仍被引用或配置已变化，拒绝删除")
		}
		_, err = c.call(ctx, "DELETE", "/1.0/networks/"+panelBridge, nil)
		if err != nil {
			return err
		}
	} else if plan.Network != nil {
		// Only a detached Linux bridge can be removed; an uplink or occupied bridge is refused.
		var data []byte
		data, err = runner.Run(ctx, "ip", []string{"-j", "-d", "link", "show", "dev", panelBridge}, "")
		if err != nil {
			return err
		}
		var links []struct {
			Info struct {
				Kind string `json:"info_kind"`
			} `json:"linkinfo"`
		}
		if json.Unmarshal(data, &links) != nil || len(links) != 1 || links[0].Info.Kind != "bridge" {
			return errors.New("同名设备不是 Linux bridge，拒绝删除")
		}
		data, err = runner.Run(ctx, "ip", []string{"-j", "link", "show", "master", panelBridge}, "")
		if err != nil {
			return err
		}
		var ports []json.RawMessage
		if json.Unmarshal(data, &ports) != nil || ports == nil || len(ports) != 0 {
			return errors.New("非托管网桥仍连接其它网卡，拒绝删除")
		}
		_, err = runner.Run(ctx, "ip", []string{"link", "delete", "dev", panelBridge, "type", "bridge"}, "")
		if err != nil {
			return err
		}
	}
	err = checkpoint("创建新 NAT 网桥")
	if err != nil {
		return err
	}
	config := map[string]string{}
	if plan.Network != nil && plan.Network.Managed {
		maps.Copy(config, plan.Network.Config)
	}
	for key := range config {
		if strings.HasPrefix(key, "volatile.") {
			delete(config, key)
		}
	}
	if config["ipv4.address"] != panelGateway {
		delete(config, "ipv4.dhcp.ranges")
	}
	for key, value := range panelBridgeConfig() {
		if key != "ipv6.address" || config[key] == "" {
			config[key] = value
		}
	}
	_, err = c.call(ctx, "POST", "/1.0/networks", map[string]any{"name": panelBridge, "type": "bridge", "config": config})
	if err != nil {
		return err
	}
	err = checkpoint("恢复 profile 与容器网卡，保留有效 IP")
	if err != nil {
		return err
	}
	for _, kind := range []string{"profiles", "instances"} {
		targets := plan.Profiles
		if kind == "instances" {
			targets = plan.Instances
		}
		for _, target := range targets {
			err = c.patchRepairDevices(ctx, kind, target, false)
			if err != nil {
				return err
			}
		}
	}
	currentNetwork, checkErr := c.panelNetwork(ctx)
	if checkErr != nil {
		return checkErr
	}
	if !compatiblePanelNetwork(currentNetwork) {
		return errors.New("重建后的 NAT 网桥校验失败，容器保持停止")
	}
	err = checkpoint("恢复原本运行的容器")
	if err != nil {
		return err
	}
	for _, target := range plan.Instances {
		if len(target.IPs) > 0 {
			logOperation(ctx, "容器 %s/%s 地址已补齐或变更：%v；请核对面板登记，节点心跳不会覆盖面板的地址分配记录", target.Original.Project, target.Original.Name, target.IPs)
		}
		if target.Original.Status != "Running" {
			continue
		}
		path := strings.Replace(repairPath("instances", target.Original), "?", "/state?", 1)
		_, err = c.call(ctx, "PUT", path, map[string]any{"action": "start", "timeout": 60, "force": false, "stateful": false})
		if err != nil {
			return err
		}
	}
	return checkpoint("修复完成")
}
