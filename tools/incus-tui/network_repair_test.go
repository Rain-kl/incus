package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type repairFixture struct {
	network   *nodeNetwork
	instances map[string]*repairObject
	profiles  map[string]*repairObject
	writes    []string
	fail      string
	clustered bool
	onWrite   func()
	forwards  []string
}

func fixtureContainer(name, project, status, address string) *repairObject {
	devices := map[string]map[string]string{
		"root": {"type": "disk", "path": "/", "pool": "DD", "size": "25GiB"},
		"eth0": {"type": "nic", "nictype": "bridged", "parent": panelBridge, "name": "eth0", "ipv4.address": address, "hwaddr": "00:16:3e:11:22:33", "limits.ingress": "50Mbit"},
	}
	return &repairObject{Name: name, Project: project, Type: "container", Status: status, Config: map[string]string{"user.user-data": "password: fixture-hidden-value", "limits.memory": "2GiB"}, Devices: devices, ExpandedDevices: copyDevices(devices)}
}

func newRepairFixture() *repairFixture {
	f := &repairFixture{
		network:   &nodeNetwork{Name: panelBridge, Type: "bridge", Managed: true, Config: panelBridgeConfig()},
		instances: map[string]*repairObject{}, profiles: map[string]*repairObject{},
	}
	for _, object := range []*repairObject{fixtureContainer("a", "default", "Running", "10.10.2.132"), fixtureContainer("b", "default", "Stopped", "10.10.2.132"), fixtureContainer("a", "other", "Stopped", "10.10.1.1")} {
		f.instances[repairPath("instances", *object)] = object
	}
	profile := &repairObject{Name: "default", Project: "default", Config: map[string]string{"limits.cpu": "2"}, Devices: map[string]map[string]string{"root": {"type": "disk", "pool": "DD", "path": "/"}, "eth0": {"type": "nic", "network": panelBridge, "name": "eth0"}}}
	f.profiles[repairPath("profiles", *profile)] = profile
	return f
}

func (f *repairFixture) usedBy() []string {
	var result []string
	for _, objects := range []map[string]*repairObject{f.instances, f.profiles} {
		for key, object := range objects {
			for _, device := range object.Devices {
				if usesPanelBridge(device) {
					result = append(result, key)
					break
				}
			}
		}
	}
	return result
}

func (f *repairFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.RequestURI()
		write := r.Method != "GET"
		if write {
			f.writes = append(f.writes, r.Method+" "+key)
			if f.fail == r.Method+" "+r.URL.Path {
				w.WriteHeader(500)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "dnsmasq: fixture permission denied", "error_code": 500})
				return
			}
		}
		switch {
		case key == "/1.0":
			extensions := []string{"profiles_all_projects", "instance_all_projects"}
			if f.forwards != nil {
				extensions = append(extensions, "network_forward")
			}
			syncJSON(w, map[string]any{"api_extensions": extensions, "environment": map[string]any{"server_clustered": f.clustered}})
		case r.Method == "GET" && r.URL.Path == "/1.0/networks/incusbr0/forwards":
			syncJSON(w, f.forwards)
		case r.Method == "GET" && r.URL.Path == "/1.0/networks":
			list := []nodeNetwork{}
			if f.network != nil {
				list = append(list, *f.network)
			}
			syncJSON(w, list)
		case r.URL.Path == "/1.0/networks/"+panelBridge:
			if f.network == nil {
				t.Error("queried missing bridge")
				w.WriteHeader(404)
				return
			}
			if r.Method == "GET" {
				w.Header().Set("ETag", "bridge-etag")
				network := *f.network
				network.UsedBy = f.usedBy()
				syncJSON(w, network)
			} else if r.Method == "DELETE" {
				if len(f.usedBy()) != 0 || r.Header.Get("If-Match") != "bridge-etag" {
					t.Error("bridge deleted while in use or without concurrency check")
				}
				f.network = nil
				syncJSON(w, nil)
			}
		case r.Method == "POST" && r.URL.Path == "/1.0/networks":
			if f.network != nil {
				t.Error("bridge created without deleting old bridge")
			}
			var network nodeNetwork
			if json.NewDecoder(r.Body).Decode(&network) != nil {
				t.Error("bad network payload")
			}
			network.Managed = true
			if !compatiblePanelNetwork(&network) {
				t.Error("wrong new bridge configuration")
			}
			f.network = &network
			syncJSON(w, nil)
		case r.Method == "GET" && (r.URL.Path == "/1.0/instances" || r.URL.Path == "/1.0/profiles"):
			if r.URL.Query().Get("all-projects") != "true" {
				t.Error("inventory omitted projects")
			}
			list := []repairObject{}
			objects := f.instances
			if r.URL.Path == "/1.0/profiles" {
				objects = f.profiles
			}
			for _, object := range objects {
				list = append(list, *object)
			}
			syncJSON(w, list)
		default:
			kind := "instances"
			if strings.HasPrefix(r.URL.Path, "/1.0/profiles/") {
				kind = "profiles"
			}
			objects := f.instances
			if kind == "profiles" {
				objects = f.profiles
			}
			objectKey := strings.Replace(key, "/state?", "?", 1)
			object := objects[objectKey]
			if object == nil {
				t.Errorf("unexpected request: %s %s", r.Method, key)
				w.WriteHeader(404)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/state") {
				if r.Method == "GET" {
					syncJSON(w, map[string]any{"network": map[string]any{"eth0": map[string]any{"addresses": []any{map[string]string{"family": "inet", "address": "10.10.3.100"}}}}})
					return
				}
				var body struct {
					Action string `json:"action"`
					Force  bool   `json:"force"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Force {
					t.Error("unexpected forced container shutdown")
				}
				if body.Action == "stop" {
					object.Status = "Stopped"
				} else if body.Action == "start" {
					object.Status = "Running"
				} else {
					t.Error("unknown state action")
				}
				syncJSON(w, nil)
			} else if r.Method == "GET" {
				w.Header().Set("ETag", fmt.Sprintf("etag-%d", len(f.writes)))
				syncJSON(w, object)
			} else if r.Method == "PATCH" {
				if r.Header.Get("If-Match") != fmt.Sprintf("etag-%d", len(f.writes)-1) {
					t.Error("device write omitted ETag")
				}
				var body struct {
					Config  map[string]string            `json:"config"`
					Devices map[string]map[string]string `json:"devices"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid device payload")
				}
				object.Config, object.Devices = body.Config, body.Devices
				syncJSON(w, nil)
			} else {
				t.Errorf("unexpected mutation: %s %s", r.Method, key)
				w.WriteHeader(500)
			}
		}
		if write && f.onWrite != nil {
			f.onWrite()
		}
	}
}

func TestBridgeRepairPreservesAddressesDisksAndStoppedState(t *testing.T) {
	f := newRepairFixture()
	root := maps.Clone(f.instances["/1.0/instances/a?project=default"].Devices["root"])
	var logs strings.Builder
	ctx := context.WithValue(context.Background(), operationLogKey{}, &operationLog{writer: &logs})
	c := apiFixture(t, f.handler(t))
	dir := t.TempDir()
	err := c.repairBridge(ctx, &fakeRunner{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := f.instances["/1.0/instances/a?project=default"], f.instances["/1.0/instances/b?project=default"]
	if a.Devices["eth0"]["ipv4.address"] != "10.10.2.132" || b.Devices["eth0"]["ipv4.address"] != "10.10.1.2" {
		t.Fatal("valid address lost or duplicate not reallocated", a.Devices, b.Devices)
	}
	if a.Devices["eth0"]["network"] != panelBridge || a.Devices["eth0"]["parent"] != "" || a.Devices["eth0"]["nictype"] != "" {
		t.Fatal("NIC not attached to managed network")
	}
	if a.Devices["eth0"]["hwaddr"] != "00:16:3e:11:22:33" || a.Devices["eth0"]["limits.ingress"] != "50Mbit" || !reflect.DeepEqual(root, a.Devices["root"]) {
		t.Fatal("unrelated resource settings changed")
	}
	if a.Status != "Running" || b.Status != "Stopped" {
		t.Fatal("original running state lost")
	}
	if f.profiles["/1.0/profiles/default?project=default"].Config["limits.cpu"] != "2" {
		t.Fatal("profile config lost")
	}
	if strings.Contains(logs.String(), "fixture-hidden-value") {
		t.Fatal("guest cloud-init secret leaked into logs")
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "bridge-*", "state.json"))
	if len(backups) != 1 {
		t.Fatal("missing backup")
	}
	info, err := os.Stat(backups[0])
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("unsafe backup permissions")
	}
	data, _ := os.ReadFile(backups[0])
	if !strings.Contains(string(data), "修复完成") || !strings.Contains(string(data), "ip_changes") {
		t.Fatal("backup missing progress or address plan")
	}
	if !slices.Contains(f.writes, "DELETE /1.0/networks/incusbr0") {
		t.Fatal("old bridge was not replaced")
	}
}

func TestBridgeRepairFailureRetainsDiagnosticsAndDoesNotStartContainers(t *testing.T) {
	f := newRepairFixture()
	f.fail = "POST /1.0/networks"
	c := apiFixture(t, f.handler(t))
	err := c.repairBridge(context.Background(), &fakeRunner{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "dnsmasq: fixture permission denied") || !strings.Contains(err.Error(), "state.json") {
		t.Fatalf("lost repair failure: %v", err)
	}
	if f.instances["/1.0/instances/a?project=default"].Status != "Stopped" {
		t.Fatal("container restarted with broken network")
	}
}

func TestBridgeRepairRejectsUnsafeTargetsBeforeMutation(t *testing.T) {
	for _, mode := range []string{"cluster", "vm", "ephemeral", "frozen", "physical", "unknown-profile-user", "unknown-network-user", "forwards"} {
		t.Run(mode, func(t *testing.T) {
			f := newRepairFixture()
			a := f.instances["/1.0/instances/a?project=default"]
			switch mode {
			case "cluster":
				f.clustered = true
			case "vm":
				a.Type = "virtual-machine"
			case "ephemeral":
				a.Ephemeral = true
			case "frozen":
				a.Status = "Frozen"
			case "physical":
				f.network.Config["bridge.external_interfaces"] = "eth0"
			case "unknown-profile-user":
				f.profiles["/1.0/profiles/default?project=default"].UsedBy = []string{"/1.0/instances/unknown"}
			case "unknown-network-user":
				f.network.UsedBy = []string{"/1.0/instances/unknown"}
			case "forwards":
				f.forwards = []string{"/1.0/networks/incusbr0/forwards/192.0.2.1"}
			}
			c := apiFixture(t, f.handler(t))
			err := c.repairBridge(context.Background(), &fakeRunner{}, t.TempDir())
			if err == nil || len(f.writes) != 0 {
				t.Fatal("unsafe repair mutated the node", err, f.writes)
			}
		})
	}
}

func TestBridgeRepairPreservesLiveIPWhenLeaseIsMissingAndHandlesCancellation(t *testing.T) {
	f := newRepairFixture()
	f.instances["/1.0/instances/a?project=default"].ExpandedDevices["eth0"]["ipv4.address"] = ""
	c := apiFixture(t, f.handler(t))
	plan, err := c.repairInventory(context.Background())
	if err != nil || plan.Instances[0].NICs["eth0"]["ipv4.address"] != "10.10.3.100" {
		t.Fatal("live address was discarded", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.onWrite = cancel
	err = c.repairBridge(ctx, &fakeRunner{}, t.TempDir())
	if !errors.Is(err, context.Canceled) || slices.Contains(f.writes, "DELETE /1.0/networks/incusbr0") {
		t.Fatal("cancelled repair continued or hid cause", err)
	}
}

func TestRepairMenuRequiresConfirmation(t *testing.T) {
	m := model{backend: &backend{runner: &fakeRunner{}}, menuGroup: "repair"}
	updated, cmd := m.selectMenu()
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" || m.confirm == nil || !strings.Contains(m.View(), "会中断网络") {
		t.Fatal("repair did not require explicit confirmation")
	}
}

func TestBridgeRepairMissingAndUnmanagedBridge(t *testing.T) {
	for _, mode := range []string{"missing", "unmanaged", "occupied"} {
		t.Run(mode, func(t *testing.T) {
			f := newRepairFixture()
			if mode == "missing" {
				f.network = nil
			} else {
				f.network.Managed = false
			}
			runner := &fakeRunner{reply: func(call call) ([]byte, error) {
				args := strings.Join(call.args, " ")
				if args == "-j -d link show dev incusbr0" {
					return []byte(`[{"linkinfo":{"info_kind":"bridge"}}]`), nil
				}
				if args == "-j link show master incusbr0" {
					if mode == "occupied" {
						return []byte(`[{"ifname":"eth0"}]`), nil
					}
					return []byte(`[]`), nil
				}
				if args == "link delete dev incusbr0 type bridge" {
					f.network = nil
					return nil, nil
				}
				t.Error("unexpected OS command", args)
				return nil, errors.New("unexpected command")
			}}
			c := apiFixture(t, f.handler(t))
			err := c.repairBridge(context.Background(), runner, t.TempDir())
			if mode == "occupied" {
				if err == nil || len(f.writes) != 0 {
					t.Fatal("occupied bridge was modified", err)
				}
			} else if err != nil || !compatiblePanelNetwork(f.network) {
				t.Fatal("missing or unmanaged bridge was not rebuilt", err)
			}
		})
	}
}

func TestBridgeRepairUpdatesChangedIPReferencesAndPreservesInheritedNIC(t *testing.T) {
	f := newRepairFixture()
	b := f.instances["/1.0/instances/b?project=default"]
	b.Config["user.network-config"] = "addresses: [10.10.2.132/24]\nroutes: [{via: 192.0.2.1}]"
	b.Config["volatile.cloud-init.instance-id"] = "fixture-id"
	b.Devices["ssh"] = map[string]string{"type": "proxy", "connect": "tcp:10.10.2.132:22", "listen": "tcp:127.0.0.1:12345", "nat": "true"}
	b.ExpandedDevices = copyDevices(b.Devices)
	delete(b.Devices, "eth0") // The effective NIC comes from a profile.
	f.network.Config["ipv4.address"] = "192.0.2.1/24"
	c := apiFixture(t, f.handler(t))
	err := c.repairBridge(context.Background(), &fakeRunner{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b.Devices["ssh"]["connect"] != "tcp:10.10.1.2:22" || !strings.Contains(b.Config["user.network-config"], "10.10.1.2/22") || !strings.Contains(b.Config["user.network-config"], "via: 10.10.0.1") {
		t.Fatal("reallocated IP references were not repaired", b.Config, b.Devices)
	}
	if b.Devices["root"]["pool"] != "DD" || b.Devices["eth0"]["hwaddr"] != "00:16:3e:11:22:33" {
		t.Fatal("inherited NIC or storage was lost")
	}
}

func TestBridgeRepairAddressExhaustionDoesNotMutateNode(t *testing.T) {
	f := newRepairFixture()
	f.instances = map[string]*repairObject{}
	index := 0
	last := netip.MustParseAddr("10.10.3.254")
	for ip := netip.MustParseAddr("10.10.1.1"); ip.Compare(last) <= 0; ip = ip.Next() {
		object := fixtureContainer(fmt.Sprintf("c-%04d", index), "default", "Stopped", ip.String())
		f.instances[repairPath("instances", *object)] = object
		index++
	}
	missing := fixtureContainer("zz-missing", "default", "Stopped", "")
	f.instances[repairPath("instances", *missing)] = missing
	c := apiFixture(t, f.handler(t))
	err := c.repairBridge(context.Background(), &fakeRunner{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "地址池已满") || len(f.writes) != 0 || index != 766 {
		t.Fatal("exhausted address pool allowed a destructive repair", err, index)
	}
}
