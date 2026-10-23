package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This fixture changes network configuration. Run only in the disposable VM
// with no external NIC, never on an application host or in an ordinary go test.
func TestIsolatedDatapath(t *testing.T) {
	if os.Getenv("INCUDAL_DATAPATH_TEST") != "1" {
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
	command := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}

		return string(out)
	}

	for _, ns := range []string{"fixture-wan", "fixture-container"} {
		command("ip", "netns", "add", ns)
		command("ip", "-n", ns, "link", "set", "lo", "up")
		{
			err := atomicFile("/etc/netns/"+ns+"/resolv.conf", []byte("nameserver 1.1.1.1\n"), 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	command("ip", "link", "add", "fixture-uplink", "type", "veth", "peer", "name", "eth0", "netns", "fixture-wan")
	command("ip", "addr", "add", "198.18.0.1/24", "dev", "fixture-uplink")
	command("ip", "link", "set", "fixture-uplink", "up")
	command("ip", "-n", "fixture-wan", "addr", "add", "198.18.0.2/24", "dev", "eth0")
	command("ip", "-n", "fixture-wan", "addr", "add", "198.19.0.2/32", "dev", "lo")
	command("ip", "-n", "fixture-wan", "addr", "add", "198.19.0.3/32", "dev", "lo")
	command("ip", "-n", "fixture-wan", "addr", "add", "198.19.0.4/32", "dev", "lo")
	command("ip", "-n", "fixture-wan", "addr", "add", "1.1.1.1/32", "dev", "lo")
	command("ip", "-n", "fixture-wan", "addr", "add", "203.0.113.53/32", "dev", "lo")
	command("ip", "-n", "fixture-wan", "link", "set", "eth0", "up")
	command("ip", "-n", "fixture-wan", "route", "add", "default", "via", "198.18.0.1")
	command("ip", "route", "add", "default", "via", "198.18.0.2")
	command("ip", "link", "add", "incusbr0", "type", "bridge")
	command("ip", "addr", "add", "10.10.0.1/22", "dev", "incusbr0")
	command("ip", "link", "set", "incusbr0", "up")
	command("ip", "link", "add", "fixture-veth", "type", "veth", "peer", "name", "eth0", "netns", "fixture-container")
	command("ip", "link", "set", "fixture-veth", "master", "incusbr0")
	command("ip", "link", "set", "fixture-veth", "up")
	command("ip", "-n", "fixture-container", "addr", "add", "10.10.2.103/22", "dev", "eth0")
	command("ip", "-n", "fixture-container", "link", "set", "eth0", "up")
	command("ip", "-n", "fixture-container", "route", "add", "default", "via", "10.10.0.1")
	command("sysctl", "-qw", "net.ipv4.ip_forward=1", "net.ipv4.conf.all.rp_filter=0")
	in := testInstruction()
	in.Egress.Mode, in.Nodes = "direct", nil
	inv := inventory{Bridges: []string{"incusbr0"}, Internal: []string{"10.10.0.0/22"}}
	{
		err := checkTransparentProxy(ctx)
		if err != nil {
			cmd := exec.CommandContext(ctx, "nft", "--check", "-f", "-")
			cmd.Stdin = strings.NewReader(namespaceRules())
			out, _ := cmd.CombinedOutput()
			t.Fatalf("%v: %s", err, out)
		}
	}
	{
		err := prepareNamespace(ctx, "1.1.1.1")
		if err != nil {
			t.Fatal(err)
		}
	}

	rules, err := hostRules(in, inv)
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
	// Model Incus bridge NAT, plus one public DNAT port. Targets are entirely
	// local VM namespaces; the VM has no Internet-facing network device.
	{
		err := nft(ctx, `table ip fixture_nat {
 chain post { type nat hook postrouting priority 100; policy accept;
 ip saddr 10.10.0.0/22 ip daddr != 10.10.0.0/22 masquerade
 }

 chain pre { type nat hook prerouting priority -100; policy accept;
 iifname "fixture-uplink" tcp dport 8080 dnat to 10.10.2.103:8080
 }
}`, false)
		if err != nil {
			t.Fatal(err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	start := func(ns, role string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, exe, "-test.run=^TestDatapathEndpoint$", "-test.v")
		cmd.Env = append(os.Environ(), "INCUDAL_DATAPATH_ENDPOINT="+role)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		{
			err := cmd.Start()
			if err != nil {
				t.Fatal(err)
			}
		}

		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	}

	start("fixture-wan", "wan")
	start("fixture-container", "container")
	localListener, err := net.Listen("tcp4", "10.10.0.1:8081")
	if err != nil {
		t.Fatal(err)
	}

	localServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fixture:internal") }))
	_ = localServer.Listener.Close()
	localServer.Listener = localListener
	localServer.Start()
	t.Cleanup(localServer.Close)
	dir := t.TempDir()
	core, err := extractBinary(dir)
	if err != nil {
		t.Fatal(err)
	}

	config, err := xrayConfig(in, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "xray.json")
	{
		err := os.WriteFile(configPath, config, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cmd := networkCommand(ctx, "ip", "netns", "exec", namespace, core, "run", "-config", configPath)
	{
		err := cmd.Start()
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	time.Sleep(time.Second)
	command("nft", "add", "element", "inet", "incudal_egress", "ready", "{", mark, "}")
	defer func() {
		if t.Failed() {
			for _, args := range [][]string{{"list", "ruleset"}} {
				out, _ := exec.Command("nft", args...).CombinedOutput()
				t.Logf("host nft: %s", out)
			}

			out, _ := exec.Command("ip", "netns", "exec", namespace, "nft", "list", "ruleset").CombinedOutput()
			t.Logf("namespace nft: %s", out)
		}
	}()
	client := func(ns, target string, wantSuccess bool) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, exe, "-test.run=^TestDatapathEndpoint$", "-test.v")
		cmd.Env = append(os.Environ(), "INCUDAL_DATAPATH_ENDPOINT=client", "INCUDAL_DATAPATH_TARGET="+target)
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("namespace %s target %s success=%v: %v\n%s", ns, target, wantSuccess, err, out)
		}

		t.Logf("namespace %s target %s expectedSuccess=%v: %s", ns, target, wantSuccess, out)
		return string(out)
	}
	// Reproduce the real host: namespace all=1 overrides interface=0. A source
	// lookup with mark430000 hits the local catch-all and rejects incoming SYNs.
	hostSourceValidation := command("sysctl", "-n", "net.ipv4.conf.all.src_valid_mark")
	command("ip", "netns", "exec", namespace, "sysctl", "-qw", "net.ipv4.conf.all.src_valid_mark=1", "net.ipv4.conf.igw-ns.src_valid_mark=0")
	{
		err := exec.CommandContext(ctx, "ip", "netns", "exec", namespace, "ip", "-4", "route", "get", "198.19.0.2", "from", "169.254.242.1", "iif", "igw-ns", "mark", "0x430000").Run() // codespell:ignore iif
		if err == nil {
			t.Fatal("fixture did not reproduce marked source-validation failure")
		}
	}

	client("fixture-container", "http://198.19.0.2/", false)
	{
		err := prepareNamespace(ctx, "1.1.1.1")
		if err != nil {
			t.Fatal(err)
		}
	}

	command("ip", "netns", "exec", namespace, "ip", "-4", "route", "get", "198.19.0.2", "from", "169.254.242.1", "iif", "igw-ns", "mark", "0x430000") // codespell:ignore iif
	if command("sysctl", "-n", "net.ipv4.conf.all.src_valid_mark") != hostSourceValidation {
		t.Fatal("gateway preparation changed host source validation")
	}

	client("fixture-container", "http://198.19.0.2/", true)
	client("fixture-container", "https://198.19.0.2/", true)
	client("fixture-container", "dns:1.1.1.1:53", true)
	client(namespace, "dns:169.254.242.2:53", true)
	client("fixture-container", "http://10.10.0.1:8081/", true)
	client("fixture-wan", "http://198.18.0.1:8080/", true)
	client("fixture-wan", "http://198.19.0.2:25/", true)
	client("fixture-container", "http://198.19.0.2:25/", false)
	upstreamConfig := filepath.Join(dir, "upstream.json")
	{
		err := os.WriteFile(upstreamConfig, []byte(`{"log":{"loglevel":"none"},"inbounds":[{"tag":"first","listen":"198.19.0.2","port":1080,"protocol":"socks","settings":{"auth":"noauth","udp":true,"ip":"198.19.0.2"}},{"tag":"second","listen":"198.19.0.2","port":1081,"protocol":"socks","settings":{"auth":"noauth","udp":true,"ip":"198.19.0.2"}}],"outbounds":[{"tag":"first","protocol":"freedom","sendThrough":"198.19.0.3"},{"tag":"second","protocol":"freedom","sendThrough":"198.19.0.4"}],"routing":{"rules":[{"type":"field","inboundTag":["first"],"outboundTag":"first"},{"type":"field","inboundTag":["second"],"outboundTag":"second"}]}}`), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	upstream := exec.CommandContext(ctx, "ip", "netns", "exec", "fixture-wan", core, "run", "-config", upstreamConfig)
	{
		err := upstream.Start()
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() { _ = upstream.Process.Kill(); _ = upstream.Wait() })
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	proxyInstruction := testInstruction()
	proxyInstruction.Egress.ProbeURL = "https://example.com/"
	proxyInstruction.Egress.ProbeIntervalSeconds = 10
	proxyInstruction.Nodes[0].Outbound = json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"proxy.fixture.example","port":1080}]}}`)
	proxyInstruction.Nodes[1].Outbound = json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"proxy.fixture.example","port":1081}]}}`)
	// Reproduce the fault: general egress works, but the fixed bootstrap resolver
	// is unavailable from the host network. Remote proxy DNS remains available.
	rejectDNS := exec.CommandContext(ctx, "ip", "netns", "exec", "fixture-wan", "nft", "-f", "-")
	rejectDNS.Stdin = strings.NewReader(`table inet fixture_dns {
 chain input { type filter hook input priority 0; policy accept;
 iifname "eth0" ip daddr 1.1.1.1 udp dport 53 reject
 iifname "eth0" ip daddr 1.1.1.1 tcp dport 53 reject
 }
}`)
	{
		out, err := rejectDNS.CombinedOutput()
		if err != nil {
			t.Fatalf("DNS fault injection failed: %s %v", out, err)
		}
	}

	oldBody, _ := xrayConfig(proxyInstruction, nil, true)
	{
		err := os.WriteFile(configPath, oldBody, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	broken := networkCommand(ctx, "ip", "netns", "exec", namespace, core, "run", "-config", configPath)
	{
		err := broken.Start()
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() { _ = broken.Process.Kill(); _ = broken.Wait() })
	time.Sleep(time.Second)
	command("ip", "netns", "exec", namespace, core, "api", "bo", "--server=127.0.0.1:15491", "-b", "egress", nodeTag(1))
	client("fixture-container", "https://198.19.0.2/", false)
	_ = broken.Process.Kill()
	_ = broken.Wait()
	resolvers, err := readBootstrapResolvers(func(path string) ([]byte, error) {
		if path == "/etc/resolv.conf" {
			return []byte("nameserver 127.0.0.53\n"), nil
		}

		if path == "/run/systemd/resolve/resolv.conf" {
			return []byte("nameserver 8.8.8.8\nnameserver 203.0.113.53\n"), nil
		}

		return nil, os.ErrNotExist
	}, inv)
	if err != nil {
		t.Fatal(err)
	}
	{
		err := atomicFile(namespaceResolverPath, namespaceResolverConfig(resolvers), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, strategy := range []string{"failover", "balance", "leastPing"} {
		proxyInstruction.Egress.Strategy = strategy
		body, err := xrayConfig(proxyInstruction, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		{
			err := os.WriteFile(configPath, body, 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}

		gateway := networkCommand(ctx, "ip", "netns", "exec", namespace, core, "run", "-config", configPath)
		gateway.Env = append(os.Environ(), "SSL_CERT_FILE=/tmp/fixture-ca.pem")
		{
			err := gateway.Start()
			if err != nil {
				t.Fatal(err)
			}
		}

		t.Cleanup(func() { _ = gateway.Process.Kill(); _ = gateway.Wait() })
		time.Sleep(3 * time.Second)
		if strategy == "failover" {
			for index, origin := range []string{"198.19.0.3", "198.19.0.4"} {
				command("ip", "netns", "exec", namespace, core, "api", "bo", "--server=127.0.0.1:15491", "-b", "egress", nodeTag(proxyInstruction.Nodes[index].ID))
				out := client("fixture-container", "https://198.19.0.2/", true)
				if !strings.Contains(out, origin) {
					t.Fatalf("failover selected wrong outbound: %s", out)
				}
			}

			command("ip", "netns", "exec", namespace, core, "api", "bo", "--server=127.0.0.1:15491", "-b", "egress", "blocked")
			client("fixture-container", "https://198.19.0.2/", false)
		} else {
			seen := map[string]bool{}
			for request := 0; request < 4; request++ {
				out := client("fixture-container", "https://198.19.0.2/", true)
				for _, origin := range []string{"198.19.0.3", "198.19.0.4"} {
					if strings.Contains(out, origin) {
						seen[origin] = true
					}
				}

				if strings.Contains(out, "fixture:wan:198.18.0.1:") {
					t.Fatal("proxy mode leaked to direct outbound")
				}
			}

			if strategy == "balance" && len(seen) != 2 {
				t.Fatal("round-robin did not use both healthy proxies")
			}

			client("fixture-container", "dns:1.1.1.1:53", true)
		}

		_ = gateway.Process.Kill()
		_ = gateway.Wait()
		t.Logf("real transparent datapath passed strategy=%s", strategy)
	}
	// Stop interception readiness/core, while keeping isolation rules installed.
	command("nft", "flush", "set", "inet", "incudal_egress", "ready")
	_ = cmd.Process.Kill()
	client("fixture-container", "http://198.19.0.2/", false)
	client("fixture-container", "http://10.10.0.1:8081/", true)
	client("fixture-wan", "http://198.18.0.1:8080/", true)
	// Disabling interception restores ordinary container egress and retains ACLs.
	in.Egress.Mode = "off"
	offRules, err := hostRules(in, inv)
	if err != nil {
		t.Fatal(err)
	}
	{
		err := replaceHostRules(ctx, offRules)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		err := cleanGateway(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}

	client("fixture-container", "https://198.19.0.2/", true)
	client("fixture-container", "http://198.19.0.2:25/", false)
}

func TestDatapathEndpoint(t *testing.T) {
	role := os.Getenv("INCUDAL_DATAPATH_ENDPOINT")
	if role == "" {
		t.Skip("isolated VM endpoint helper")
	}
	{
		_, err := os.Stat("/run/incudal-datapath-test-authorized")
		if err != nil {
			t.Fatal(err)
		}
	}

	if role == "client" {
		target := os.Getenv("INCUDAL_DATAPATH_TARGET")
		if strings.HasPrefix(target, "dns:") {
			r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(target, "dns:"))
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ips, err := r.LookupHost(ctx, "fixture.example")
			if err != nil || len(ips) == 0 || ips[0] != "198.19.0.2" {
				t.Fatalf("DNS: %v %v", ips, err)
			}

			return
		}

		transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
		if strings.HasPrefix(target, "https:") {
			cert, err := os.ReadFile("/tmp/fixture-ca.pem")
			if err != nil {
				t.Fatal(err)
			}

			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(cert) {
				t.Fatal("fixture certificate unavailable")
			}

			transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "example.com"}
		}

		c := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		resp, err := c.Get(target)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || !strings.HasPrefix(string(body), "fixture:") {
			t.Fatalf("unexpected HTTP response: %q %v", body, err)
		}

		t.Logf("received %s", body)
		return
	}

	if role == "wan" {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprintf(w, "fixture:wan:%s", r.RemoteAddr) })
		tlsListener, err := net.Listen("tcp4", "198.19.0.2:443")
		if err != nil {
			t.Fatal(err)
		}

		tlsServer := httptest.NewUnstartedServer(handler)
		_ = tlsServer.Listener.Close()
		tlsServer.Listener = tlsListener
		tlsServer.StartTLS()
		defer tlsServer.Close()
		cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw})
		{
			err := os.WriteFile("/tmp/fixture-ca.pem", cert, 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}

		go func() { _ = http.ListenAndServe(":25", handler) }()
		for _, address := range []string{"1.1.1.1:53", "203.0.113.53:53"} {
			pc, err := net.ListenPacket("udp4", address)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = pc.Close() })
			go func() {
				buf := make([]byte, 4096)
				for {
					n, addr, err := pc.ReadFrom(buf)
					if err != nil {
						return
					}

					if n < 17 {
						continue
					}

					end := 12
					for end < n && buf[end] != 0 {
						end += int(buf[end]) + 1
					}

					end += 5
					if end > n {
						continue
					}

					response := append([]byte{}, buf[:end]...)
					response[2], response[3] = 0x81, 0x80
					response[10], response[11] = 0, 0
					if binary.BigEndian.Uint16(response[end-4:end-2]) == 1 {
						response[7] = 1
						response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 198, 19, 0, 2)
					}

					_, _ = pc.WriteTo(response, addr)
				}
			}()
		}
	}

	address := ":80"
	if role == "container" {
		address = ":8080"
	}

	t.Fatal(http.ListenAndServe(address, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "fixture:%s:%s", role, r.RemoteAddr)
	})))
}
