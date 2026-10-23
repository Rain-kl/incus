package egress

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testGroups() Instruction {
	policy := Policy{Mode: "proxy", Strategy: "balance", ProbeURL: "https://www.gstatic.com/generate_204", ProbeIntervalSeconds: 30, ProxyNodeIDs: []int{1}}
	group := func(key string) EgressGroup {
		return EgressGroup{Key: key, Egress: policy, Nodes: []Node{{ID: 1, Outbound: json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"198.18.0.2","port":1080}]}}`)}}}
	}

	nic := func(name, ip string) []EgressNIC { return []EgressNIC{{HostName: name, IPv4: ip, Bridge: "incusbr0"}} }
	return Instruction{SchemaVersion: 2, Revision: 1, Groups: []EgressGroup{group("v1"), group("v2")}, Bindings: []EgressBinding{
		{InstanceName: "a", Generation: 1, Mode: "profile", GroupKey: "v1", Nics: nic("ipgaaaaaaaaaaa", "10.10.0.2")},
		{InstanceName: "b", Generation: 1, Mode: "profile", GroupKey: "v2", Nics: nic("ipgbbbbbbbbbbb", "10.10.0.3")},
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

	bridge, inet, err := compileGroupRules(in, runtime, map[string]bool{"v2": true})
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

	if runtime["v1"].mark() == runtime["v2"].mark() {
		t.Fatal("group marks collide")
	}

	changed := in
	changed.Bindings = append(append([]EgressBinding{}, in.Bindings...), in.Bindings[0])
	if validateGroups(changed) == nil {
		t.Fatal("duplicate binding accepted")
	}

	changed = in
	changed.Bindings = append([]EgressBinding{}, in.Bindings...)
	changed.Bindings[0].GroupKey = "v999"
	if validateGroups(changed) == nil {
		t.Fatal("unknown group accepted")
	}

	if strings.Contains(groupUnit(runtime["v1"], "/xray"), "flush set") {
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

func TestGroupPinnedCoreConfig(t *testing.T) {
	in := testGroups()
	body, err := groupCoreConfig(in.Groups[0], groupRuntime{Key: "v1", Slot: 1})
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
