package egress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	groupKeyPattern = regexp.MustCompile(`^v[1-9][0-9]{0,9}$`)
	groupNICPattern = regexp.MustCompile(`^ipg[a-f0-9]{11}$`)
)

// EgressGroup describes an immutable package exit and its proxy nodes.
type EgressGroup struct {
	Key    string `json:"key"`
	Egress Policy `json:"egress"`
	Nodes  []Node `json:"nodes"`
}

// EgressNIC identifies the host interface and source address of a package NIC.
type EgressNIC struct {
	HostName string `json:"hostName"`
	IPv4     string `json:"ipv4"`
	Bridge   string `json:"bridge"`
}

// EgressBinding binds an instance generation to an isolated exit group.
type EgressBinding struct {
	InstanceName string      `json:"instanceName"`
	Generation   int         `json:"generation"`
	Mode         string      `json:"mode"`
	GroupKey     string      `json:"groupKey"`
	Nics         []EgressNIC `json:"nics"`
}

// EgressGroupStatus reports the readiness and revision of an exit group.
type EgressGroupStatus struct {
	Key   string `json:"key"`
	State string `json:"state"`
	Error string `json:"error"`
}

// EgressBindingStatus reports the applied generation of an instance binding.
type EgressBindingStatus struct {
	InstanceName string `json:"instanceName"`
	Generation   int    `json:"generation"`
}

func groupCoreConfig(group EgressGroup, g groupRuntime) ([]byte, error) {
	body, err := xrayConfig(Instruction{Egress: group.Egress, Nodes: group.Nodes}, nil, true)
	if err != nil {
		return nil, err
	}

	var config map[string]any
	{
		err = json.Unmarshal(body, &config)
		if err != nil {
			return nil, err
		}
	}

	inbounds, ok := config["inbounds"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Xray inbounds")
	}

	for _, raw := range inbounds {
		in, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Xray inbound")
		}

		if in["tag"] == "dns-forward" {
			in["listen"] = g.ip()
		}
	}

	config["dns"] = map[string]any{"servers": []string{"tcp://1.1.1.1:53"}, "tag": "group-dns-query", "queryStrategy": "UseIPv4"}
	outbounds := []any{}
	original, ok := config["outbounds"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Xray outbounds")
	}

	for _, raw := range original {
		outbound, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Xray outbound")
		}

		if outbound["tag"] != "direct" {
			outbounds = append(outbounds, raw)
		}
	}

	outbounds = append(outbounds, map[string]any{"tag": "group-dns-out", "protocol": "dns", "settings": map[string]any{"network": "tcp", "address": "1.1.1.1", "port": 53, "nonIPQuery": "skip"}, "proxySettings": map[string]any{"tag": nodeTag(group.Nodes[0].ID)}})
	config["outbounds"] = outbounds
	config["routing"] = map[string]any{"domainStrategy": "AsIs", "rules": []any{
		map[string]any{"type": "field", "inboundTag": []string{"api-in"}, "outboundTag": "api"},
		map[string]any{"type": "field", "inboundTag": []string{"dns-forward"}, "outboundTag": "group-dns-out"},
		map[string]any{"type": "field", "inboundTag": []string{"transparent", "probe-0", "group-dns-query"}, "outboundTag": nodeTag(group.Nodes[0].ID)},
	}}
	delete(config, "observatory")
	return json.MarshalIndent(config, "", "  ")
}

func groupUnit(g groupRuntime, binary string) string {
	return fmt.Sprintf(`[Unit]
Description=Incudal package egress %s
After=network-online.target incudal-network.service
Requires=incudal-network.service
[Service]
Type=simple
ExecStart=/usr/sbin/ip netns exec %s %s run -config %s
ExecStopPost=-/usr/sbin/nft delete element inet incudal_package_egress ready { %s }
Restart=always
RestartSec=3
UMask=0077
NoNewPrivileges=true
ProtectHome=true
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
[Install]
WantedBy=multi-user.target
`, g.Key, g.ns(), binary, filepath.Join(g.dir(), "xray.json"), g.mark())
}

func readGroupRegistry() map[string]groupRuntime {
	result := map[string]groupRuntime{}
	body, err := os.ReadFile(filepath.Join(stateDir, "groups", "registry.json"))
	if err == nil {
		_ = json.Unmarshal(body, &result)
	}

	return result
}

func allocateGroups(groups []EgressGroup, all map[string]groupRuntime) (map[string]groupRuntime, error) {
	active := map[string]groupRuntime{}
	used := map[int]bool{}
	for key, g := range all {
		if !groupKeyPattern.MatchString(key) || g.Key != key || g.Slot < 1 || g.Slot > 63 || used[g.Slot] {
			return nil, fmt.Errorf("invalid group resource registry")
		}

		used[g.Slot] = true
	}

	for _, group := range groups {
		{
			g, ok := all[group.Key]
			if ok {
				active[group.Key] = g
				continue
			}
		}

		slot := 1
		for used[slot] && slot <= 63 {
			slot++
		}

		if slot > 63 {
			return nil, fmt.Errorf("group resource capacity")
		}

		g := groupRuntime{Key: group.Key, Slot: slot}
		active[group.Key] = g
		all[group.Key] = g
		used[slot] = true
	}

	return active, nil
}

func saveGroupRegistry(all map[string]groupRuntime) error {
	body, _ := json.Marshal(all)
	return atomicFile(filepath.Join(stateDir, "groups", "registry.json"), body, 0o600)
}

func groupInternal(inv inventory) []string {
	result := []string{}
	for _, value := range inv.Internal {
		ip, network, err := net.ParseCIDR(value)
		if err == nil && ip.To4() != nil {
			result = append(result, network.String())
		}
	}

	return compactNetworks(result)
}

func protectGroupPool(ctx context.Context, all map[string]groupRuntime) error {
	body, err := networkCommand(ctx, "ip", "-j", "route", "show", "table", "all").Output()
	if err != nil {
		return err
	}

	var routes []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}

	if json.Unmarshal(body, &routes) != nil {
		return fmt.Errorf("invalid route inventory")
	}

	_, pool, _ := net.ParseCIDR("169.254.244.0/24")
	for _, route := range routes {
		if route.Dst == "default" || route.Dst == "" {
			continue
		}

		ip, network, e := net.ParseCIDR(route.Dst)
		if e != nil {
			ip = net.ParseIP(route.Dst)
			if ip == nil {
				continue
			}

			network = &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}
		}

		if ip.To4() == nil || (!pool.Contains(ip) && !network.Contains(pool.IP)) {
			continue
		}

		owned := false
		for _, g := range all {
			if route.Dev == g.iface() {
				owned = true
			}
		}

		if !owned {
			return fmt.Errorf("package gateway address pool already in use")
		}
	}

	return nil
}

func ensureGroupCore(ctx context.Context, group EgressGroup, g groupRuntime, resolvers []string) error {
	if !Available() {
		return fmt.Errorf("xray")
	}

	previousResolver, _ := os.ReadFile(filepath.Join("/etc/netns", g.ns(), "resolv.conf"))
	{
		err := prepareGroupNamespace(ctx, g, resolvers)
		if err != nil {
			return err
		}
	}

	binary, err := extractBinary(stateDir)
	if err != nil {
		return err
	}

	body, err := groupCoreConfig(group, g)
	if err != nil {
		return err
	}

	next := filepath.Join(g.dir(), "xray.json.next")
	{
		err = atomicFile(next, body, 0o600)
		if err != nil {
			return err
		}
	}
	{
		err = validateCore(ctx, binary, next)
		if err != nil {
			return err
		}
	}

	current, _ := os.ReadFile(filepath.Join(g.dir(), "xray.json"))
	unitPath := filepath.Join("/etc/systemd/system", g.unit())
	unit := groupUnit(g, binary)
	oldUnit, _ := os.ReadFile(unitPath)
	changed := string(previousResolver) != string(namespaceResolverConfig(resolvers)) || string(current) != string(body) || string(oldUnit) != unit || networkCommand(ctx, "systemctl", "is-active", "--quiet", g.unit()).Run() != nil
	if changed {
		{
			err = os.Rename(next, filepath.Join(g.dir(), "xray.json"))
			if err != nil {
				return err
			}
		}
		{
			err = atomicFile(unitPath, []byte(unit), 0o644)
			if err != nil {
				return err
			}
		}
		{
			err = run(ctx, "systemctl", "daemon-reload")
			if err != nil {
				return err
			}
		}
		{
			err = run(ctx, "systemctl", "enable", g.unit())
			if err != nil {
				return err
			}
		}
		{
			err = run(ctx, "systemctl", "restart", g.unit())
			if err != nil {
				return err
			}
		}
	} else {
		_ = os.Remove(next)
	}

	return nil
}

func groupHealthy(ctx context.Context, group EgressGroup, g groupRuntime) bool {
	exe, err := workerExecutable()
	if err != nil {
		return false
	}

	out, err := networkCommand(ctx, "ip", "netns", "exec", g.ns(), exe, "-network-probe-port", "21000", "-network-probe-url", group.Egress.ProbeURL).Output()
	var result ProbeResult
	return err == nil && json.Unmarshal(out, &result) == nil && result.Success
}

func groupHash(group EgressGroup) string {
	data, _ := json.Marshal(group)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Caller holds applyGate. Only the common nft transaction is shared; each core has its own readiness bit.
func (m *Manager) reconcileGroups(ctx context.Context, in Instruction, inv inventory, restore bool) error {
	{
		err := validateGroups(in)
		if err != nil {
			return err
		}
	}
	{
		err := ensureGroupOwnership(ctx)
		if err != nil {
			return err
		}
	}

	all := readGroupRegistry()
	previous := map[string]groupRuntime{}
	for key, value := range all {
		previous[key] = value
	}

	active, err := allocateGroups(in.Groups, all)
	if err != nil {
		return err
	}
	{
		err = protectGroupPool(ctx, previous)
		if err != nil {
			return err
		}
	}

	for key, g := range active {
		{
			_, ok := previous[key]
			if !ok {
				if networkCommand(ctx, "ip", "link", "show", g.iface()).Run() == nil || networkCommand(ctx, "ip", "netns", "exec", g.ns(), "true").Run() == nil {
					return fmt.Errorf("reserved group resources already exist")
				}

				routes, _ := networkCommand(ctx, "ip", "route", "show", "table", g.table()).Output()
				if len(bytes.TrimSpace(routes)) > 0 {
					return fmt.Errorf("reserved group routing table already in use")
				}

				rules, _ := networkCommand(ctx, "ip", "rule", "show").Output()
				if strings.Contains(string(rules), "lookup "+g.table()) || strings.Contains(string(rules), fmt.Sprint(17000+g.Slot)+":") {
					return fmt.Errorf("reserved group routing rule already in use")
				}
			}
		}
	}
	{
		err = saveGroupRegistry(all)
		if err != nil {
			return err
		}
	}

	resolvers, resolverErr := hostBootstrapResolvers(inv)
	if resolverErr != nil && len(in.Groups) > 0 {
		return resolverErr
	}

	in.GroupDirectDNS = "1.1.1.1"
	for _, resolver := range resolvers {
		if net.ParseIP(resolver).To4() != nil {
			in.GroupDirectDNS = resolver
			break
		}
	}

	for _, network := range inv.Networks {
		{
			ip, _, e := net.ParseCIDR(network.Config["ipv4.address"])
			if e == nil && ip.To4() != nil {
				in.GroupBridgeDNS = append(in.GroupBridgeDNS, ip.String())
			}
		}
	}

	in.GroupInternal = groupInternal(inv)
	ready := map[string]bool{}
	status := []EgressGroupStatus{}
	if m.groupChecks == nil {
		m.groupChecks = map[string]time.Time{}
		m.groupHashes = map[string]string{}
		m.groupReady = map[string]bool{}
	}
	// Install structural guards before creating/updating a gateway. Start with no newly enabled groups.
	for _, group := range in.Groups {
		ready[group.Key] = m.groupReady[group.Key] && !restore && m.groupHashes[group.Key] == groupHash(group)
	}

	bridge, inet, err := compileGroupRules(in, active, ready)
	if err != nil {
		return err
	}
	{
		err = replaceGroupRules(ctx, bridge, inet)
		if err != nil {
			return err
		}
	}

	if !restore && len(in.Bindings) > 0 {
		{
			err = installGroupBootGuard(ctx)
			if err != nil {
				return err
			}
		}
	}

	type outcome struct {
		group   EgressGroup
		state   EgressGroupStatus
		ready   bool
		hash    string
		checked time.Time
	}

	results := make([]outcome, len(in.Groups))
	var workers sync.WaitGroup
	gate := make(chan struct{}, 4)
	for index, group := range in.Groups {
		oldHash, lastCheck, wasReady := m.groupHashes[group.Key], m.groupChecks[group.Key], ready[group.Key]
		workers.Add(1)
		go func(index int, group EgressGroup, oldHash string, lastCheck time.Time, wasReady bool) {
			defer workers.Done()
			result := outcome{group: group, state: EgressGroupStatus{Key: group.Key, State: "blocked", Error: "health"}, hash: groupHash(group), checked: lastCheck}
			defer func() { results[index] = result }()
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			case <-ctx.Done():
				return
			}

			groupCtx, cancel := context.WithTimeout(ctx, 18*time.Second)
			defer cancel()
			g := active[group.Key]
			if restore {
				_ = prepareGroupNamespace(groupCtx, g, resolvers)
				return
			}

			if networkCommand(groupCtx, "systemctl", "is-active", "--quiet", g.unit()).Run() != nil {
				wasReady = false
			}
			{
				e := ensureGroupCore(groupCtx, group, g, resolvers)
				if e != nil {
					result.state.State = "error"
					result.state.Error = "xray"
					return
				}
			}

			result.ready = wasReady
			if oldHash != result.hash || time.Since(lastCheck) >= 30*time.Second || !wasReady {
				result.ready = groupHealthy(groupCtx, group, g)
				result.checked = time.Now()
			}

			if result.ready {
				result.state.State = "ready"
				result.state.Error = "none"
			}
		}(index, group, oldHash, lastCheck, wasReady)
	}

	workers.Wait()
	for _, result := range results {
		ready[result.group.Key] = result.ready
		m.groupHashes[result.group.Key] = result.hash
		m.groupReady[result.group.Key] = result.ready
		m.groupChecks[result.group.Key] = result.checked
		status = append(status, result.state)
	}

	bridge, inet, err = compileGroupRules(in, active, ready)
	if err != nil {
		return err
	}
	{
		err = replaceGroupRules(ctx, bridge, inet)
		if err != nil {
			return err
		}
	}

	for key, g := range all {
		{
			_, keep := active[key]
			if !keep {
				cleanupGroup(ctx, g)
				routes, _ := networkCommand(ctx, "ip", "route", "show", "table", g.table()).Output()
				rules, rulesErr := networkCommand(ctx, "ip", "rule", "show").Output()
				if len(bytes.TrimSpace(routes)) == 0 && rulesErr == nil && !strings.Contains(string(rules), "lookup "+g.table()) && networkCommand(ctx, "ip", "link", "show", g.iface()).Run() != nil && networkCommand(ctx, "ip", "netns", "exec", g.ns(), "true").Run() != nil {
					delete(all, key)
				}
			}
		}
	}
	{
		err = saveGroupRegistry(all)
		if err != nil {
			return err
		}
	}

	bindings := []EgressBindingStatus{}
	for _, b := range in.Bindings {
		bindings = append(bindings, EgressBindingStatus{InstanceName: b.InstanceName, Generation: b.Generation})
	}

	m.mu.Lock()
	m.status.Groups = status
	m.status.Bindings = bindings
	m.mu.Unlock()
	return nil
}
