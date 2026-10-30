package egress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	groupKeyPattern = regexp.MustCompile(`^p[1-9][0-9]{0,9}$`)
	groupNICPattern = regexp.MustCompile(`^ipg[a-f0-9]{11}$`)
)

// EgressGroup describes a shared mutable policy exit and its proxy nodes.
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

func groupTags(group EgressGroup) []string {
	tags := make([]string, 0, len(group.Nodes))
	for _, node := range group.Nodes {
		tags = append(tags, nodeTag(node.ID))
	}
	return tags
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

		if in["tag"] == "api-in" {
			in["listen"] = coreAPIPath(g.dir())
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

	dnsTarget := nodeTag(group.Nodes[0].ID)
	if len(group.Nodes) > 1 {
		dnsTarget = "group-dns-route"
		outbounds = append(outbounds, map[string]any{"tag": dnsTarget, "protocol": "loopback", "settings": map[string]any{"inboundTag": dnsTarget}})
	}
	outbounds = append(outbounds, map[string]any{"tag": "group-dns-out", "protocol": "dns", "settings": map[string]any{"network": "tcp", "address": "1.1.1.1", "port": 53, "nonIPQuery": "skip"}, "proxySettings": map[string]any{"tag": dnsTarget}})
	config["outbounds"] = outbounds
	rules := []any{
		map[string]any{"type": "field", "inboundTag": []string{"api-in"}, "outboundTag": "api"},
		map[string]any{"type": "field", "inboundTag": []string{"dns-forward"}, "outboundTag": "group-dns-out"},
	}
	for index, node := range group.Nodes {
		rules = append(rules, map[string]any{"type": "field", "inboundTag": []string{fmt.Sprintf("probe-%d", index)}, "outboundTag": nodeTag(node.ID)})
	}
	route := map[string]any{"type": "field", "inboundTag": []string{"transparent", "group-dns-query", "group-dns-route"}}
	routing, ok := config["routing"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid group routing")
	}
	if len(group.Nodes) == 1 {
		route["outboundTag"] = nodeTag(group.Nodes[0].ID)
		delete(routing, "balancers")
		delete(config, "observatory")
	} else {
		route["balancerTag"] = "egress"
	}
	routing["rules"] = append(rules, route)
	config["routing"] = routing
	return json.MarshalIndent(config, "", "  ")
}

func groupUnit(g groupRuntime, binary string) string {
	return fmt.Sprintf(`[Unit]
Description=Incudal package egress %s
After=network-online.target incudal-network.service
Requires=incudal-network.service
[Service]
Type=simple
ExecStartPre=-/usr/sbin/nft delete element inet incudal_package_egress ready { %s }
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
`, g.Key, g.mark(), g.ns(), binary, filepath.Join(g.dir(), "xray.json"), g.mark())
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

// groupPoolRouteConflict reports whether a host route claims part of the group
// gateway pool without belonging to one of our own group interfaces.
//
// Only routes whose own destination lies inside the pool conflict. Wider
// routes that merely contain the pool are ordinary host state: DHCP clients
// install 169.254.0.0/16 on the uplink, and cloud DHCP setups (OCI and most
// other providers) refresh it on every boot. Treating those as a conflict made
// every DHCP node fail reconciliation, which then blocked incus.service
// through its incudal-network.service dependency.
func groupPoolRouteConflict(dst string, device string, owned map[string]bool, pool *net.IPNet) bool {
	if dst == "default" || dst == "" {
		return false
	}

	ip, _, err := net.ParseCIDR(dst)
	if err != nil {
		ip = net.ParseIP(dst)
		if ip == nil {
			return false
		}
	}

	ip4 := ip.To4()
	if ip4 == nil || !pool.Contains(ip4) {
		return false
	}

	return !owned[device]
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
	owned := map[string]bool{}
	for _, g := range all {
		owned[g.iface()] = true
	}

	for _, route := range routes {
		if groupPoolRouteConflict(route.Dst, route.Dev, owned, pool) {
			return fmt.Errorf("package gateway address pool already in use")
		}
	}

	return nil
}

func ensureGroupCore(ctx context.Context, group EgressGroup, g groupRuntime, resolvers []string) error {
	if !Available() {
		return fmt.Errorf("embedded Xray payload is unavailable for this architecture")
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

	current, _ := os.ReadFile(filepath.Join(g.dir(), "xray.json"))
	unitPath := filepath.Join("/etc/systemd/system", g.unit())
	unit := groupUnit(g, binary)
	oldUnit, _ := os.ReadFile(unitPath)
	unchanged := string(previousResolver) == string(namespaceResolverConfig(resolvers)) && string(current) == string(body) && string(oldUnit) == unit
	healthy := unchanged && networkCommand(ctx, "systemctl", "is-active", "--quiet", g.unit()).Run() == nil && coreReady(ctx, coreAPIPath(g.dir()), groupTags(group)) == nil
	changed, err := stageCoreFiles(ctx, binary, filepath.Join(g.dir(), "xray.json"), unitPath, body, unit, !healthy)
	if err != nil {
		return err
	}
	if !changed {
		return nil
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
	return run(ctx, "systemctl", "restart", g.unit())
}

type groupHealthState struct {
	active    int
	successes map[int]int
	failures  map[int]int
}

// selectGroupFailover shares the host selector, but immediately leaves a failed active node.
func selectGroupFailover(nodes []Node, probes []ProbeResult, state *groupHealthState) int {
	if state.successes == nil {
		state.successes, state.failures = map[int]int{}, map[int]int{}
	}
	active := state.active
	for index, node := range nodes {
		if index < len(probes) && probes[index].Success {
			state.successes[node.ID]++
			state.failures[node.ID] = 0
		} else {
			state.successes[node.ID] = 0
			state.failures[node.ID]++
			if active == node.ID {
				active = 0
			}
		}
	}
	state.active = chooseFailover(nodes, active, state.successes, state.failures)
	return state.active
}

func groupHealthy(ctx context.Context, group EgressGroup, g groupRuntime, health *groupHealthState) bool {
	exe, err := workerExecutable()
	if err != nil {
		return false
	}
	probes := make([]ProbeResult, len(group.Nodes))
	var workers sync.WaitGroup
	gate := make(chan struct{}, 4)
	for index := range group.Nodes {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			case <-ctx.Done():
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			output, callErr := networkCommand(probeCtx, "ip", "netns", "exec", g.ns(), exe, "-network-probe-port", fmt.Sprint(socksBasePort+index), "-network-probe-url", group.Egress.ProbeURL).Output()
			if callErr == nil {
				_ = json.Unmarshal(output, &probes[index])
			}
		}(index)
	}
	workers.Wait()
	if ctx.Err() != nil {
		return false
	}
	usable := false
	for _, probe := range probes {
		usable = usable || probe.Success
	}
	if group.Egress.Strategy == "failover" && len(group.Nodes) > 1 {
		selected := selectGroupFailover(group.Nodes, probes, health)
		target := "blocked"
		if selected > 0 {
			target = nodeTag(selected)
		}
		if overrideCore(ctx, coreAPIPath(g.dir()), target) != nil {
			return false
		}
		usable = selected > 0
	}
	if !usable {
		return false
	}
	// DNS follows the same balancer, including TCP conversion for UDP-only client queries.
	return retryGroupHealth(ctx, func() bool {
		output, callErr := networkCommand(ctx, "ip", "netns", "exec", g.ns(), exe, "-network-probe-url", group.Egress.ProbeURL, "-network-dns-address", net.JoinHostPort(g.ip(), "53")).Output()
		var result ProbeResult
		return callErr == nil && json.Unmarshal(output, &result) == nil && result.Success
	})
}

// systemctl restart returns before Xray is ready to accept and route traffic.
// Keep the group blocked while retrying; never allow direct fallback.
func retryGroupHealth(ctx context.Context, probe func() bool) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		if probe() {
			return true
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func waitGroupCore(ctx context.Context, check func() (string, error)) (string, bool) {
	var reference string
	ready := retryGroupHealth(ctx, func() bool {
		var err error
		reference, err = check()
		return err == nil && reference != ""
	})
	return reference, ready
}

func groupHash(group EgressGroup) string {
	data, _ := json.Marshal(group)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Caller holds applyGate. Only the common nft transaction is shared; each core has its own readiness bit.
// teardownGroups removes every group a previous revision created. It never
// inspects unrelated host routes, so disabling egress cannot fail because of
// pre-existing network state.
func (m *Manager) teardownGroups(ctx context.Context) error {
	all := readGroupRegistry()
	if len(all) == 0 {
		return nil
	}

	for key, g := range all {
		cleanupGroup(ctx, g)
		routes, _ := networkCommand(ctx, "ip", "route", "show", "table", g.table()).Output()
		rules, rulesErr := networkCommand(ctx, "ip", "rule", "show").Output()
		if len(bytes.TrimSpace(routes)) == 0 && rulesErr == nil && !strings.Contains(string(rules), "lookup "+g.table()) && networkCommand(ctx, "ip", "link", "show", g.iface()).Run() != nil && networkCommand(ctx, "ip", "netns", "exec", g.ns(), "true").Run() != nil {
			delete(all, key)
			delete(m.groupChecks, key)
			delete(m.groupReady, key)
			delete(m.groupHashes, key)
			delete(m.groupRefs, key)
			delete(m.groupHealth, key)
		}
	}

	m.mu.Lock()
	m.status.Groups = []EgressGroupStatus{}
	m.status.Bindings = []EgressBindingStatus{}
	m.mu.Unlock()

	return saveGroupRegistry(all)
}

func (m *Manager) reconcileGroups(ctx context.Context, in Instruction, inv inventory, restore bool) error {
	{
		err := validateGroups(in)
		if err != nil {
			return err
		}
	}

	// No package owns a group: tear down anything a previous revision left and
	// stop. Claiming ownership and probing the gateway address pool would make an
	// untouched node pay for package egress, and unrelated host state could then
	// fail an apply that only wanted to disable egress.
	if len(in.Groups) == 0 && len(in.Bindings) == 0 {
		return m.teardownGroups(ctx)
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
		m.groupRefs = map[string]string{}
	}
	if m.groupRefs == nil {
		m.groupRefs = map[string]string{}
	}
	// Install structural guards before creating/updating a gateway. Start with no newly enabled groups.
	for _, group := range in.Groups {
		ready[group.Key] = false
		g := active[group.Key]
		if m.groupReady[group.Key] && !restore && m.groupHashes[group.Key] == groupHash(group) {
			reference, e := unitReference(ctx, g.unit())
			ready[group.Key] = e == nil && reference == m.groupRefs[group.Key] && coreReady(ctx, coreAPIPath(g.dir()), groupTags(group)) == nil
		}
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
		group     EgressGroup
		state     EgressGroupStatus
		ready     bool
		hash      string
		checked   time.Time
		reference string
	}

	results := make([]outcome, len(in.Groups))
	var workers sync.WaitGroup
	if m.groupHealth == nil {
		m.groupHealth = map[string]*groupHealthState{}
	}
	for _, group := range in.Groups {
		if m.groupHealth[group.Key] == nil {
			m.groupHealth[group.Key] = &groupHealthState{}
		}
	}
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

			groupCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
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
					log.Printf("[egress] group %s core setup failed: %v", group.Key, e)
					result.state.State = "error"
					result.state.Error = "xray"
					return
				}
			}

			reference, coreIsReady := waitGroupCore(groupCtx, func() (string, error) {
				reference, err := unitReference(groupCtx, g.unit())
				if err != nil {
					return "", err
				}
				err = coreReady(groupCtx, coreAPIPath(g.dir()), groupTags(group))
				if err != nil {
					return "", err
				}
				current, err := unitReference(groupCtx, g.unit())
				if err != nil || current != reference {
					return "", errCoreAPI
				}
				return reference, nil
			})
			if !coreIsReady {
				result.state.Error = "xray"
				return
			}
			result.reference = reference
			health := m.groupHealth[group.Key]
			if reference != m.groupRefs[group.Key] || oldHash != result.hash {
				wasReady = false
				*health = groupHealthState{}
			}
			result.ready = wasReady
			if oldHash != result.hash || time.Since(lastCheck) >= time.Duration(group.Egress.ProbeIntervalSeconds)*time.Second || !wasReady {
				result.ready = groupHealthy(groupCtx, group, g, health)
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
		m.groupRefs[result.group.Key] = result.reference
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
					delete(m.groupChecks, key)
					delete(m.groupReady, key)
					delete(m.groupHashes, key)
					delete(m.groupRefs, key)
					delete(m.groupHealth, key)
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
