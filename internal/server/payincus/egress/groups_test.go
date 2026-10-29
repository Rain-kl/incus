package egress

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGroupCoreWaitsForAPIAndHonorsCancellation(t *testing.T) {
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reference, ready := waitGroupCore(ctx, func() (string, error) {
		attempts++
		if attempts == 1 {
			return "", errors.New("API not ready")
		}
		return "current-invocation", nil
	})
	if !ready || reference != "current-invocation" || attempts != 2 {
		t.Fatal("group did not wait for its current core API")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, ready = waitGroupCore(ctx, func() (string, error) { return "", errors.New("API unavailable") })
	cancel()
	if ready {
		t.Fatal("unavailable core must remain blocked after timeout")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, ready = waitGroupCore(ctx, func() (string, error) { t.Fatal("check ran after cancellation"); return "", nil })
	if ready {
		t.Fatal("cancelled core must remain blocked")
	}
}

func TestGroupHealthWaitsForStartupAndHonorsCancellation(t *testing.T) {
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !retryGroupHealth(ctx, func() bool { attempts++; return attempts == 2 }) || attempts != 2 {
		t.Fatal("group did not recover after its initial startup failure")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if retryGroupHealth(ctx, func() bool { t.Fatal("probe ran after cancellation"); return true }) {
		t.Fatal("cancelled group must remain blocked")
	}
}

func testGroups() Instruction {
	policy := Policy{Mode: "proxy", Strategy: "balance", ProbeURL: "https://www.gstatic.com/generate_204", ProbeIntervalSeconds: 30, ProxyNodeIDs: []int{1}}
	group := func(key string) EgressGroup {
		return EgressGroup{Key: key, Egress: policy, Nodes: []Node{{ID: 1, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"198.18.0.2","port":1080}]}}`)}}}
	}

	nic := func(name, ip string) []EgressNIC { return []EgressNIC{{HostName: name, IPv4: ip, Bridge: "incusbr0"}} }
	return Instruction{SchemaVersion: 2, Revision: 1, Groups: []EgressGroup{group("p1"), group("p2")}, Bindings: []EgressBinding{
		{InstanceName: "a", Generation: 1, Mode: "profile", GroupKey: "p1", Nics: nic("ipgaaaaaaaaaaa", "10.10.0.2")},
		{InstanceName: "b", Generation: 1, Mode: "profile", GroupKey: "p2", Nics: nic("ipgbbbbbbbbbbb", "10.10.0.3")},
		{InstanceName: "direct", Generation: 1, Mode: "direct", Nics: nic("ipgccccccccccc", "10.10.0.4")},
	}, GroupInternal: []string{"10.10.0.0/24"}, GroupBridgeDNS: []string{"10.10.0.1"}, GroupDirectDNS: "198.18.0.2"}
}

func TestGroupIsolationAndValidation(t *testing.T) {
	in := testGroups()
	all := map[string]groupRuntime{}
	runtime, err := allocateGroups(in.Groups, all)
	if err != nil {
		t.Fatal(err)
	}

	bridge, inet, err := compileGroupRules(in, runtime, map[string]bool{"p2": true})
	if err != nil {
		t.Fatal(err)
	}

	for _, needle := range []string{"hook prerouting priority -310", "ip saddr 10.10.0.2", "ip saddr 10.10.0.3", "iifname \"ipg*\" drop"} {
		if !strings.Contains(bridge, needle) {
			t.Fatalf("missing %s", needle)
		}
	}

	for _, needle := range []string{"priority -110", "goto route_420100", "meta mark & 0x00ff0000 == 0x00420000 meta mark & 0x0000ff00 != 0 drop", "dnat ip to 169.254.244.6:53", "dnat ip to 169.254.244.10:53", "dnat ip to 198.18.0.2:53", "ct direction reply", "ct status dnat", "oifname != \"ipgh1\" drop", "oifname != \"ipgh2\" drop", "0x420200"} {
		if !strings.Contains(inet, needle) {
			t.Fatalf("missing %s", needle)
		}
	}

	if runtime["p1"].mark() == runtime["p2"].mark() {
		t.Fatal("group marks collide")
	}

	changed := in
	changed.Bindings = append(append([]EgressBinding{}, in.Bindings...), in.Bindings[0])
	if validateGroups(changed) == nil {
		t.Fatal("duplicate binding accepted")
	}

	changed = in
	changed.Bindings = append([]EgressBinding{}, in.Bindings...)
	changed.Bindings[0].GroupKey = "p0"
	if validateGroups(changed) == nil {
		t.Fatal("unknown group accepted")
	}

	if strings.Contains(groupUnit(runtime["p1"], "/xray"), "flush set") {
		t.Fatal("group service may not clear other groups readiness")
	}
	{
		dir := os.Getenv("PACKAGE_EGRESS_FIXTURES")
		if dir != "" {
			{
				err = os.MkdirAll(dir, 0o700)
				if err != nil {
					t.Fatal(err)
				}
			}

			_ = os.WriteFile(filepath.Join(dir, "groups.nft"), []byte(bridge+inet), 0o600)
			for _, group := range in.Groups {
				cfg, e := groupCoreConfig(group, runtime[group.Key])
				if e != nil {
					t.Fatal(e)
				}

				_ = os.WriteFile(filepath.Join(dir, group.Key+".json"), cfg, 0o600)
			}
		}
	}
}

func TestMultiNodeGroupValidationAndScopedInternalCIDRs(t *testing.T) {
	in := testGroups()
	in.Groups[0].Nodes = append(in.Groups[0].Nodes, Node{ID: 2, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"proxy-two.example","port":1080}]}}`)})
	in.Groups[0].Egress.ProxyNodeIDs = []int{1, 2}
	in.Groups[0].Egress.InternalCIDRs = []string{"192.0.2.77/24"}
	in.Groups[1].Egress.InternalCIDRs = []string{"198.51.100.5"}
	err := validateGroups(in)
	if err != nil {
		t.Fatalf("valid ordered multi-node profile was rejected: %v", err)
	}
	for _, invalid := range []Instruction{
		func() Instruction {
			v := in
			v.Groups = append([]EgressGroup{}, in.Groups...)
			v.Groups[0].Nodes = nil
			return v
		}(),
		func() Instruction {
			v := in
			v.Groups = append([]EgressGroup{}, in.Groups...)
			v.Groups[0].Egress.ProxyNodeIDs = []int{1}
			return v
		}(),
		func() Instruction {
			v := in
			v.Groups = append([]EgressGroup{}, in.Groups...)
			v.Groups[0].Nodes = []Node{in.Groups[0].Nodes[0], Node{ID: 1, Outbound: in.Groups[0].Nodes[1].Outbound}}
			v.Groups[0].Egress.ProxyNodeIDs = []int{1, 1}
			return v
		}(),
		func() Instruction {
			v := in
			v.Groups = append([]EgressGroup{}, in.Groups...)
			v.Groups[0].Egress.ProxyNodeIDs = []int{2, 1}
			return v
		}(),
	} {
		if validateGroups(invalid) == nil {
			t.Fatal("invalid node cardinality, duplicate identity or ordering was accepted")
		}
	}
	runtime, err := allocateGroups(in.Groups, map[string]groupRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	_, rules, err := compileGroupRules(in, runtime, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rules, "chain route_420100 {") {
		t.Fatal("first profile route was not generated")
	}
	first := strings.SplitN(rules, "chain route_420100 {", 2)[1]
	first = strings.SplitN(first, "chain route_420200 {", 2)[0]
	second := strings.SplitN(rules, "chain route_420200 {", 2)[1]
	second = strings.SplitN(second, "chain guard {", 2)[0]
	if !strings.Contains(first, "192.0.2.0/24") || strings.Contains(first, "198.51.100.5/32") {
		t.Fatalf("first profile internal CIDRs leaked across policy scope: %s", first)
	}
	if !strings.Contains(second, "198.51.100.5/32") || strings.Contains(second, "192.0.2.0/24") {
		t.Fatalf("second profile internal CIDRs leaked across policy scope: %s", second)
	}
}

func TestGroupPinnedCoreConfig(t *testing.T) {
	in := testGroups()
	body, err := groupCoreConfig(in.Groups[0], groupRuntime{Key: "p1", Slot: 1})
	if err != nil {
		t.Fatal(err)
	}

	var cfg map[string]any
	_ = json.Unmarshal(body, &cfg)
	for _, raw := range mustValue[[]any](t, cfg["outbounds"]) {
		if mustValue[map[string]any](t, raw)["protocol"] == "freedom" {
			t.Fatal("proxy groups must have no native direct outbound")
		}
	}

	if cfg["observatory"] != nil {
		t.Fatal("group health must be scoped to the group")
	}

	if !strings.Contains(string(body), "group-dns-query") || !strings.Contains(string(body), `"network": "tcp"`) {
		t.Fatal("group DNS must support TCP-only proxies")
	}
	{
		binary := os.Getenv("PACKAGE_EGRESS_XRAY")
		if binary != "" {
			path := filepath.Join(t.TempDir(), "group.json.next")
			_ = os.WriteFile(path, body, 0o600)
			{
				err = validateCore(context.Background(), binary, path)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestSharedGroupCorePreservesStrategiesProbesAndDNSBalancer(t *testing.T) {
	for _, strategy := range []string{"balance", "failover", "leastPing"} {
		t.Run(strategy, func(t *testing.T) {
			group := testGroups().Groups[0]
			group.Egress.Strategy = strategy
			group.Nodes = []Node{
				{ID: 1, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"proxy-one.example","port":1080}]}}`)},
				{ID: 2, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"proxy-two.example","port":1080}]}}`)},
			}
			body, err := groupCoreConfig(group, groupRuntime{Key: group.Key, Slot: 1})
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			err = json.Unmarshal(body, &config)
			if err != nil {
				t.Fatal(err)
			}
			routing := mustValue[map[string]any](t, config["routing"])
			balancers := mustValue[[]any](t, routing["balancers"])
			if len(balancers) != 1 {
				t.Fatalf("strategy %s has no isolated shared balancer: %#v", strategy, balancers)
			}
			balancer := mustValue[map[string]any](t, balancers[0])
			if balancer["tag"] != "egress" || !strings.Contains(strings.Join(anyStrings(t, balancer["selector"]), ","), nodeTag(1)) || !strings.Contains(strings.Join(anyStrings(t, balancer["selector"]), ","), nodeTag(2)) {
				t.Fatalf("strategy %s did not include both configured outbounds: %#v", strategy, balancer)
			}
			if config["observatory"] == nil {
				t.Fatalf("strategy %s lost shared node health observation", strategy)
			}
			pinned := map[string]string{}
			transparentUsesBalancer := false
			dnsUsesBalancer := false
			for _, raw := range mustValue[[]any](t, routing["rules"]) {
				rule := mustValue[map[string]any](t, raw)
				for _, inbound := range anyStrings(t, rule["inboundTag"]) {
					switch inbound {
					case "probe-0", "probe-1":
						pinned[inbound], _ = rule["outboundTag"].(string)
					case "transparent":
						transparentUsesBalancer = rule["balancerTag"] == "egress"
					case "group-dns-query":
						dnsUsesBalancer = rule["balancerTag"] == "egress"
					}
				}
			}
			if pinned["probe-0"] != nodeTag(1) || pinned["probe-1"] != nodeTag(2) {
				t.Fatalf("probe traffic was not pinned to each individual node: %#v", pinned)
			}
			if !transparentUsesBalancer || !dnsUsesBalancer {
				t.Fatalf("transparent or group DNS traffic bypasses shared balancer: transparent=%t dns=%t", transparentUsesBalancer, dnsUsesBalancer)
			}
			binary := os.Getenv("PACKAGE_EGRESS_XRAY")
			if binary != "" {
				path := filepath.Join(t.TempDir(), "group.json.next")
				err = os.WriteFile(path, body, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				err = validateCore(context.Background(), binary, path)
				if err != nil {
					t.Fatalf("pinned Xray rejected %s strategy: %v", strategy, err)
				}
			}
		})
	}
}

func anyStrings(t *testing.T, value any) []string {
	t.Helper()
	if value == nil {
		return nil
	}
	items := mustValue[[]any](t, value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, mustValue[string](t, item))
	}
	return result
}
