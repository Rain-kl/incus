package egress

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Constants define protocol identifiers and timing limits.
const (
	mark = "0x420000"
	mask = "0xff0000"
)

var interfacePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)

func ensureOwnership(ctx context.Context) error {
	owner := filepath.Join(stateDir, "owner")
	{
		body, err := os.ReadFile(owner)
		if err == nil {
			if string(body) == "incudal-network-v1\n" {
				return nil
			}

			return fmt.Errorf("reserved network ownership conflict")
		}
	}

	if networkCommand(ctx, "ip", "link", "show", "igw-host").Run() == nil || networkCommand(ctx, "ip", "netns", "exec", namespace, "true").Run() == nil || networkCommand(ctx, "nft", "list", "table", "inet", "incudal_egress").Run() == nil {
		return fmt.Errorf("reserved network resources already exist")
	}

	for _, path := range []string{"/etc/systemd/system/incudal-network.service", "/etc/systemd/system/incudal-xray.service", "/etc/systemd/system/incus.service.d/incudal-network.conf"} {
		{
			_, err := os.Stat(path)
			if err == nil {
				return fmt.Errorf("reserved service resource already exists")
			}
		}
	}

	for _, family := range []string{"-4", "-6"} {
		routes, err := networkCommand(ctx, "ip", family, "route", "show", "table", "7420").Output()
		if err == nil && len(bytes.TrimSpace(routes)) > 0 {
			return fmt.Errorf("reserved routing table already in use")
		}

		rules, _ := networkCommand(ctx, "ip", family, "rule", "show").Output()
		if strings.Contains(string(rules), "17420:") {
			return fmt.Errorf("reserved routing priority already in use")
		}
	}

	return atomicFile(owner, []byte("incudal-network-v1\n"), 0o600)
}

func compactNetworks(values []string) []string {
	networks := []*net.IPNet{}
	for _, value := range values {
		if !strings.Contains(value, "/") {
			if net.ParseIP(value).To4() != nil {
				value += "/32"
			} else {
				value += "/128"
			}
		}

		_, network, err := net.ParseCIDR(value)
		if err != nil {
			continue
		}

		networks = append(networks, network)
	}

	sort.Slice(networks, func(i, j int) bool { a, _ := networks[i].Mask.Size(); b, _ := networks[j].Mask.Size(); return a < b })
	result := []string{}
	kept := []*net.IPNet{}
	for _, network := range networks {
		covered := false
		for _, prior := range kept {
			if prior.Contains(network.IP) {
				covered = true
				break
			}
		}

		if !covered {
			kept = append(kept, network)
			result = append(result, network.String())
		}
	}

	return result
}

func run(ctx context.Context, name string, args ...string) error {
	// Never include command output in an error: Xray/Incus may echo credentials.
	{
		err := networkCommand(ctx, name, args...).Run()
		if err != nil {
			return fmt.Errorf("%s failed", filepath.Base(name))
		}
	}

	return nil
}

func nft(ctx context.Context, script string, netns bool) error {
	args := []string{"-f", "-"}
	name := "nft"
	if netns {
		name = "ip"
		args = append([]string{"netns", "exec", namespace, "nft"}, args...)
	}

	cmd := networkCommand(ctx, name, args...)
	cmd.Stdin = strings.NewReader(script)
	{
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("nft transaction failed")
		}
	}

	return nil
}

func checkTransparentProxy(ctx context.Context) error {
	// --check validates the running kernel's nft_tproxy support without
	// changing any firewall tables. Fail before modifying Incus ACLs or DNS.
	cmd := networkCommand(ctx, "nft", "--check", "-f", "-")
	cmd.Stdin = strings.NewReader(strings.Replace(namespaceRules(), "incudal_gateway", "incudal_tproxy_preflight", 1))
	{
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("kernel transparent proxy support unavailable")
		}
	}

	return nil
}

func quotedInterfaces(values []string) (string, error) {
	items := []string{}
	seen := map[string]bool{}
	for _, v := range values {
		if !interfacePattern.MatchString(v) {
			return "", fmt.Errorf("invalid interface")
		}

		if !seen[v] {
			items = append(items, fmt.Sprintf("%q", v))
			seen[v] = true
		}
	}

	sort.Strings(items)
	return strings.Join(items, ", "), nil
}

func subjectRules(rule Rule) ([]string, error) {
	parts := []string{}
	family := ""
	for _, item := range []struct{ key, value string }{{"saddr", rule.Source}, {"daddr", rule.Destination}} {
		if item.value == "" {
			continue
		}

		values := []string{}
		for _, v := range strings.Split(item.value, ",") {
			v = strings.TrimSpace(v)
			ip := net.ParseIP(v)
			if strings.Contains(v, "/") {
				parsed, _, e := net.ParseCIDR(v)
				if e != nil {
					return nil, e
				}

				ip = parsed
			}

			if ip == nil {
				return nil, fmt.Errorf("invalid ACL address")
			}

			f := "ip6"
			if ip.To4() != nil {
				f = "ip"
			}

			if family != "" && family != f {
				return nil, fmt.Errorf("mixed ACL family")
			}

			family = f
			values = append(values, v)
		}

		parts = append(parts, fmt.Sprintf("%s %s { %s }", family, item.key, strings.Join(values, ", ")))
	}

	switch rule.Protocol {
	case "tcp", "udp":
		parts = append(parts, "meta l4proto "+rule.Protocol)
	case "icmp4":
		parts = append(parts, "meta l4proto icmp")
	case "icmp6":
		parts = append(parts, "meta l4proto ipv6-icmp")
	case "":
	default:
		return nil, fmt.Errorf("invalid ACL protocol")
	}

	for _, item := range []struct{ key, value string }{{"sport", rule.SourcePort}, {"dport", rule.DestinationPort}} {
		if item.value != "" {
			if rule.Protocol != "tcp" && rule.Protocol != "udp" {
				return nil, fmt.Errorf("invalid ACL ports")
			}

			if !regexp.MustCompile(`^[0-9,-]+$`).MatchString(item.value) {
				return nil, fmt.Errorf("invalid ACL ports")
			}

			parts = append(parts, rule.Protocol+" "+item.key+" { "+item.value+" }")
		}
	}

	return parts, nil
}

func firewallChain(f Firewall, direction string) (string, error) {
	var b strings.Builder
	b.WriteString("ct state established,related accept\n")
	for _, action := range []string{"drop", "reject", "allow"} {
		for _, rule := range f.Rules {
			if !rule.Enabled || rule.Direction != direction || rule.Action != action {
				continue
			}

			parts, err := subjectRules(rule)
			if err != nil {
				return "", err
			}

			verdict := action
			if verdict == "allow" {
				verdict = "accept"
			}

			fmt.Fprintf(&b, "%s %s\n", strings.Join(parts, " "), verdict)
		}
	}

	action := f.EgressAction
	if direction == "ingress" {
		action = f.IngressAction
	}

	if action == "allow" {
		action = "accept"
	}

	if action != "accept" && action != "drop" && action != "reject" {
		return "", fmt.Errorf("invalid default ACL action")
	}

	b.WriteString(action + "\n")
	return b.String(), nil
}

func hostRules(in Instruction, inv inventory) (string, error) {
	interfaces := append(append([]string{}, inv.Bridges...), inv.Routed...)
	names, err := quotedInterfaces(interfaces)
	if err != nil {
		return "", err
	}

	scope := `iifname "iceg*"`
	outScope := `oifname "iceg*"`
	if names != "" {
		scope = "iifname { " + names + `, "iceg*" }`
		outScope = "oifname { " + names + `, "iceg*" }`
	}

	egressMatches := []string{scope}
	ingressMatches := []string{outScope}
	routedIn := []string{`iifname "iceg*"`}
	routedOut := []string{`oifname "iceg*"`}
	{
		routedNames, e := quotedInterfaces(inv.Routed)
		if e != nil {
			return "", e
		} else if routedNames != "" {
			routedIn = []string{"iifname { " + routedNames + `, "iceg*" }`}
			routedOut = []string{"oifname { " + routedNames + `, "iceg*" }`}
		}
	}

	routed4, routed6 := []string{}, []string{}
	for _, value := range inv.RoutedSources {
		ip, _, e := net.ParseCIDR(value)
		if e != nil {
			return "", e
		}

		if ip.To4() != nil {
			routed4 = append(routed4, value)
		} else {
			routed6 = append(routed6, value)
		}
	}

	for _, item := range []struct {
		family string
		values []string
	}{{"ip", compactNetworks(routed4)}, {"ip6", compactNetworks(routed6)}} {
		if len(item.values) > 0 {
			src := item.family + " saddr { " + strings.Join(item.values, ", ") + " }"
			dst := item.family + " daddr { " + strings.Join(item.values, ", ") + " }"
			egressMatches = append(egressMatches, src)
			ingressMatches = append(ingressMatches, dst)
			routedIn = append(routedIn, src)
			routedOut = append(routedOut, dst)
		}
	}

	v4 := []string{"127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32"}
	v6 := []string{"::1/128", "fe80::/10", "ff00::/8"}
	for _, value := range append(append([]string{}, inv.Internal...), in.Egress.InternalCIDRs...) {
		ip := net.ParseIP(value)
		if strings.Contains(value, "/") {
			var e error
			ip, _, e = net.ParseCIDR(value)
			if e != nil {
				return "", e
			}
		}

		if ip == nil {
			return "", fmt.Errorf("invalid internal network")
		}

		if ip.To4() != nil {
			v4 = append(v4, value)
		} else {
			v6 = append(v6, value)
		}
	}

	v4 = compactNetworks(v4)
	v6 = compactNetworks(v6)
	var b strings.Builder
	b.WriteString("table inet incudal_egress {\n set ready { type mark; }\n")
	matches := func(conditions []string, chain string) {
		for _, condition := range conditions {
			fmt.Fprintf(&b, " %s jump %s\n", condition, chain)
		}
	}

	if in.Firewall.Enabled {
		for _, direction := range []string{"ingress", "egress"} {
			rules, e := firewallChain(in.Firewall, direction)
			if e != nil {
				return "", e
			}

			fmt.Fprintf(&b, "chain %s {\n%s}\n", direction, rules)
		}
		// Separate base chains are essential: accept in egress must not skip an
		// ingress restriction when two managed instances communicate.
		b.WriteString("chain acl_forward { type filter hook forward priority -6; policy accept;\n")
		matches(egressMatches, "egress")
		b.WriteString("}\n")
		b.WriteString("chain acl_ingress_forward { type filter hook forward priority -5; policy accept;\n")
		matches(ingressMatches, "ingress")
		b.WriteString("}\n")
		b.WriteString("chain routed_input { type filter hook input priority -5; policy accept;\n")
		for _, condition := range routedIn {
			fmt.Fprintf(&b, " %s meta l4proto ipv6-icmp icmpv6 type { nd-router-solicit, nd-neighbor-solicit, nd-neighbor-advert } accept\n", condition)
		}

		matches(routedIn, "egress")
		b.WriteString("}\n")
		b.WriteString("chain routed_output { type filter hook output priority -5; policy accept;\n")
		for _, condition := range routedOut {
			fmt.Fprintf(&b, " %s meta l4proto ipv6-icmp icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n", condition)
		}

		matches(routedOut, "ingress")
		b.WriteString("}\n")
	}

	if in.Egress.Mode != "off" {
		b.WriteString("chain classify { type filter hook prerouting priority -90; policy accept;\n iifname \"igw-host\" meta mark set meta mark & 0xff00ffff return\n ct direction reply return\n ct status dnat return\n")
		if in.SchemaVersion >= 2 {
			b.WriteString(" meta mark & 0x00ff0000 == 0x00440000 return\n ct mark & 0x00ff0000 == 0x00440000 return\n ct mark & 0x00ff0000 == 0x00420000 ct mark & 0x0000ff00 != 0 return\n")
		}

		matches(egressMatches, "selected")
		b.WriteString("}\n")
		fmt.Fprintf(&b, `chain selected {
 ct mark & %s == %s meta mark set meta mark & 0xff00ffff | %s return
 ct state != new return
 ip daddr { %s } return
 ip6 daddr { %s } return
 meta l4proto { tcp, udp } ct mark set ct mark & 0xff00ffff | %s meta mark set meta mark & 0xff00ffff | %s
}

chain guard { type filter hook forward priority 10; policy accept;
`, mask, mark, mark, strings.Join(v4, ", "), strings.Join(v6, ", "), mark, mark)
		if in.SchemaVersion >= 2 {
			b.WriteString(" meta mark & 0x00ff0000 == 0x00440000 return\n ct mark & 0x00ff0000 == 0x00440000 return\n ct mark & 0x00ff0000 == 0x00420000 ct mark & 0x0000ff00 != 0 return\n")
		}

		matches(egressMatches, "selected_guard")
		b.WriteString("}\n")
		fmt.Fprintf(&b, `chain selected_guard {
 ip daddr 169.254.242.0/30 drop
 ip6 daddr fd42:696e:6375::/126 drop
 ct direction reply return
 ct status dnat return
 ct mark & %s == %s ct mark & %s != @ready drop
 ct mark & %s == %s oifname != "igw-host" drop
 ip daddr { %s } return
 ip6 daddr { %s } return
 ct state { invalid, untracked } drop
 ct state new meta l4proto != { tcp, udp } drop
}

chain masquerade_gateway { type nat hook postrouting priority 99; policy accept;
 iifname "igw-host" ip saddr 169.254.242.2 masquerade
 iifname "igw-host" ip6 saddr fd42:696e:6375::2 masquerade
}
`, mask, mark, mask, mask, mark, strings.Join(v4, ", "), strings.Join(v6, ", "))
	}

	b.WriteString("}\n")
	return b.String(), nil
}

func replaceHostRules(ctx context.Context, rules string) error {
	prefix := ""
	if networkCommand(ctx, "nft", "list", "table", "inet", "incudal_egress").Run() == nil {
		prefix = "delete table inet incudal_egress\n"
	}

	return nft(ctx, prefix+rules, false)
}

func prepareNamespace(ctx context.Context, resolvers ...string) error {
	if len(resolvers) == 0 {
		var err error
		resolvers, err = hostBootstrapResolvers(inventory{})
		if err != nil {
			return err
		}
	}
	{
		err := ensureHostNamespace(ctx)
		if err != nil {
			return err
		}
	}

	if networkCommand(ctx, "ip", "link", "show", "igw-host").Run() != nil {
		{
			err := run(ctx, "ip", "link", "add", "igw-host", "type", "veth", "peer", "name", "igw-ns", "netns", namespace)
			if err != nil {
				return err
			}
		}
	}

	commands := [][]string{
		{"ip", "addr", "replace", "169.254.242.1/30", "dev", "igw-host"},
		{"ip", "-6", "addr", "replace", "fd42:696e:6375::1/126", "dev", "igw-host", "nodad"},
		{"ip", "link", "set", "igw-host", "up"},
		{"ip", "netns", "exec", namespace, "ip", "link", "set", "lo", "up"},
		{"ip", "netns", "exec", namespace, "ip", "link", "set", "igw-ns", "up"},
		{"ip", "netns", "exec", namespace, "ip", "addr", "replace", "169.254.242.2/30", "dev", "igw-ns"},
		{"ip", "netns", "exec", namespace, "ip", "-6", "addr", "replace", "fd42:696e:6375::2/126", "dev", "igw-ns", "nodad"},
		{"ip", "netns", "exec", namespace, "ip", "route", "replace", "default", "via", "169.254.242.1"},
		{"ip", "netns", "exec", namespace, "ip", "-6", "route", "replace", "default", "via", "fd42:696e:6375::1"},
		// The local catch-all table is only for TPROXY destination routing. Source
		// validation must not use that mark, or it treats the remote source as a
		// local address and rejects the SYN even when rp_filter is disabled. Both
		// all and per-interface values matter; Linux uses their maximum.
		{"ip", "netns", "exec", namespace, "sysctl", "-qw", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0", "net.ipv4.conf.igw-ns.rp_filter=0", "net.ipv4.conf.all.src_valid_mark=0", "net.ipv4.conf.default.src_valid_mark=0", "net.ipv4.conf.igw-ns.src_valid_mark=0"},
	}

	for _, args := range commands {
		{
			err := run(ctx, args[0], args[1:]...)
			if err != nil {
				return err
			}
		}
	}
	// These settings affect only the new gateway interface, not existing uplinks.
	{
		err := run(ctx, "sysctl", "-qw", "net.ipv4.conf.igw-host.rp_filter=2", "net.ipv4.conf.igw-host.forwarding=1", "net.ipv6.conf.igw-host.forwarding=1")
		if err != nil {
			return err
		}
	}

	for _, family := range []string{"-4", "-6"} {
		gw := "169.254.242.2"
		if family == "-6" {
			gw = "fd42:696e:6375::2"
		}
		{
			err := run(ctx, "ip", family, "route", "replace", "unreachable", "default", "metric", "42760", "table", "7420")
			if err != nil {
				return err
			}
		}
		{
			err := run(ctx, "ip", family, "route", "replace", "default", "via", gw, "dev", "igw-host", "onlink", "metric", "10", "table", "7420")
			if err != nil {
				return err
			}
		}
		// Adding only if absent avoids accumulating duplicate rules on every sync.
		out, _ := networkCommand(ctx, "ip", family, "rule", "show").Output()
		if !strings.Contains(string(out), "fwmark "+mark+"/"+mask+" lookup 7420") {
			{
				err := run(ctx, "ip", family, "rule", "add", "pref", "17420", "fwmark", mark+"/"+mask, "lookup", "7420")
				if err != nil {
					return err
				}
			}
		}
		{
			err := run(ctx, "ip", "netns", "exec", namespace, "ip", family, "route", "replace", "local", "default", "dev", "lo", "table", "7421")
			if err != nil {
				return err
			}
		}

		out, _ = networkCommand(ctx, "ip", "netns", "exec", namespace, "ip", family, "rule", "show").Output()
		if !strings.Contains(string(out), "lookup 7421") {
			{
				err := run(ctx, "ip", "netns", "exec", namespace, "ip", family, "rule", "add", "pref", "17421", "fwmark", "0x430000/0xff0000", "lookup", "7421")
				if err != nil {
					return err
				}
			}
		}
	}
	// Bootstrap proxy hostnames through the host's real upstreams. Container DNS
	// still uses its selected Xray outbound; this resolver is for core bootstrap.
	{
		err := atomicFile(namespaceResolverPath, namespaceResolverConfig(resolvers), 0o600)
		if err != nil {
			return err
		}
	}

	script := namespaceRules()
	prefix := ""
	if networkCommand(ctx, "ip", "netns", "exec", namespace, "nft", "list", "table", "inet", "incudal_gateway").Run() == nil {
		prefix = "delete table inet incudal_gateway\n"
	}

	return nft(ctx, prefix+script, true)
}

func namespaceRules() string {
	return `table inet incudal_gateway {
 chain intercept { type filter hook prerouting priority -150; policy accept;
  ct direction reply return
  ip daddr 169.254.242.2 return
  ip6 daddr fd42:696e:6375::2 return
  iifname "igw-ns" meta l4proto { tcp, udp } tproxy to :15490 meta mark set 0x430000 accept
 }

 chain no_forward { type filter hook forward priority 0; policy drop; }
}`
}

func gatewayUnit(binaryPath, configPath string, failover bool) string {
	worker, _ := workerExecutable()
	override := fmt.Sprintf("ExecStartPost=%s -network-core-api %s", worker, coreAPIPath(filepath.Dir(configPath)))
	if failover {
		override += " -network-core-failover"
	}
	override += "\n"

	return fmt.Sprintf(`[Unit]
Description=Incudal Xray container egress
After=network-online.target
Wants=network-online.target
Before=incus.service incus-containers.service
[Service]
Type=simple
ExecStartPre=/usr/sbin/nft flush set inet incudal_egress ready
ExecStart=/usr/sbin/ip netns exec %s %s run -config %s
%s
ExecStopPost=-/usr/sbin/nft flush set inet incudal_egress ready
Restart=always
RestartSec=3
TimeoutStartSec=30
UMask=0077
NoNewPrivileges=true
ProtectHome=true
LimitNOFILE=65536
StandardOutput=null
StandardError=null
[Install]
WantedBy=multi-user.target
`, namespace, binaryPath, configPath, override)
}

func cleanGateway(ctx context.Context) error {
	// Remove classification first. Existing kernel connections retain their
	// ordinary route; no unrelated conntrack entries or firewall tables are reset.
	_ = run(ctx, "systemctl", "disable", "--now", "incudal-xray.service")
	for _, family := range []string{"-4", "-6"} {
		out, _ := networkCommand(ctx, "ip", family, "rule", "show").Output()
		if strings.Contains(string(out), "fwmark "+mark+"/"+mask+" lookup 7420") {
			{
				err := run(ctx, "ip", family, "rule", "del", "pref", "17420", "fwmark", mark+"/"+mask, "lookup", "7420")
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func removeRestoreUnit(ctx context.Context) error {
	_ = run(ctx, "systemctl", "disable", "--now", "incudal-network.service")
	for _, path := range []string{"/etc/systemd/system/incus.service.d/incudal-network.conf", "/etc/systemd/system/incudal-network.service"} {
		{
			err := os.Remove(path)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}

	return run(ctx, "systemctl", "daemon-reload")
}

func installRestoreUnit(ctx context.Context, exe string) error {
	// A copied worker survives Agent OTA. The last applied policy is restored
	// before Incus is allowed to start containers after a host reboot.
	data, err := os.ReadFile(exe)
	if err != nil {
		return err
	}

	worker := filepath.Join(stateDir, "network-worker")
	{
		err = atomicFile(worker, data, 0o700)
		if err != nil {
			return err
		}
	}

	unit := fmt.Sprintf("[Unit]\nDescription=Restore Incudal container firewall and egress\nAfter=network-online.target\nBefore=incus.service incus-containers.service incudal-xray.service\n[Service]\nType=oneshot\nExecStart=%s -network-restore\nRemainAfterExit=yes\n[Install]\nWantedBy=multi-user.target\n", worker)
	{
		err = atomicFile("/etc/systemd/system/incudal-network.service", []byte(unit), 0o644)
		if err != nil {
			return err
		}
	}
	{
		err = atomicFile("/etc/systemd/system/incus.service.d/incudal-network.conf", []byte("[Unit]\nRequires=incudal-network.service\nAfter=incudal-network.service\n"), 0o644)
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

	return run(ctx, "systemctl", "enable", "incudal-network.service")
}
