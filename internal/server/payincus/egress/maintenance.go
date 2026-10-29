package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// NodeSummary intentionally excludes authentication credentials and outbound options.
type NodeSummary struct {
	ID       int    `json:"id"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
}

func summarizeNode(node Node) NodeSummary {
	var outbound struct {
		Protocol string `json:"protocol"`
		Settings struct {
			Servers []struct {
				Address string `json:"address"`
				Port    int    `json:"port"`
			} `json:"servers"`
			Vnext []struct {
				Address string `json:"address"`
				Port    int    `json:"port"`
			} `json:"vnext"`
		} `json:"settings"`
	}
	_ = json.Unmarshal(node.Outbound, &outbound)
	result := NodeSummary{ID: node.ID, Protocol: outbound.Protocol}
	if len(outbound.Settings.Servers) > 0 {
		result.Address, result.Port = outbound.Settings.Servers[0].Address, outbound.Settings.Servers[0].Port
	}
	if len(outbound.Settings.Vnext) > 0 {
		result.Address, result.Port = outbound.Settings.Vnext[0].Address, outbound.Settings.Vnext[0].Port
	}
	return result
}

// Inspect returns the effective node catalog and status without disclosing proxy passwords.
func (m *Manager) Inspect() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodes := []NodeSummary{}
	seen := map[int]bool{}
	appendNodes := func(items []Node) {
		for _, node := range items {
			if !seen[node.ID] {
				nodes = append(nodes, summarizeNode(node))
				seen[node.ID] = true
			}
		}
	}
	if m.desired != nil {
		appendNodes(m.desired.Nodes)
		for _, group := range m.desired.Groups {
			appendNodes(group.Nodes)
		}
	}
	return struct {
		Status Status        `json:"status"`
		Nodes  []NodeSummary `json:"nodes"`
	}{m.status, nodes}
}

// TestConfigured probes the currently assigned nodes in bounded batches, without persisting results.
func (m *Manager) TestConfigured(ctx context.Context) (any, error) {
	m.mu.Lock()
	if m.desired == nil {
		m.mu.Unlock()
		return map[string]any{"items": []ProbeResult{}}, nil
	}
	in := *m.desired
	m.mu.Unlock()
	requests := []ProbeRequest{{ProbeURL: in.Egress.ProbeURL, Nodes: in.Nodes}}
	for _, group := range in.Groups {
		requests = append(requests, ProbeRequest{ProbeURL: group.Egress.ProbeURL, Nodes: group.Nodes})
	}
	seen := map[int]bool{}
	items := []ProbeResult{}
	for _, request := range requests {
		nodes := []Node{}
		for _, node := range request.Nodes {
			if !seen[node.ID] {
				nodes = append(nodes, node)
				seen[node.ID] = true
			}
		}
		for i := 0; i < len(nodes); i += 10 {
			request.Nodes = nodes[i:min(i+10, len(nodes))]
			result, err := m.Probe(ctx, request)
			if err != nil {
				return nil, err
			}
			items = append(items, result...)
		}
	}
	return map[string]any{"items": items}, nil
}

func removeManagedFile(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Maintain restarts cores or recreates owned runtime resources while retaining fail-closed kernel guards and intent.
func (m *Manager) Maintain(ctx context.Context, reinstall bool) (any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	select {
	case m.applyGate <- struct{}{}:
		defer func() { <-m.applyGate }()
	default:
		return nil, fmt.Errorf("busy")
	}
	select {
	case m.probeGate <- struct{}{}:
		defer func() { <-m.probeGate }()
	default:
		return nil, fmt.Errorf("busy")
	}
	m.mu.Lock()
	desired := m.desired
	m.mu.Unlock()
	if desired == nil {
		return nil, fmt.Errorf("no network policy configured")
	}
	owner, err := os.ReadFile(filepath.Join(m.dir, "owner"))
	if err != nil || string(owner) != "incudal-network-v1\n" {
		return nil, fmt.Errorf("network ownership unavailable")
	}
	registry := map[string]groupRuntime{}
	body, readErr := os.ReadFile(filepath.Join(m.dir, "groups", "registry.json"))
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if readErr == nil && json.Unmarshal(body, &registry) != nil {
		return nil, fmt.Errorf("invalid resource registry")
	}
	groups := make([]groupRuntime, 0, len(registry))
	for key, group := range registry {
		if key != group.Key || !groupKeyPattern.MatchString(key) || group.Slot < 1 || group.Slot > 63 {
			return nil, fmt.Errorf("invalid resource registry")
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Slot < groups[j].Slot })
	// Clear readiness before stopping any process; keep classification and unreachable routes installed throughout.
	if desired.Egress.Mode != "off" {
		err = nft(ctx, "flush set inet incudal_egress ready", false)
		if err != nil {
			return nil, err
		}
	}
	if len(groups) > 0 {
		if err = nft(ctx, "flush set inet incudal_package_egress ready", false); err != nil {
			return nil, err
		}
	}
	m.lastHash = ""
	m.groupHashes, m.groupReady, m.groupRefs, m.groupChecks = nil, nil, nil, nil
	m.groupHealth = nil
	units := []string{"incudal-xray.service"}
	for _, group := range groups {
		units = append(units, group.unit())
	}
	for _, unit := range units {
		loadState, inspectErr := networkCommand(ctx, "systemctl", "show", "--property=LoadState", "--value", unit).Output()
		if inspectErr != nil {
			return nil, inspectErr
		}
		if strings.TrimSpace(string(loadState)) == "not-found" {
			continue
		}
		err = run(ctx, "systemctl", "stop", unit)
		if err != nil {
			return nil, err
		}
	}
	if reinstall {
		for _, group := range groups {
			closeCoreAPI(coreAPIPath(group.dir()))
			if err = resetNamespace(ctx, group.ns(), group.iface()); err != nil {
				return nil, err
			}
			for _, path := range []string{filepath.Join(group.dir(), "xray.json"), coreAPIPath(group.dir()), filepath.Join("/etc/systemd/system", group.unit())} {
				if err = removeManagedFile(path); err != nil {
					return nil, err
				}
			}
			if err = os.RemoveAll(filepath.Join("/etc/netns", group.ns())); err != nil {
				return nil, err
			}
		}
		closeCoreAPI(coreAPIPath(m.dir))
		if err = resetNamespace(ctx, namespace, "igw-host"); err != nil {
			return nil, err
		}
		for _, path := range []string{filepath.Join(m.dir, "xray.json"), coreAPIPath(m.dir), "/etc/systemd/system/incudal-xray.service"} {
			if err = removeManagedFile(path); err != nil {
				return nil, err
			}
		}
		if err = os.RemoveAll(filepath.Join("/etc/netns", namespace)); err != nil {
			return nil, err
		}
		if err = run(ctx, "systemctl", "daemon-reload"); err != nil {
			return nil, err
		}
	}
	m.lastHash, m.legacyRef = "", ""
	m.lastHealth = time.Time{}
	m.groupHashes, m.groupReady, m.groupRefs, m.groupChecks = nil, nil, nil, nil
	m.groupHealth = nil
	m.coreRestartRequired = true
	m.reconcileLocked(ctx, true)
	status := m.Report()
	if status.State != "applied" {
		return nil, errors.New("network configuration incomplete; inspect Incus journal")
	}
	return status, nil
}

func resetNamespace(ctx context.Context, name, iface string) error {
	if networkCommand(ctx, "ip", "link", "show", iface).Run() == nil {
		deleteErr := run(ctx, "ip", "link", "del", iface)
		if deleteErr != nil {
			return deleteErr
		}
	}
	output, err := networkCommand(ctx, "ip", "netns", "list").Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.Fields(line)
		if len(parts) > 0 && parts[0] == name {
			return run(ctx, "ip", "netns", "del", name)
		}
	}
	return nil
}
