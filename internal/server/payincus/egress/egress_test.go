package egress

import (
	"bytes"
	"context"
	"crypto/x509"
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
	"sync/atomic"
	"testing"
	"time"
)

func testInstruction() Instruction {
	return Instruction{Revision: 1, Egress: Policy{Mode: "proxy", Strategy: "balance", ProxyNodeIDs: []int{1, 10}, ProbeURL: "https://www.gstatic.com/generate_204", ProbeIntervalSeconds: 30}, Firewall: Firewall{Enabled: true, IngressAction: "allow", EgressAction: "allow", Rules: []Rule{{Direction: "egress", Action: "drop", Protocol: "tcp", DestinationPort: "25", Enabled: true}}}, Nodes: []Node{{ID: 1, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`)}, {ID: 10, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1081}]}}`)}}}
}

func TestRealtimeProbeHasNoPersistentResults(t *testing.T) {
	dir := t.TempDir()
	m := newManager(dir)
	result, err := m.Probe(context.Background(), ProbeRequest{ProbeURL: "https://example.com", Nodes: []Node{{ID: 7, Outbound: json.RawMessage(`invalid`)}}})
	if err != nil || len(result) != 1 || result[0].NodeID != 7 || result[0].Success || result[0].Error != "xray" {
		t.Fatalf("unexpected immediate result: %+v %v", result, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "xray-") && !strings.HasPrefix(entry.Name(), "XRAY-") {
			t.Fatalf("probe left persistent data: %s", entry.Name())
		}
	}
}

func TestXrayConfiguration(t *testing.T) {
	in := testInstruction()
	data, err := xrayConfig(in, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	var c map[string]any
	_ = json.Unmarshal(data, &c)
	outbounds := mustValue[[]any](t, c["outbounds"])
	if mustValue[map[string]any](t, outbounds[0])["protocol"] != "blackhole" {
		t.Fatal("default outbound must block")
	}

	b := mustValue[map[string]any](t, mustValue[[]any](t, mustValue[map[string]any](t, c["routing"])["balancers"])[0])
	if b["fallbackTag"] != "blocked" || mustValue[map[string]any](t, b["strategy"])["type"] != "roundRobin" {
		t.Fatal("native healthy-node balancing and fail-closed fallback required")
	}

	if strings.HasPrefix(nodeTag(10), nodeTag(1)) {
		t.Fatal("Xray selector prefix collision")
	}

	if !Available() {
		t.Skip("source-only build; prepare pinned Xray to run core validation")
	}

	dir := t.TempDir()
	binary, err := extractBinary(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"direct", "proxy"} {
		for _, strategy := range []string{"balance", "failover", "leastPing"} {
			in.Egress.Mode = mode
			in.Egress.Strategy = strategy
			body, e := xrayConfig(in, nil, true)
			if e != nil {
				t.Fatal(e)
			}

			path := filepath.Join(dir, "config.json")
			_ = os.WriteFile(path, body, 0o600)
			{
				output, e := exec.Command(binary, "run", "-test", "-config", path).CombinedOutput()
				if e != nil {
					t.Fatalf("core rejected %s/%s: %s", mode, strategy, output)
				}
			}
		}
	}
	// Production validates an atomic staging file ending in .json.next. Xray
	// cannot infer that extension; exercise the exact validator and zero-node
	// direct mode instead of relying only on ordinary .json fixtures.
	in.Egress.Mode = "direct"
	in.Nodes = nil
	body, e := xrayConfig(in, nil, true)
	if e != nil {
		t.Fatal(e)
	}

	path := filepath.Join(dir, "xray.json.next")
	{
		e = os.WriteFile(path, body, 0o600)
		if e != nil {
			t.Fatal(e)
		}
	}
	{
		e = validateCore(context.Background(), binary, path)
		if e != nil {
			t.Fatalf("production staging path rejected zero-node direct configuration: %v", e)
		}
	}
}

func TestImportedXrayConfiguration(t *testing.T) {
	fixture := os.Getenv("EGRESS_OUTBOUND_FIXTURE")
	if fixture == "" || !Available() {
		t.Skip("optional private import fixture and embedded Xray required")
	}

	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}

	in := testInstruction()
	{
		err = json.Unmarshal(body, &in.Nodes)
		if err != nil {
			t.Fatal("invalid private import fixture")
		}
	}

	dir := t.TempDir()
	binary, err := extractBinary(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, strategy := range []string{"balance", "failover", "leastPing"} {
		in.Egress.Strategy = strategy
		body, err = xrayConfig(in, nil, true)
		if err != nil {
			t.Fatal("cannot generate imported configuration")
		}

		path := filepath.Join(dir, "imported.json")
		{
			err = os.WriteFile(path, body, 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
		{
			err = validateCore(context.Background(), binary, path)
			if err != nil {
				t.Fatalf("imported configuration rejected (%s): %v", strategy, err)
			}
		}
	}
}

func TestCoreValidationRedactsCredentials(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rejected-core")
	{
		err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'invalid \"password\": private-fixture-credential' >&2\nexit 1\n"), 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := validateCore(context.Background(), binary, "config.json")
	if err == nil || err.Error() != "reality_key_invalid" {
		t.Fatalf("core diagnostic must expose only its category: %v", err)
	}
}

func TestConnectionAndACLBoundaries(t *testing.T) {
	in := testInstruction()
	rules, err := hostRules(in, inventory{Bridges: []string{"incusbr0"}, Routed: []string{"veth123"}, Internal: []string{"10.10.0.0/16"}, RoutedSources: []string{"2001:db8:10::2/128"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, required := range []string{"ct direction reply return", "ct status dnat return", "ct state != new return", "ct state { invalid, untracked } drop", "10.10.0.0/16", "0xff00ffff", "@ready drop", "oifname != \"igw-host\" drop", "priority -5", "priority 10", "iceg*", "tcp dport { 25 } drop"} {
		if !strings.Contains(rules, required) {
			t.Fatalf("missing protection %q", required)
		}
	}

	if strings.Index(rules, "ct direction reply return") > strings.Index(rules, "ct mark set") {
		t.Fatal("inbound reply bypass must precede interception")
	}

	if !strings.Contains(rules, "ip6 saddr { 2001:db8:10::2/128 } jump selected") || !strings.Contains(rules, "chain acl_ingress_forward") {
		t.Fatal("routed source protection and independent ingress ACL evaluation required")
	}
	{
		_, err = hostRules(in, inventory{Bridges: []string{"bad\"; flush ruleset"}})
		if err == nil {
			t.Fatal("unsafe interface accepted")
		}
	}

	in.Egress.Mode = "off"
	rules, err = hostRules(in, inventory{Bridges: []string{"incusbr0"}})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(rules, "chain classify") || !strings.Contains(rules, "chain acl_forward") {
		t.Fatal("ACL must remain when proxy is disabled")
	}
}

func TestWriteNftCheckFixtures(t *testing.T) {
	dir := os.Getenv("EGRESS_NFT_FIXTURES_DIR")
	if dir == "" {
		t.Skip("optional isolated nft parser verification")
	}

	for _, mode := range []string{"off", "direct", "proxy"} {
		in := testInstruction()
		in.Egress.Mode = mode
		rules, err := hostRules(in, inventory{Bridges: []string{"incusbr0"}, Routed: []string{"veth123"}, Internal: []string{"10.10.0.0/16", "fd00:10::/64"}})
		if err != nil {
			t.Fatal(err)
		}
		{
			err = os.WriteFile(filepath.Join(dir, mode+".nft"), []byte(rules), 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	{
		err := os.WriteFile(filepath.Join(dir, "gateway.nft"), []byte(namespaceRules()), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestACLPreservesUnrelatedReferencesAndDefaults(t *testing.T) {
	{
		refs := aclReferences("other,"+aclName, true)
		if refs != "other,"+aclName {
			t.Fatal(refs)
		}
	}

	f := Firewall{Enabled: true, IngressAction: "allow", EgressAction: "drop"}
	patch := aclConfig(map[string]string{"security.acls": "other", "security.acls.default.ingress.action": "reject"}, f)
	if patch["user.incudal.acl.ingress"] != "reject" || patch["security.acls"] != "other,"+aclName {
		t.Fatal(patch)
	}

	previous := map[string]string{}
	for k, v := range patch {
		previous[k] = v
	}

	f.Enabled = false
	restored := aclConfig(previous, f)
	if restored["security.acls"] != "other" || restored["security.acls.default.ingress.action"] != "reject" || restored["security.acls.default.egress.action"] != "" {
		t.Fatal(restored)
	}
}

func TestFailoverPriorityAndRecovery(t *testing.T) {
	nodes := []Node{{ID: 1}, {ID: 2}, {ID: 3}}
	for _, tc := range []struct {
		label               string
		active              int
		successes, failures map[int]int
		want                int
	}{
		{"first healthy is primary", 0, map[int]int{1: 1, 2: 1}, nil, 1},
		{"unhealthy primary chooses backup", 0, map[int]int{2: 1, 3: 1}, nil, 2},
		{"one miss does not flap", 1, map[int]int{2: 2}, map[int]int{1: 1}, 1},
		{"three misses switch", 1, map[int]int{2: 2}, map[int]int{1: 3}, 2},
		{"recovered primary waits for stability", 2, map[int]int{1: 1, 2: 1}, nil, 2},
		{"stable primary takes precedence", 2, map[int]int{1: 2, 2: 2}, nil, 1},
		{"all failed blocks", 2, map[int]int{}, map[int]int{1: 3, 2: 3, 3: 3}, 0},
	} {
		t.Run(tc.label, func(t *testing.T) {
			{
				got := chooseFailover(nodes, tc.active, tc.successes, tc.failures)
				if got != tc.want {
					t.Fatalf("got %d want %d", got, tc.want)
				}
			}
		})
	}
}

func TestOverlappingInternalNetworks(t *testing.T) {
	got := compactNetworks([]string{"10.10.1.0/24", "10.10.0.0/16", "10.10.0.1", "10.10.0.0/16"})
	if len(got) != 1 || got[0] != "10.10.0.0/16" {
		t.Fatal(got)
	}
}

func socksServer(t *testing.T, count *atomic.Int64) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, e := l.Accept()
			if e != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				header := make([]byte, 2)
				{
					_, e := io.ReadFull(conn, header)
					if e != nil {
						return
					}
				}

				methods := make([]byte, int(header[1]))
				{
					_, e := io.ReadFull(conn, methods)
					if e != nil {
						return
					}
				}

				_, _ = conn.Write([]byte{5, 0})
				request := make([]byte, 4)
				{
					_, e := io.ReadFull(conn, request)
					if e != nil {
						return
					}
				}

				var host string
				switch request[3] {
				case 1:
					b := make([]byte, 4)
					_, _ = io.ReadFull(conn, b)
					host = net.IP(b).String()
				case 3:
					b := make([]byte, 1)
					_, _ = io.ReadFull(conn, b)
					v := make([]byte, int(b[0]))
					_, _ = io.ReadFull(conn, v)
					host = string(v)
				case 4:
					b := make([]byte, 16)
					_, _ = io.ReadFull(conn, b)
					host = net.IP(b).String()
				default:
					return
				}

				p := make([]byte, 2)
				{
					_, e := io.ReadFull(conn, p)
					if e != nil {
						return
					}
				}

				dest, e := net.DialTimeout("tcp", net.JoinHostPort(host, fmtPort(int(p[0])<<8|int(p[1]))), time.Second)
				if e != nil {
					_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}

				defer func() { _ = dest.Close() }()
				count.Add(1)
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go func() { _, _ = io.Copy(dest, conn) }()
				_, _ = io.Copy(conn, dest)
			}()
		}
	}()
	return l
}

func fmtPort(p int) string { return fmt.Sprintf("%d", p) }
func TestRealXrayHTTPSProbe(t *testing.T) {
	if !Available() {
		t.Skip("source-only build; embedded Xray absent")
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600)
	t.Setenv("SSL_CERT_FILE", certPath)
	var count atomic.Int64
	proxy := socksServer(t, &count)
	port := proxy.Addr().(*net.TCPAddr).Port
	outbound, _ := json.Marshal(map[string]any{"protocol": "socks", "settings": map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "port": port}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := executeProbe(ctx, dir, ProbeRequest{ProbeURL: server.URL, Nodes: []Node{{ID: 7, Outbound: outbound}}})
	if len(result) != 1 || !result[0].Success || count.Load() == 0 {
		t.Fatalf("HTTPS through real Xray and SOCKS outbound failed: %+v", result)
	}

	t.Run("native-balancer-override", func(t *testing.T) {
		freePort := func() int {
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}

			defer func() { _ = l.Close() }()
			return l.Addr().(*net.TCPAddr).Port
		}

		entryPort := freePort()
		apiDir, e := os.MkdirTemp("", "xray-api-")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = os.RemoveAll(apiDir) }()
		apiSocket := coreAPIPath(apiDir)
		defer closeCoreAPI(apiSocket)
		in := testInstruction()
		in.Nodes = []Node{{ID: 7, Outbound: outbound}}
		in.Egress.ProbeURL = server.URL
		in.Egress.ProbeIntervalSeconds = 3600
		body, e := xrayConfig(in, nil, true)
		if e != nil {
			t.Fatal(e)
		}

		var config map[string]any
		_ = json.Unmarshal(body, &config)
		// Replace only the privileged transparent listener with a local SOCKS
		// entry. Production routing, observatory and RoutingService stay intact.
		listeners := []any{}
		for _, raw := range mustValue[[]any](t, config["inbounds"]) {
			v := mustValue[map[string]any](t, raw)
			switch v["tag"] {
			case "transparent":
				listeners = append(listeners, map[string]any{"tag": "transparent", "listen": "127.0.0.1", "port": entryPort, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}})
			case "api-in":
				v["listen"] = apiSocket
				listeners = append(listeners, v)
			case "dns-forward":
				listeners = append(listeners, map[string]any{"tag": "dns-forward", "listen": "127.0.0.1", "port": freePort(), "protocol": "socks", "settings": map[string]any{"auth": "noauth"}})
			}
		}

		config["inbounds"] = listeners
		body, _ = json.Marshal(config)
		path := filepath.Join(dir, "native.json")
		{
			e = os.WriteFile(path, body, 0o600)
			if e != nil {
				t.Fatal(e)
			}
		}

		binary, e := extractBinary(dir)
		if e != nil {
			t.Fatal(e)
		}

		cmd := exec.CommandContext(ctx, binary, "run", "-config", path)
		var coreOutput bytes.Buffer
		cmd.Stdout, cmd.Stderr = &coreOutput, &coreOutput
		{
			e = cmd.Start()
			if e != nil {
				t.Fatal(e)
			}
		}

		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		e = WaitCoreAPI(ctx, apiSocket, true)
		if e != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("native API readiness failed: %v; fixture core output: %s", e, coreOutput.String())
		}
		if !strings.Contains(gatewayUnit(binary, path, true), "-network-core-api") || strings.Contains(gatewayUnit(binary, path, true), " api bo ") {
			t.Fatal("startup must wait on the Go API adapter, not a CLI balancer command")
		}
		override := func(tag string) {
			t.Helper()
			{
				err := overrideCore(ctx, apiSocket, tag)
				if err != nil {
					t.Fatalf("native gRPC balancer override failed: %v", err)
				}
			}
		}

		override("blocked")
		{
			p := ProbeURL(ctx, entryPort, server.URL)
			if p.Success {
				t.Fatal("blackhole override leaked to direct outbound")
			}
		}

		override(nodeTag(7))
		{
			p := ProbeURL(ctx, entryPort, server.URL)
			if !p.Success {
				t.Fatalf("native balancer failed to select the healthy proxy: %+v", p)
			}
		}
	})
	_ = proxy.Close()
	result = executeProbe(ctx, dir, ProbeRequest{ProbeURL: server.URL, Nodes: []Node{{ID: 7, Outbound: outbound}}})
	if result[0].Success {
		t.Fatal("dead proxy incorrectly reported healthy")
	}
}
