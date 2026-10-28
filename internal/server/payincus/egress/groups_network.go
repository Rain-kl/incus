package egress

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Constants define protocol identifiers and timing limits.
const (
	groupMask  = "0xffff00"
	groupKeep  = "0xff0000ff"
	directMark = "0x00440000"
)

func ensureGroupOwnership(ctx context.Context) error {
	path := filepath.Join(stateDir, "groups", "owner")
	{
		body, err := os.ReadFile(path)
		if err == nil {
			if string(body) == "incudal-package-egress-v2\n" {
				return nil
			}

			return fmt.Errorf("package egress ownership conflict")
		}
	}

	for _, table := range []struct{ family, name string }{{"bridge", "incudal_package_ports"}, {"bridge", "incudal_package_boot"}, {"inet", "incudal_package_egress"}} {
		if networkCommand(ctx, "nft", "list", "table", table.family, table.name).Run() == nil {
			return fmt.Errorf("reserved package firewall already exists")
		}
	}
	{
		_, err := os.Stat("/etc/systemd/system/incudal-package-guard.service")
		if err == nil {
			return fmt.Errorf("reserved package boot service already exists")
		}
	}

	return atomicFile(path, []byte("incudal-package-egress-v2\n"), 0o600)
}

// A separate boot service survives an Agent downgrade and fails closed even
// when the installed restore worker cannot understand package bindings.
func groupBootRules() string {
	return `add table bridge incudal_package_boot
add chain bridge incudal_package_boot guard { type filter hook prerouting priority -320; policy accept; }
flush chain bridge incudal_package_boot guard
add rule bridge incudal_package_boot guard iifname "ipg*" drop
`
}

func installGroupBootGuard(ctx context.Context) error {
	path := filepath.Join(stateDir, "groups", "boot.nft")
	unit := "[Unit]\nDescription=Block package NICs until egress restoration\nAfter=local-fs.target\nBefore=incudal-network.service incus.service incus-containers.service\n[Service]\nType=oneshot\nExecStart=/usr/sbin/nft -f " + path + "\nRemainAfterExit=yes\n"
	files := map[string]string{
		path: groupBootRules(),
		"/etc/systemd/system/incudal-package-guard.service":                         unit,
		"/etc/systemd/system/incus.service.d/incudal-package-guard.conf":            "[Unit]\nRequires=incudal-package-guard.service\nAfter=incudal-package-guard.service\n",
		"/etc/systemd/system/incus-containers.service.d/incudal-package-guard.conf": "[Unit]\nRequires=incudal-package-guard.service\nAfter=incudal-package-guard.service\n",
	}

	changed := false
	for name, body := range files {
		old, _ := os.ReadFile(name)
		if string(old) == body {
			continue
		}
		{
			err := atomicFile(name, []byte(body), 0o644)
			if err != nil {
				return err
			}
		}

		changed = true
	}

	if changed {
		return run(ctx, "systemctl", "daemon-reload")
	}

	return nil
}

type groupRuntime struct {
	Key  string `json:"key"`
	Slot int    `json:"slot"`
}

func (g groupRuntime) ns() string     { return "incudal-eg-" + g.Key }
func (g groupRuntime) iface() string  { return fmt.Sprintf("ipgh%d", g.Slot) }
func (g groupRuntime) peer() string   { return "ipgn0" }
func (g groupRuntime) hostIP() string { return fmt.Sprintf("169.254.244.%d", g.Slot*4+1) }
func (g groupRuntime) ip() string     { return fmt.Sprintf("169.254.244.%d", g.Slot*4+2) }
func (g groupRuntime) mark() string   { return fmt.Sprintf("0x%x", 0x420000|g.Slot<<8) }
func (g groupRuntime) table() string  { return fmt.Sprint(8000 + g.Slot) }
func (g groupRuntime) unit() string   { return "incudal-egress-" + g.Key + ".service" }
func (g groupRuntime) dir() string    { return filepath.Join(stateDir, "groups", g.Key) }

func validateGroups(in Instruction) error {
	if len(in.Groups) > 16 || len(in.Bindings) > 10000 {
		return fmt.Errorf("invalid_request")
	}

	groups := map[string]bool{}
	interfaces := map[string]bool{}
	names := map[string]bool{}
	for _, group := range in.Groups {
		if !groupKeyPattern.MatchString(group.Key) || groups[group.Key] || group.Egress.Mode != "proxy" || len(group.Nodes) != 1 || len(group.Egress.ProxyNodeIDs) != 1 || group.Nodes[0].ID != group.Egress.ProxyNodeIDs[0] {
			return fmt.Errorf("invalid_request")
		}

		groups[group.Key] = true
	}

	for _, b := range in.Bindings {
		if b.InstanceName == "" || len(b.InstanceName) > 100 || names[b.InstanceName] || b.Generation <= 0 || (b.Mode != "direct" && b.Mode != "profile") || (b.Mode == "profile" && !groups[b.GroupKey]) || (b.Mode == "direct" && b.GroupKey != "") || len(b.Nics) == 0 || len(b.Nics) > 4 {
			return fmt.Errorf("invalid_request")
		}

		names[b.InstanceName] = true
		for _, nic := range b.Nics {
			if !groupNICPattern.MatchString(nic.HostName) || interfaces[nic.HostName] || net.ParseIP(nic.IPv4).To4() == nil || !interfacePattern.MatchString(nic.Bridge) {
				return fmt.Errorf("invalid_request")
			}

			interfaces[nic.HostName] = true
		}
	}

	return nil
}

func compileGroupRules(in Instruction, runtime map[string]groupRuntime, ready map[string]bool) (string, string, error) {
	{
		err := validateGroups(in)
		if err != nil {
			return "", "", err
		}
	}

	var bridge, inet strings.Builder
	bridge.WriteString("table bridge incudal_package_ports { chain classify { type filter hook prerouting priority -310; policy accept;\n")
	// Clear only our assigned bits on tenant ingress, before any trusted classification.
	for _, b := range in.Bindings {
		for _, nic := range b.Nics {
			mark := directMark
			if b.Mode == "profile" {
				mark = runtime[b.GroupKey].mark()
			}

			fmt.Fprintf(&bridge, "iifname %q ether type ip udp sport 68 udp dport 67 ip saddr 0.0.0.0 meta mark set meta mark & %s return\n", nic.HostName, groupKeep)
			fmt.Fprintf(&bridge, "iifname %q ether type arp arp saddr ip { 0.0.0.0, %s } return\n", nic.HostName, nic.IPv4)
			fmt.Fprintf(&bridge, "iifname %q ether type ip ip saddr %s meta mark set meta mark & %s | %s return\n", nic.HostName, nic.IPv4, groupKeep, mark)
		}
	}

	bridge.WriteString("iifname \"ipg*\" drop\n}\n}\n")
	inet.WriteString("table inet incudal_package_egress { set ready { type mark; ")
	marks := []string{}
	keys := []string{}
	for key := range runtime {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	for _, key := range keys {
		if ready[key] {
			marks = append(marks, runtime[key].mark())
		}
	}

	if len(marks) > 0 {
		inet.WriteString("elements = { " + strings.Join(marks, ", ") + " }; ")
	}

	inet.WriteString("}\n")
	// DNS is intercepted before Incus's shared bridge dnsmasq sees the request.
	inet.WriteString("chain dns { type nat hook prerouting priority -110; policy accept;\n")
	for _, b := range in.Bindings {
		if b.Mode != "direct" || len(in.GroupBridgeDNS) == 0 {
			continue
		}

		for _, nic := range b.Nics {
			fmt.Fprintf(&inet, "iifname %q ip saddr %s ip daddr { %s } meta mark & %s == %s meta l4proto { tcp, udp } th dport 53 ct mark set ct mark & %s | %s dnat ip to %s:53\n", nic.Bridge, nic.IPv4, strings.Join(in.GroupBridgeDNS, ", "), groupMask, directMark, groupKeep, directMark, in.GroupDirectDNS)
		}
	}

	for _, b := range in.Bindings {
		if b.Mode != "profile" {
			continue
		}

		g := runtime[b.GroupKey]
		for _, nic := range b.Nics {
			fmt.Fprintf(&inet, "iifname %q ip saddr %s meta mark & %s == %s meta l4proto { tcp, udp } th dport 53 ct mark set ct mark & %s | %s dnat ip to %s:53\n", nic.Bridge, nic.IPv4, groupMask, g.mark(), groupKeep, g.mark(), g.ip())
		}
	}

	inet.WriteString("}\nchain classify { type filter hook prerouting priority -91; policy accept;\n")
	inet.WriteString("iifname \"ipgh*\" meta mark set meta mark & " + groupKeep + " return\n")
	inet.WriteString("ct direction reply meta mark set meta mark & " + groupKeep + " return\nct status dnat meta mark set meta mark & " + groupKeep + " return\n")
	// Match the bridge AND a host-assigned mark; an arbitrary WAN packet cannot select a group.
	for _, b := range in.Bindings {
		for _, nic := range b.Nics {
			mark := directMark
			if b.Mode == "profile" {
				mark = runtime[b.GroupKey].mark()
			}

			fmt.Fprintf(&inet, "iifname %q ip saddr %s meta mark & %s == %s goto route_%s\n", nic.Bridge, nic.IPv4, groupMask, mark, strings.TrimPrefix(mark, "0x"))
		}
	}
	// Stale marks on an unbound tenant may never select a slot reused by another policy.
	inet.WriteString("meta mark & 0x00ff0000 == 0x00420000 meta mark & 0x0000ff00 != 0 drop\nmeta mark & " + groupMask + " == " + directMark + " drop\nct mark & 0x00ff0000 == 0x00420000 ct mark & 0x0000ff00 != 0 drop\n}\n")
	used := map[string]bool{}
	for _, b := range in.Bindings {
		mark := directMark
		if b.Mode == "profile" {
			mark = runtime[b.GroupKey].mark()
		}

		if used[mark] {
			continue
		}

		used[mark] = true
		fmt.Fprintf(&inet, "chain route_%s {\n", strings.TrimPrefix(mark, "0x"))
		fmt.Fprintf(&inet, "ct mark & %s != 0 ct mark & %s != %s drop\n", groupMask, groupMask, mark)
		inet.WriteString("ip daddr 169.254.244.0/24 drop\n")
		if b.Mode == "profile" {
			// Only actual host-managed networks are bypassed, not all RFC1918 destinations.
			for _, cidr := range in.GroupInternal {
				fmt.Fprintf(&inet, "ip daddr %s meta mark set meta mark & "+groupKeep+" return\n", cidr)
			}

			inet.WriteString("ip daddr { 127.0.0.0/8, 169.254.0.0/16, 224.0.0.0/4, 255.255.255.255/32 } meta mark set meta mark & " + groupKeep + " return\nmeta l4proto != { tcp, udp } drop\n")
		}

		fmt.Fprintf(&inet, "ct mark set ct mark & %s | %s meta mark set meta mark & %s | %s\n}\n", groupKeep, mark, groupKeep, mark)
	}

	inet.WriteString("chain guard { type filter hook forward priority 9; policy accept;\n")
	for _, key := range keys {
		g := runtime[key]
		fmt.Fprintf(&inet, "ct mark & %s == %s jump guard_%d\n", groupMask, g.mark(), g.Slot)
	}

	inet.WriteString("}\n")
	for _, key := range keys {
		g := runtime[key]
		fmt.Fprintf(&inet, "chain guard_%d { ct mark & %s != @ready drop\nct direction reply return\noifname != %q drop\n}\n", g.Slot, groupMask, g.iface())
	}

	inet.WriteString("chain nat_gateway { type nat hook postrouting priority 99; policy accept;\n")
	for _, key := range keys {
		g := runtime[key]
		fmt.Fprintf(&inet, "iifname %q ip saddr %s masquerade\n", g.iface(), g.ip())
	}

	inet.WriteString("}\n}\n")
	return bridge.String(), inet.String(), nil
}

func replaceGroupRules(ctx context.Context, bridge, inet string) error {
	script := ""
	if networkCommand(ctx, "nft", "list", "table", "bridge", "incudal_package_boot").Run() == nil {
		script += "delete table bridge incudal_package_boot\n"
	}

	for _, table := range []struct{ family, name string }{{"bridge", "incudal_package_ports"}, {"inet", "incudal_package_egress"}} {
		if networkCommand(ctx, "nft", "list", "table", table.family, table.name).Run() == nil {
			script += "delete table " + table.family + " " + table.name + "\n"
		}
	}

	return nft(ctx, script+bridge+inet, false)
}

func prepareGroupNamespace(ctx context.Context, g groupRuntime, resolvers []string) error {
	if networkCommand(ctx, "ip", "netns", "exec", g.ns(), "true").Run() != nil {
		{
			err := run(ctx, "ip", "netns", "add", g.ns())
			if err != nil {
				return err
			}
		}
	}

	if networkCommand(ctx, "ip", "link", "show", g.iface()).Run() != nil {
		{
			err := run(ctx, "ip", "link", "add", g.iface(), "type", "veth", "peer", "name", g.peer(), "netns", g.ns())
			if err != nil {
				return err
			}
		}
	}

	commands := [][]string{
		{"ip", "addr", "replace", g.hostIP() + "/30", "dev", g.iface()},
		{"ip", "link", "set", g.iface(), "up"},
		{"ip", "-n", g.ns(), "link", "set", "lo", "up"},
		{"ip", "-n", g.ns(), "link", "set", g.peer(), "up"},
		{"ip", "-n", g.ns(), "addr", "replace", g.ip() + "/30", "dev", g.peer()},
		{"ip", "-n", g.ns(), "route", "replace", "default", "via", g.hostIP()},
		{"ip", "netns", "exec", g.ns(), "sysctl", "-qw", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0", "net.ipv4.conf.ipgn0.rp_filter=0", "net.ipv4.conf.all.src_valid_mark=0", "net.ipv4.conf.default.src_valid_mark=0", "net.ipv4.conf.ipgn0.src_valid_mark=0"},
		{"sysctl", "-qw", "net.ipv4.conf." + g.iface() + ".rp_filter=2", "net.ipv4.conf." + g.iface() + ".forwarding=1"},
		{"ip", "route", "replace", "unreachable", "default", "metric", "42760", "table", g.table()},
		{"ip", "route", "replace", "default", "via", g.ip(), "dev", g.iface(), "onlink", "metric", "10", "table", g.table()},
		{"ip", "-n", g.ns(), "route", "replace", "local", "default", "dev", "lo", "table", "7421"},
	}

	for _, args := range commands {
		{
			err := run(ctx, args[0], args[1:]...)
			if err != nil {
				return err
			}
		}
	}

	out, _ := networkCommand(ctx, "ip", "rule", "show").Output()
	rule := g.mark() + "/" + groupMask + " lookup " + g.table()
	if !strings.Contains(string(out), rule) {
		{
			err := run(ctx, "ip", "rule", "add", "pref", fmt.Sprint(17000+g.Slot), "fwmark", g.mark()+"/"+groupMask, "lookup", g.table())
			if err != nil {
				return err
			}
		}
	}

	out, _ = networkCommand(ctx, "ip", "-n", g.ns(), "rule", "show").Output()
	if !strings.Contains(string(out), "lookup 7421") {
		{
			err := run(ctx, "ip", "-n", g.ns(), "rule", "add", "pref", "17421", "fwmark", "0x430000/0xff0000", "lookup", "7421")
			if err != nil {
				return err
			}
		}
	}
	{
		err := atomicFile(filepath.Join("/etc/netns", g.ns(), "resolv.conf"), namespaceResolverConfig(resolvers), 0o600)
		if err != nil {
			return err
		}
	}

	script := fmt.Sprintf("table inet incudal_gateway { chain intercept { type filter hook prerouting priority -150; policy accept; ct direction reply return\nip daddr %s return\niifname %q meta l4proto { tcp, udp } tproxy to :15490 meta mark set 0x430000 accept\n}\nchain no_forward { type filter hook forward priority 0; policy drop; }\n}\n", g.ip(), g.peer())
	if networkCommand(ctx, "ip", "netns", "exec", g.ns(), "nft", "list", "table", "inet", "incudal_gateway").Run() == nil {
		script = "delete table inet incudal_gateway\n" + script
	}

	cmd := networkCommand(ctx, "ip", "netns", "exec", g.ns(), "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	{
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("group nft failed")
		}
	}

	return nil
}

func cleanupGroup(ctx context.Context, g groupRuntime) {
	closeCoreAPI(coreAPIPath(g.dir()))
	_ = run(ctx, "systemctl", "disable", "--now", g.unit())
	_ = run(ctx, "ip", "rule", "del", "pref", fmt.Sprint(17000+g.Slot), "fwmark", g.mark()+"/"+groupMask, "lookup", g.table())
	_ = run(ctx, "ip", "route", "flush", "table", g.table())
	_ = run(ctx, "ip", "link", "del", g.iface())
	_ = run(ctx, "ip", "netns", "del", g.ns())
	_ = os.Remove(filepath.Join("/etc/systemd/system", g.unit()))
}
