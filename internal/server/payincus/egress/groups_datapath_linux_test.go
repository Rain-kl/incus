package egress

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func configureDatapathProbeTLS(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	probeIP := netip.MustParseAddr("198.19.0.2")
	template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: probeIP.String()}, IPAddresses: []net.IP{net.IP(probeIP.AsSlice())}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificate, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certificatePath := filepath.Join(dir, "fixture.pem")
	keyPath := filepath.Join(dir, "fixture-key.pem")
	err = os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PACKAGE_TLS_CERT", certificatePath)
	t.Setenv("PACKAGE_TLS_KEY", keyPath)
	t.Setenv("SSL_CERT_FILE", certificatePath)
}

// Only run inside a disposable full-kernel VM with no external NIC. The VM
// fixture owns all namespaces, routes and firewall rules it creates.
func TestPackageEgressDatapath(t *testing.T) {
	if os.Getenv("INCUDAL_PACKAGE_DATAPATH_TEST") != "1" {
		t.Skip("isolated full-kernel VM required")
	}
	{
		_, err := os.Stat("/run/incudal-datapath-test-authorized")
		if err != nil || os.Geteuid() != 0 {
			t.Fatal("disposable VM sentinel/root missing")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	configureDatapathProbeTLS(t)
	command := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v %s", name, args, err, out)
		}

		return string(out)
	}

	for _, ns := range []string{"pkg-wan", "pkg-a", "pkg-b", "pkg-direct", "pkg-old", "pkg-unknown", "pkg-failover"} {
		command("ip", "netns", "add", ns)
		command("ip", "-n", ns, "link", "set", "lo", "up")
	}

	command("ip", "link", "add", "pkg-uplink", "type", "veth", "peer", "name", "eth0", "netns", "pkg-wan")
	command("ip", "addr", "add", "198.18.0.1/24", "dev", "pkg-uplink")
	command("ip", "link", "set", "pkg-uplink", "up")
	for _, ip := range []string{"198.18.0.2/24", "198.19.0.2/32", "198.19.0.3/32", "198.19.0.4/32", "198.19.0.5/32", "1.1.1.1/32"} {
		dev := "lo"
		if strings.HasPrefix(ip, "198.18") {
			dev = "eth0"
		}

		command("ip", "-n", "pkg-wan", "addr", "add", ip, "dev", dev)
	}

	command("ip", "-n", "pkg-wan", "link", "set", "eth0", "up")
	command("ip", "-n", "pkg-wan", "route", "add", "default", "via", "198.18.0.1")
	command("ip", "route", "add", "default", "via", "198.18.0.2")
	command("ip", "link", "add", "incusbr0", "type", "bridge")
	command("ip", "addr", "add", "10.10.0.1/24", "dev", "incusbr0")
	command("ip", "link", "set", "incusbr0", "up")
	for i, item := range []struct{ ns, nic string }{{"pkg-a", "ipgaaaaaaaaaaa"}, {"pkg-b", "ipgbbbbbbbbbbb"}, {"pkg-direct", "ipgccccccccccc"}, {"pkg-old", "old-veth"}, {"pkg-unknown", "ipgddddddddddd"}, {"pkg-failover", "ipg" + strings.Repeat("e", 11)}} {
		command("ip", "link", "add", item.nic, "type", "veth", "peer", "name", "eth0", "netns", item.ns)
		command("ip", "link", "set", item.nic, "master", "incusbr0")
		command("ip", "link", "set", item.nic, "up")
		command("ip", "-n", item.ns, "addr", "add", fmt.Sprintf("10.10.0.%d/24", i+2), "dev", "eth0")
		command("ip", "-n", item.ns, "link", "set", "eth0", "up")
		command("ip", "-n", item.ns, "route", "add", "default", "via", "10.10.0.1")
	}

	command("sysctl", "-qw", "net.ipv4.ip_forward=1", "net.ipv4.conf.all.rp_filter=0", "net.ipv6.conf.all.forwarding=1")
	command("ip", "-6", "addr", "add", "fd00:10::1/64", "dev", "incusbr0", "nodad")
	command("ip", "-n", "pkg-a", "-6", "addr", "add", "fd00:10::2/64", "dev", "eth0", "nodad")
	command("ip", "-n", "pkg-a", "-6", "route", "replace", "default", "via", "fd00:10::1")
	{
		err := ensureGroupOwnership(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}

	in := testGroups()
	failover := in.Groups[0]
	failover.Key = "p3"
	failover.Egress.Strategy = "failover"
	failover.Egress.ProbeURL = "https://198.19.0.2/"
	failover.Egress.ProxyNodeIDs = []int{1, 2}
	failover.Nodes = []Node{
		{ID: 1, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"198.18.0.2","port":1080}]}}`)},
		{ID: 2, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"198.18.0.2","port":1081}]}}`)},
	}
	in.Groups = append(in.Groups, failover)
	in.Bindings = append(in.Bindings, EgressBinding{InstanceName: "pkg-failover", Generation: 1, Mode: "profile", GroupKey: "p3", Nics: []EgressNIC{{HostName: "ipg" + strings.Repeat("e", 11), IPv4: "10.10.0.7", Bridge: "incusbr0"}}})
	runtime, err := allocateGroups(in.Groups, map[string]groupRuntime{})
	if err != nil {
		t.Fatal(err)
	}

	in.Groups[1].Nodes[0].Outbound = json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"198.18.0.2","port":1081}]}}`)
	for _, group := range in.Groups {
		{
			err := prepareGroupNamespace(ctx, runtime[group.Key], []string{"1.1.1.1"})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	// Repeated reconciliation must not accumulate routing rules. Group rules
	// must win over the legacy 17420 mask, which also matches group marks.
	{
		err := prepareGroupNamespace(ctx, runtime["p1"], []string{"1.1.1.1"})
		if err != nil {
			t.Fatal(err)
		}
	}

	hostRulesText := command("ip", "rule", "show")
	if strings.Count(hostRulesText, "fwmark "+runtime["p1"].mark()+"/"+groupMask+" lookup "+runtime["p1"].table()) != 1 {
		t.Fatalf("duplicate or missing group route rule: %s", hostRulesText)
	}
	{
		err := protectGroupPool(ctx, runtime)
		if err != nil {
			t.Fatal(err)
		}
	}

	install := func(ready map[string]bool) {
		t.Helper()
		bridge, inet, err := compileGroupRules(in, runtime, ready)
		if err != nil {
			t.Fatal(err)
		}
		{
			err := replaceGroupRules(ctx, bridge, inet)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	install(map[string]bool{"p1": true, "p2": true, "p3": true})
	legacy := testInstruction()
	legacy.SchemaVersion = 2
	legacy.Egress.Strategy = "failover"
	legacy.Egress.ProxyNodeIDs = []int{1}
	legacy.Nodes = legacy.Nodes[:1]
	legacy.Nodes[0].Outbound = json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"198.18.0.2","port":1082}]}}`)
	inv := inventory{Bridges: []string{"incusbr0"}, Internal: []string{"10.10.0.0/24"}}
	{
		err := prepareNamespace(ctx, "1.1.1.1")
		if err != nil {
			t.Fatal(err)
		}
	}

	rules, err := hostRules(legacy, inv)
	if err != nil {
		t.Fatal(err)
	}
	{
		err := replaceHostRules(ctx, rules)
		if err != nil {
			cmd := exec.CommandContext(ctx, "nft", "--check", "-f", "-")
			cmd.Stdin = strings.NewReader(rules)
			out, _ := cmd.CombinedOutput()
			t.Fatalf("%v: %s", err, out)
		}
	}
	{
		err := nft(ctx, `table ip pkg_nat {
 chain post { type nat hook postrouting priority 100; policy accept;
 ip saddr 10.10.0.0/24 ip daddr != 10.10.0.0/24 masquerade
 }

 chain pre { type nat hook prerouting priority -100; policy accept;
 iifname "pkg-uplink" tcp dport 8080 dnat to 10.10.0.2:8080
 }
}`, false)
		if err != nil {
			t.Fatal(err)
		}
	}

	exe, _ := os.Executable()
	core := os.Getenv("PACKAGE_EGRESS_XRAY")
	if core == "" {
		t.Fatal("pinned Xray binary required")
	}

	start := func(ns, executable string, args ...string) *exec.Cmd {
		t.Helper()
		cmd := exec.CommandContext(ctx, "ip", append([]string{"netns", "exec", ns, executable}, args...)...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		{
			err := cmd.Start()
			if err != nil {
				t.Fatal(err)
			}
		}

		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}

	start("pkg-wan", exe, "-test.run=^TestPackageEndpoint$")
	container := exec.CommandContext(ctx, "ip", "netns", "exec", "pkg-a", exe, "-test.run=^TestDatapathEndpoint$")
	container.Env = append(os.Environ(), "INCUDAL_DATAPATH_ENDPOINT=container")
	{
		err := container.Start()
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() { _ = container.Process.Kill(); _ = container.Wait() })
	v6Listener, err := net.Listen("tcp6", "[fd00:10::1]:80")
	if err != nil {
		t.Fatal(err)
	}

	v6Server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fixture:ipv6") })}
	go func() { _ = v6Server.Serve(v6Listener) }()
	t.Cleanup(func() { _ = v6Server.Close() })
	dir := t.TempDir()
	upstreamPath := filepath.Join(dir, "upstream.json")
	upstream := `{"log":{"loglevel":"none"},"inbounds":[{"tag":"a","listen":"198.18.0.2","port":1080,"protocol":"socks","settings":{"udp":true,"ip":"198.18.0.2"}},{"tag":"b","listen":"198.18.0.2","port":1081,"protocol":"socks","settings":{"udp":true,"ip":"198.18.0.2"}},{"tag":"c","listen":"198.18.0.2","port":1082,"protocol":"socks","settings":{"udp":true,"ip":"198.18.0.2"}}],"outbounds":[{"tag":"a","protocol":"freedom","sendThrough":"198.19.0.3"},{"tag":"b","protocol":"freedom","sendThrough":"198.19.0.4"},{"tag":"c","protocol":"freedom","sendThrough":"198.19.0.5"}],"routing":{"rules":[{"type":"field","inboundTag":["a"],"outboundTag":"a"},{"type":"field","inboundTag":["b"],"outboundTag":"b"},{"type":"field","inboundTag":["c"],"outboundTag":"c"}]}}`
	_ = os.WriteFile(upstreamPath, []byte(upstream), 0o600)
	start("pkg-wan", core, "run", "-config", upstreamPath)
	cores := map[string]*exec.Cmd{}
	for _, group := range in.Groups {
		{
			err := os.MkdirAll(runtime[group.Key].dir(), 0o700)
			if err != nil {
				t.Fatal(err)
			}
		}
		body, err := groupCoreConfig(group, runtime[group.Key])
		if err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(dir, group.Key+".json")
		_ = os.WriteFile(path, body, 0o600)
		cores[group.Key] = start(runtime[group.Key].ns(), core, "run", "-config", path)
	}

	{
		err := os.MkdirAll(stateDir, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}
	legacyBody, _ := xrayConfig(legacy, nil, true)
	legacyPath := filepath.Join(dir, "legacy.json")
	_ = os.WriteFile(legacyPath, legacyBody, 0o600)
	start(namespace, core, "run", "-config", legacyPath)
	time.Sleep(time.Second)
	{
		err := WaitCoreAPI(ctx, coreAPIPath(stateDir), true)
		if err != nil {
			t.Fatal(err)
		}
	}
	groupAPI := coreAPIPath(runtime["p3"].dir())
	err = WaitCoreAPI(ctx, groupAPI, true)
	if err != nil {
		t.Fatal("multi-node group Xray API did not become ready", err)
	}
	{
		err := overrideCore(ctx, coreAPIPath(stateDir), nodeTag(1))
		if err != nil {
			t.Fatal(err)
		}
	}
	command("nft", "add", "element", "inet", "incudal_egress", "ready", "{", mark, "}")

	for _, ns := range []string{"", runtime["p1"].ns()} {
		ns := ns
		args := []string{"nft", "-f", "-"}
		if ns != "" {
			args = append([]string{"ip", "netns", "exec", ns}, args...)
		}

		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = strings.NewReader("table inet pkg_trace { chain trace { type filter hook prerouting priority -400; policy accept; meta nftrace set 1\n}\n}\n")
		{
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("trace: %s %v", out, err)
			}
		}

		args = []string{"nft", "monitor", "trace"}
		if ns != "" {
			args = append([]string{"ip", "netns", "exec", ns}, args...)
		}

		monitor := exec.CommandContext(ctx, args[0], args[1:]...)
		var trace bytes.Buffer
		monitor.Stdout = &trace
		monitor.Stderr = &trace
		_ = monitor.Start()
		t.Cleanup(func() {
			_ = monitor.Process.Kill()
			_ = monitor.Wait()
			if t.Failed() {
				t.Logf("trace ns=%s: %s", ns, trace.String())
				for _, args := range [][]string{{"ip", "rule", "show"}, {"ip", "route", "show", "table", "all"}, {"ip", "-n", runtime["p1"].ns(), "rule", "show"}} {
					out, _ := exec.Command(args[0], args[1:]...).CombinedOutput()
					t.Logf("%s", out)
				}
			}
		})
	}

	client := func(ns, target, want string, success bool) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, exe, "-test.run=^TestPackageEndpoint$", "-test.v")
		cmd.Env = append(os.Environ(), "PACKAGE_CLIENT_TARGET="+target)
		out, err := cmd.CombinedOutput()
		if (err == nil) != success || (success && !strings.Contains(string(out), want)) {
			t.Fatalf("%s %s expected %s success=%v: %v %s", ns, target, want, success, err, out)
		}

		t.Logf("%s %s: success=%v %s", ns, target, success, out)
		return string(out)
	}

	failoverHealth := &groupHealthState{}
	group := in.Groups[2]
	if !groupHealthy(ctx, group, runtime["p3"], failoverHealth) || failoverHealth.active != 1 {
		t.Fatalf("two-node failover group did not select the healthy first proxy: %#v", failoverHealth)
	}
	client("pkg-failover", "http://198.19.0.2/", "198.19.0.3", true)
	client("pkg-failover", "dns:10.10.0.1:53", "198.19.0.3", true)
	failureRule := `table inet fixture_proxy_failure {
 chain forward { type filter hook forward priority -400; policy accept;
  ip daddr 198.18.0.2 tcp dport 1080 drop
 }
}`
	err = nft(ctx, failureRule, false)
	if err != nil {
		t.Fatal("could not isolate the primary proxy failure", err)
	}
	if !groupHealthy(ctx, group, runtime["p3"], failoverHealth) || failoverHealth.active != 2 {
		t.Fatalf("failed primary did not switch the group to its backup: %#v", failoverHealth)
	}
	client("pkg-failover", "http://198.19.0.2/", "198.19.0.4", true)
	client("pkg-failover", "dns:10.10.0.1:53", "198.19.0.4", true)
	err = nft(ctx, "delete table inet fixture_proxy_failure", false)
	if err != nil {
		t.Fatal("could not restore the primary proxy fixture", err)
	}
	// Exercise native Xray selection separately from failover's explicit API override.
	restartGroupCore := func(strategy string) {
		t.Helper()
		err = cores["p3"].Process.Kill()
		if err != nil {
			t.Fatal("could not stop previous isolated group core", err)
		}
		err = cores["p3"].Wait()
		if err != nil && !strings.Contains(err.Error(), "killed") {
			t.Fatal("could not reap previous isolated group core", err)
		}
		group.Egress.Strategy = strategy
		if strategy == "leastPing" {
			group.Egress.ProbeIntervalSeconds = 10
		}
		config, configErr := groupCoreConfig(group, runtime["p3"])
		if configErr != nil {
			t.Fatal("could not generate native selection config", configErr)
		}
		path := filepath.Join(dir, "p3-"+strategy+".json")
		err = os.WriteFile(path, config, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(coreAPIPath(runtime["p3"].dir()))
		cores["p3"] = start(runtime["p3"].ns(), core, "run", "-config", path)
		err = WaitCoreAPI(ctx, groupAPI, false)
		if err != nil {
			t.Fatal("native strategy Xray API did not become ready", strategy, err)
		}
	}
	restartGroupCore("balance")
	if !groupHealthy(ctx, group, runtime["p3"], &groupHealthState{}) {
		t.Fatal("balanced group failed HTTPS and DNS readiness checks")
	}
	balanceExits := map[string]bool{}
	for i := 0; i < 8; i++ {
		output := client("pkg-failover", "http://198.19.0.2/", "", true)
		if strings.Contains(output, "198.19.0.3") {
			balanceExits["198.19.0.3"] = true
		}
		if strings.Contains(output, "198.19.0.4") {
			balanceExits["198.19.0.4"] = true
		}
	}
	if len(balanceExits) != 2 {
		t.Fatalf("native Xray balance did not use both exits over fresh connections: %#v", balanceExits)
	}
	restartGroupCore("leastPing")
	if !groupHealthy(ctx, group, runtime["p3"], &groupHealthState{}) {
		t.Fatal("leastPing group failed HTTPS and DNS readiness checks")
	}
	initial := client("pkg-failover", "http://198.19.0.2/", "", true)
	if strings.Contains(initial, "198.18.0.1") || (!strings.Contains(initial, "198.19.0.3") && !strings.Contains(initial, "198.19.0.4")) {
		t.Fatalf("leastPing did not use a healthy proxy exit: %s", initial)
	}
	initialDNS := client("pkg-failover", "dns:10.10.0.1:53", "", true)
	if strings.Contains(initialDNS, "198.18.0.1") || (!strings.Contains(initialDNS, "198.19.0.3") && !strings.Contains(initialDNS, "198.19.0.4")) {
		t.Fatalf("leastPing DNS did not use a healthy proxy exit: %s", initialDNS)
	}
	failureRule = `table inet fixture_proxy_failure {
 chain forward { type filter hook forward priority -400; policy accept;
  ip daddr 198.18.0.2 tcp dport 1080 drop
 }
}`
	err = nft(ctx, failureRule, false)
	if err != nil {
		t.Fatal("could not isolate the primary proxy failure for leastPing", err)
	}
	time.Sleep(12 * time.Second)
	backup := client("pkg-failover", "http://198.19.0.2/", "198.19.0.4", true)
	if strings.Contains(backup, "198.18.0.1") {
		t.Fatalf("leastPing failed open to host direct after primary failure: %s", backup)
	}
	backupDNS := client("pkg-failover", "dns:10.10.0.1:53", "198.19.0.4", true)
	if strings.Contains(backupDNS, "198.18.0.1") {
		t.Fatalf("leastPing DNS failed open to host direct after primary failure: %s", backupDNS)
	}
	err = nft(ctx, "delete table inet fixture_proxy_failure", false)
	if err != nil {
		t.Fatal("could not restore primary proxy after native strategy checks", err)
	}

	for _, item := range []struct{ ns, want string }{{"pkg-a", "198.19.0.3"}, {"pkg-b", "198.19.0.4"}, {"pkg-direct", "198.18.0.1"}, {"pkg-old", "198.19.0.5"}} {
		client(item.ns, "http://198.19.0.2/", item.want, true)
		if item.ns != "pkg-old" {
			client(item.ns, "udp:198.19.0.2:8888", item.want, true)
		}

		client(item.ns, "http://198.19.0.2:25/", "", false)
	}

	client("pkg-a", "dns:10.10.0.1:53", "198.19.0.3", true)
	client("pkg-b", "dns:1.1.1.1:53", "198.19.0.4", true)
	client("pkg-a", "tcpdns:8.8.8.8:53", "198.19.0.3", true)
	client("pkg-direct", "dns:10.10.0.1:53", "198.18.0.1", true)
	client("pkg-wan", "http://198.18.0.1:8080/", "fixture:container", true)
	client("pkg-unknown", "http://198.19.0.2/", "", false)
	client("pkg-a", "http://[fd00:10::1]:80/", "", false)
	for _, addr := range []string{"10.10.0.99/24", "10.10.0.2/24"} {
		command("ip", "-n", "pkg-a", "addr", "flush", "dev", "eth0")
		command("ip", "-n", "pkg-a", "addr", "add", addr, "dev", "eth0")
		command("ip", "-n", "pkg-a", "route", "replace", "default", "via", "10.10.0.1")
		if strings.Contains(addr, ".99/") {
			client("pkg-a", "http://198.19.0.2/", "", false)
		}
	}

	_ = cores["p1"].Process.Kill()
	_ = cores["p1"].Wait()
	command("nft", "delete", "element", "inet", "incudal_package_egress", "ready", "{", runtime["p1"].mark(), "}")
	client("pkg-a", "http://198.19.0.2/", "", false)
	client("pkg-b", "http://198.19.0.2/", "198.19.0.4", true)
	client("pkg-direct", "http://198.19.0.2/", "198.18.0.1", true)
	client("pkg-old", "http://198.19.0.2/", "198.19.0.5", true)
	// Missing gateway route must not fall back to the host's native default.
	command("ip", "route", "del", "default", "via", runtime["p1"].ip(), "dev", runtime["p1"].iface(), "table", runtime["p1"].table())
	install(map[string]bool{"p1": true, "p2": true})
	client("pkg-a", "http://198.19.0.2/", "", false)
	client("pkg-b", "http://198.19.0.2/", "198.19.0.4", true)
	// Boot restoration installs bindings with no ready groups; native direct remains usable.
	install(map[string]bool{})
	client("pkg-b", "http://198.19.0.2/", "", false)
	client("pkg-direct", "http://198.19.0.2/", "198.18.0.1", true)
	{
		err := nft(ctx, groupBootRules(), false)
		if err != nil {
			t.Fatal(err)
		}
	}

	client("pkg-direct", "http://198.19.0.2/", "", false)
	install(map[string]bool{"p2": true})
	client("pkg-b", "http://198.19.0.2/", "198.19.0.4", true)
}

func TestPackageEndpoint(t *testing.T) {
	if os.Getenv("INCUDAL_PACKAGE_DATAPATH_TEST") != "1" {
		t.Skip("isolated VM endpoint")
	}
	{
		_, err := os.Stat("/run/incudal-datapath-test-authorized")
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		target := os.Getenv("PACKAGE_CLIENT_TARGET")
		if target != "" {
			if strings.HasPrefix(target, "udp:") {
				conn, err := net.DialTimeout("udp", strings.TrimPrefix(target, "udp:"), time.Second)
				if err != nil {
					t.Fatal(err)
				}

				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				_, _ = conn.Write([]byte("fixture"))
				buf := make([]byte, 128)
				n, err := conn.Read(buf)
				if err != nil {
					t.Fatal(err)
				}

				t.Logf("UDP %s", buf[:n])
				return
			}

			if strings.HasPrefix(target, "dns:") || strings.HasPrefix(target, "tcpdns:") {
				network := "udp"
				address := strings.TrimPrefix(target, "dns:")
				if strings.HasPrefix(target, "tcpdns:") {
					network = "tcp"
					address = strings.TrimPrefix(target, "tcpdns:")
				}

				r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, address)
				}}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				txt, err := r.LookupTXT(ctx, "fixture.example")
				if err != nil || len(txt) == 0 {
					t.Fatalf("DNS %v %v", txt, err)
				}

				t.Logf("DNS %v", txt)
				return
			}

			c := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 3 * time.Second}
			response, err := c.Get(target)
			if err != nil {
				t.Fatal(err)
			}

			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}

			t.Logf("%s", body)
			return
		}
	}

	answer := func(q []byte, source string) []byte {
		if len(q) < 17 {
			return nil
		}

		end := 12
		for end < len(q) && q[end] != 0 {
			end += int(q[end]) + 1
		}

		end += 5
		if end > len(q) {
			return nil
		}

		out := append([]byte{}, q[:end]...)
		out[2], out[3] = 0x81, 0x80
		out[6], out[7], out[10], out[11] = 0, 1, 0, 0
		ip, _, _ := net.SplitHostPort(source)
		if binary.BigEndian.Uint16(q[end-4:end-2]) == 16 {
			value := []byte(ip)
			out = append(out, 0xc0, 0x0c, 0, 16, 0, 1, 0, 0, 0, 30, 0, byte(len(value)+1), byte(len(value)))
			out = append(out, value...)
		} else {
			out[7] = 0
		}

		return out
	}

	echo, err := net.ListenPacket("udp4", "198.19.0.2:8888")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = echo.Close() }()
	go func() {
		buf := make([]byte, 128)
		for {
			_, addr, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}

			_, _ = echo.WriteTo([]byte("fixture:wan:"+addr.String()), addr)
		}
	}()
	pc, err := net.ListenPacket("udp4", ":53")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}

			_, _ = pc.WriteTo(answer(buf[:n], addr.String()), addr)
		}
	}()
	listener, err := net.Listen("tcp4", ":53")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()
				for {
					var size [2]byte
					{
						_, err := io.ReadFull(conn, size[:])
						if err != nil {
							return
						}
					}

					q := make([]byte, binary.BigEndian.Uint16(size[:]))
					{
						_, err := io.ReadFull(conn, q)
						if err != nil {
							return
						}
					}

					a := answer(q, conn.RemoteAddr().String())
					binary.BigEndian.PutUint16(size[:], uint16(len(a)))
					_, _ = conn.Write(append(size[:], a...))
				}
			}()
		}
	}()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprintf(w, "fixture:wan:%s", r.RemoteAddr) })
	certificate, err := tls.LoadX509KeyPair(os.Getenv("PACKAGE_TLS_CERT"), os.Getenv("PACKAGE_TLS_KEY"))
	if err != nil {
		t.Fatal("fixture TLS identity unavailable", err)
	}
	tlsListener, err := net.Listen("tcp4", ":443")
	if err != nil {
		t.Fatal(err)
	}
	tlsServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fixture:tls") })}
	go func() {
		_ = tlsServer.Serve(tls.NewListener(tlsListener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}))
	}()
	go func() { _ = http.ListenAndServe(":25", handler) }()
	t.Fatal(http.ListenAndServe(":80", handler))
}
