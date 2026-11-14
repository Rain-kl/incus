package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestStructuralEgressMatchingIgnoresForeignResources(t *testing.T) {
	// Reserved group rule & host egress rule match; Tailscale & foreign table rule do not.
	rGroup, ok := parseIPRuleLine("-4", "17001:\tfrom all fwmark 0x420100/0xffff00 lookup 8001")
	if !ok || !isEgressPolicyRule(rGroup) {
		t.Fatalf("expected group policy rule to match: %+v", rGroup)
	}
	rHost, ok := parseIPRuleLine("-4", "17420:\tfrom all fwmark 0x420000/0xff0000 lookup 7420")
	if !ok || !isEgressPolicyRule(rHost) {
		t.Fatalf("expected host policy rule to match: %+v", rHost)
	}
	rTS, ok := parseIPRuleLine("-4", "5210:\tfrom all fwmark 0x80000/0xff0000 lookup 52")
	if !ok || isEgressPolicyRule(rTS) {
		t.Fatalf("Tailscale rule must never be treated as egress rule: %+v", rTS)
	}
	rForeign, ok := parseIPRuleLine("-4", "500:\tfrom 10.0.0.0/8 lookup 8001")
	if !ok || isEgressPolicyRule(rForeign) {
		t.Fatalf("foreign rule without egress priority/fwmark must not be owned: %+v", rForeign)
	}

	routeOut := strings.Join([]string{
		"unreachable default table 8001 metric 42760",
		"default via 169.254.244.6 dev ipgh1 table 8001 onlink",
		"192.168.99.0/24 dev eth0 table 8002 proto static",
		"default via 192.0.2.1 dev eth0 table 52",
	}, "\n")
	tables := splitEgressRouteTables(routeOut)
	if !tableContainsEgressRoutes(tables["8001"]) {
		t.Fatal("expected table 8001 egress marker detection")
	}
	if tableContainsEgressRoutes(tables["8002"]) {
		t.Fatal("unrelated table 8002 routes should not match egress markers")
	}
	if _, exists := tables["52"]; exists {
		t.Fatal("Tailscale table 52 must not appear in reserved tables")
	}

	for _, name := range []string{"igw-host", "ipgh1", "ipgh63"} {
		if !isEgressLinkName(name) {
			t.Fatalf("expected %s to match egress link", name)
		}
	}
	for _, name := range []string{"ipg0123456789a", "ipgh0", "ipgh64", "incusbr0", "tailscale0"} {
		if isEgressLinkName(name) {
			t.Fatalf("did not expect %s to match host egress link", name)
		}
	}

	if !isCandidateEgressNFTTable("inet", "legacy_egress") || !nftBodyMatchesEgressFingerprint("table inet legacy_egress {\n\tset ready {\n\t\ttype mark\n\t}\n}") {
		t.Fatal("expected legacy_egress table with mark set to match fingerprint")
	}
	if !isCandidateEgressNFTTable("bridge", "legacy_package_ports") || !nftBodyMatchesEgressFingerprint(`iifname "ipg*" drop`) {
		t.Fatal("expected legacy_package_ports with ipg* drop to match fingerprint")
	}
	if nftBodyMatchesEgressFingerprint("chain input { type filter hook input priority 0; policy accept; }") {
		t.Fatal("generic nft table must not match egress fingerprint")
	}
}

func TestOfflineStatusAndDiagnoseReportReadOnly(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-network.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus.service.d/incus-network.conf"), "[Unit]\nRequires=incus-network.service\n", 0o644)
	writeTestFile(t, filepath.Join(root, "run/incus-upgrade/phase"), "restore\n", 0o644)
	writeTestFile(t, filepath.Join(root, "run/incus-upgrade/active"), "incus-agent.service\nincus.service\n", 0o644)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/agent/network/applied.json"), `{"instruction":{"schemaVersion":2,"revision":3,"egress":{"mode":"off"},"groups":[{"key":"p3"}],"bindings":[]}}`, 0o600)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/agent/network/owner"), "incus-network-v1\n", 0o600)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/agent/network/groups/owner"), "incus-package-egress-v2\n", 0o600)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/agent/network/network-worker"), "#!/bin/sh\n", 0o755)

	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		switch {
		case c.name == "systemctl" && len(c.args) > 1 && c.args[0] == "show":
			unit := c.args[1]
			if unit == "incus" || unit == "incus.service" || unit == "incus-agent" || unit == "incus-agent.service" {
				return []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=dependency\nUnitFileState=enabled"), nil
			}
			if unit == "incus-network.service" {
				return []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nUnitFileState=enabled"), nil
			}
			return []byte("LoadState=not-found\nActiveState=inactive"), nil
		case c.name == "journalctl":
			return []byte("reserved group routing table already in use"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"-4", "rule", "show"}):
			return []byte("17001:\tfrom all fwmark 0x420100/0xffff00 lookup 8001\n5210:\tfrom all fwmark 0x80000/0xff0000 lookup 52\n"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"-4", "route", "show", "table", "all"}):
			return []byte("unreachable default table 8001 metric 42760\ndefault via 192.0.2.1 dev eth0 table main\n"), nil
		case c.name == "nft" && slices.Equal(c.args, []string{"list", "tables"}):
			return []byte("table inet legacy_egress\ntable inet filter\n"), nil
		case c.name == "nft" && slices.Equal(c.args, []string{"list", "table", "inet", "legacy_egress"}):
			return []byte("table inet legacy_egress {\n\tset ready {\n\t\ttype mark\n\t}\n}\n"), nil
		case c.name == "/usr/bin/incus":
			return nil, errors.New("dial unix /var/lib/incus/unix.socket: no such file or directory")
		}
		return nil, nil
	}}

	b := &backend{runner: f, rootDir: root, logs: &operationLog{}}
	statusOut := b.status(context.Background())
	for _, want := range []string{
		"upgrade", // Wait check exact Chinese text
		"restore",
		"incus-network.service",
		"reserved group routing table already in use",
		"孤立 groups/owner",
	} {
		_ = want
	}
	for _, want := range []string{"phase=restore", "incus-network.service", "reserved group routing table already in use", "孤立 groups/owner"} {
		if !strings.Contains(statusOut, want) {
			t.Fatalf("status missing %q:\n%s", want, statusOut)
		}
	}

	var buf bytes.Buffer
	if err := runDiagnoseCLI(context.Background(), b, &buf); err != nil {
		t.Fatal(err)
	}
	report := buf.String()
	for _, want := range []string{
		"=== 离线故障诊断（只读，未修改系统） ===",
		"ip-rule=1",
		"route-table=1",
		"nft-table=1",
		"reserved group routing table already in use",
		"groups/owner: 异常",
		"phase=restore",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("diagnose output missing %q:\n%s", want, report)
		}
	}
	// Read-only guarantee: no mutation commands or file removals ran.
	for _, c := range f.calls {
		if c.name == "kill" || (c.name == "nft" && c.args[0] == "delete") || (c.name == "ip" && slices.Contains(c.args, "del")) || (c.name == "systemctl" && c.args[0] == "start") {
			t.Fatalf("diagnose ran mutating command: %+v", c)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/incus/agent/network/groups/owner")); err != nil {
		t.Fatal("diagnose deleted groups/owner")
	}
}

func TestRecoverBlockedServicesPreservesDataAndResumesUpgrade(t *testing.T) {
	root := t.TempDir()
	netDir := filepath.Join(root, "var/lib/incus/agent/network")
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-network.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-package-guard.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus.service.d/incus-network.conf"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus.service.d/incus-package-guard.conf"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/incus/agent/config.yaml"), "panel: https://panel.example.com\n", 0o600)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/containers/c1/metadata.yaml"), "name: c1\n", 0o644)
	writeTestFile(t, filepath.Join(root, "run/incus-upgrade/phase"), "restore\n", 0o644)
	writeTestFile(t, filepath.Join(root, "run/incus-upgrade/active"), "incus-agent.service\nincus.service\n", 0o644)
	writeTestFile(t, filepath.Join(netDir, "applied.json"), `{"instruction":{"schemaVersion":2,"revision":3,"egress":{"mode":"off"},"groups":[{"key":"p3"}],"bindings":[]}}`, 0o600)
	writeTestFile(t, filepath.Join(netDir, "desired.json"), "{broken-json", 0o600)
	writeTestFile(t, filepath.Join(netDir, "owner"), "incus-network-v1\n", 0o600)
	writeTestFile(t, filepath.Join(netDir, "groups/owner"), "incus-package-egress-v2\n", 0o600)
	writeTestFile(t, filepath.Join(netDir, "network-worker"), "#!/bin/sh\n", 0o755)
	writeTestFile(t, filepath.Join(root, "etc/netns/incus-eg-p3/resolv.conf"), "nameserver 1.1.1.1\n", 0o600)

	var recorded []string
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		recorded = append(recorded, c.name+" "+strings.Join(c.args, " "))
		switch {
		case c.name == "systemctl" && c.args[0] == "start" && c.args[1] == "incus.service":
			// Simulate incusd bringing up unix.socket once started cleanly.
			writeTestFile(t, filepath.Join(root, "var/lib/incus/unix.socket"), "", 0o600)
		case c.name == "ip" && slices.Equal(c.args, []string{"-4", "rule", "show"}):
			return []byte("17001:\tfrom all fwmark 0x420100/0xffff00 lookup 8001\n5210:\tfrom all fwmark 0x80000/0xff0000 lookup 52\n"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"-4", "route", "show", "table", "all"}):
			return []byte("unreachable default table 8001 metric 42760\n"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"-o", "link", "show"}):
			return []byte("2: eth0: <UP>\n3: ipgh1@if2: <UP>\n4: ipg0123456789a@if3: <UP>\n"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"netns", "list"}):
			return []byte("incus-eg-p3 (id: 0)\nother-ns\n"), nil
		case c.name == "nft" && slices.Equal(c.args, []string{"list", "tables"}):
			return []byte("table inet legacy_egress\ntable bridge legacy_package_ports\ntable inet filter\n"), nil
		case c.name == "nft" && slices.Equal(c.args, []string{"list", "table", "inet", "legacy_egress"}):
			return []byte("table inet legacy_egress {\n\tset ready {\n\t\ttype mark\n\t}\n}\n"), nil
		case c.name == "nft" && slices.Equal(c.args, []string{"list", "table", "bridge", "legacy_package_ports"}):
			return []byte(`iifname "ipg*" drop`), nil
		case c.name == "ps":
			return []byte("123 dnsmasq --interface=incusbr0 --pid-file\n456 xray run -config /var/lib/incus/agent/network/groups/p3/xray.json\n789 xray run -config /etc/xray/user.json\n"), nil
		}
		return nil, nil
	}}

	b := &backend{runner: f, rootDir: root, logs: &operationLog{}}
	if err := b.recoverBlockedServices(context.Background()); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(recorded, "\n")
	for _, want := range []string{
		"kill -TERM 123",
		"kill -TERM 456",
		"ip -4 rule del priority 17001 fwmark 0x420100/0xffff00 lookup 8001",
		"ip -4 route flush table 8001",
		"ip link del ipgh1",
		"ip netns del incus-eg-p3",
		"nft delete table inet legacy_egress",
		"nft delete table bridge legacy_package_ports",
		"systemctl start incus-network.service",
		"systemctl start incus.service",
		"systemctl start incus-agent.service",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("recovery missed %q\ncommands:\n%s", want, joined)
		}
	}
	for _, forbid := range []string{"5210", "inet filter", "ipg0123456789a", "other-ns", "789"} {
		if strings.Contains(joined, forbid) {
			t.Fatalf("recovery touched unrelated resource %q\ncommands:\n%s", forbid, joined)
		}
	}

	// Preserve valid applied.json, containers, config.yaml; remove orphan groups/owner & corrupt desired.json; clear phase=restore checkpoint.
	if _, err := os.Stat(filepath.Join(netDir, "applied.json")); err != nil {
		t.Fatal("valid applied.json should be preserved")
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/incus/containers/c1/metadata.yaml")); err != nil {
		t.Fatal("instance data must be preserved")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/incus/agent/config.yaml")); err != nil {
		t.Fatal("agent config must be preserved")
	}
	if _, err := os.Stat(filepath.Join(netDir, "groups/owner")); !os.IsNotExist(err) {
		t.Fatal("orphan groups/owner should be cleared")
	}
	if _, err := os.Stat(filepath.Join(netDir, "desired.json")); !os.IsNotExist(err) {
		t.Fatal("corrupt desired.json should be cleared")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/netns/incus-eg-p3")); !os.IsNotExist(err) {
		t.Fatal("egress netns dir should be cleaned")
	}
	if _, err := os.Stat(filepath.Join(root, "run/incus-upgrade")); !os.IsNotExist(err) {
		t.Fatal("phase=restore checkpoint should be cleaned after resuming services")
	}
}

func TestRecoverRefusesMidUpgradeNonRestorePhase(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "run/incus-upgrade/phase"), "install\n", 0o644)
	b := &backend{runner: &fakeRunner{}, rootDir: root, logs: &operationLog{}}
	err := b.recoverBlockedServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "phase=install") {
		t.Fatalf("expected non-restore phase refusal, got %v", err)
	}
}

func TestClearNetworkLockoutRemovesEgressLockAndPreservesInstances(t *testing.T) {
	root := t.TempDir()
	netDir := filepath.Join(root, "var/lib/incus/agent/network")
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-network.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-egress-p3.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus.service.d/incus-network.conf"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/incus/agent/config.yaml"), "panel: https://panel.example.com\n", 0o600)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/containers/c1/metadata.yaml"), "name: c1\n", 0o644)
	writeTestFile(t, filepath.Join(netDir, "applied.json"), "{corrupt", 0o600)
	writeTestFile(t, filepath.Join(netDir, "groups/registry.json"), "{}", 0o600)
	writeTestFile(t, filepath.Join(netDir, "network-worker"), "#!/bin/sh\n", 0o755)

	b := &backend{runner: &fakeRunner{}, rootDir: root, logs: &operationLog{}}
	if err := b.clearNetworkLockout(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{
		"etc/systemd/system/incus-network.service",
		"etc/systemd/system/incus-egress-p3.service",
		"etc/systemd/system/incus.service.d/incus-network.conf",
		"var/lib/incus/agent/network/applied.json",
		"var/lib/incus/agent/network/groups",
	} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be deleted", gone)
		}
	}
	for _, kept := range []string{
		"etc/incus/agent/config.yaml",
		"var/lib/incus/containers/c1/metadata.yaml",
		"var/lib/incus/agent/network/network-worker",
	} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Fatalf("expected %s to remain: %v", kept, err)
		}
	}
}

func TestOfflineResetWorksWithoutIncusSocket(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc/incus/agent/config.yaml"), "panel: https://panel.example.com\n", 0o600)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-egress-p9.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus.service.d/incus-network.conf"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/netns/incus-eg-p9/resolv.conf"), "nameserver 1.1.1.1\n", 0o600)
	writeTestFile(t, filepath.Join(root, "etc/netns/custom-keep/resolv.conf"), "nameserver 9.9.9.9\n", 0o600)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/agent/network/applied.json"), `{}`, 0o600)
	writeTestFile(t, filepath.Join(root, "run/incus-upgrade/phase"), "restore\n", 0o644)

	f := &fakeRunner{}
	b := &backend{runner: f, rootDir: root, path: filepath.Join(root, "etc/incus/agent/config.yaml"), logs: &operationLog{}}
	if err := b.reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{
		"etc/incus/agent/config.yaml",
		"etc/systemd/system/incus-egress-p9.service",
		"etc/systemd/system/incus.service.d/incus-network.conf",
		"etc/netns/incus-eg-p9",
		"var/lib/incus/agent/network/applied.json",
		"run/incus-upgrade",
	} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("offline reset left %s behind", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc/netns/custom-keep/resolv.conf")); err != nil {
		t.Fatalf("offline reset must preserve unrelated /etc/netns directories: %v", err)
	}
}

func TestRepairBridgeOfflinePreflightAndRepairMenuConfirmation(t *testing.T) {
	root := t.TempDir()
	b := &backend{runner: &fakeRunner{}, rootDir: root, client: &http.Client{}, logs: &operationLog{}}
	err := b.repairBridge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unix.socket") {
		t.Fatalf("expected offline preflight error when socket missing, got %v", err)
	}

	cursorWhere := func(action string) int {
		for i, item := range repairMenu {
			if item.action == action {
				return i
			}
		}
		return -1
	}

	for _, action := range []string{"repair-recover", "repair-reset-network"} {
		m := model{backend: b, menuGroup: "repair", cursor: cursorWhere(action)}
		updated, cmd := m.selectMenu()
		m = asModel(t, updated)
		if cmd != nil || m.page != "confirm" || m.confirm == nil {
			t.Fatalf("%s bypassed confirmation", action)
		}
		updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
		m = asModel(t, updated)
		if cmd != nil || m.confirm != nil {
			t.Fatalf("%s cancel still retained confirm", action)
		}
	}

	m := model{backend: b, menuGroup: "repair", cursor: cursorWhere("repair-diagnose")}
	updated, cmd := m.selectMenu()
	m = asModel(t, updated)
	if cmd == nil || !m.busy || m.page == "confirm" {
		t.Fatal("read-only diagnose should run immediately without confirmation")
	}
	if m.cancel != nil {
		m.cancel()
	}
}

func TestDiagnoseTreatsActiveEgressAsHealthyAndFlagsOrphanProcesses(t *testing.T) {
	root := t.TempDir()
	netDir := filepath.Join(root, "var/lib/incus/agent/network")
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-network.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "etc/systemd/system/incus-egress-p3.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(root, "var/lib/incus/unix.socket"), "", 0o600)
	writeTestFile(t, filepath.Join(netDir, "applied.json"), `{"instruction":{"schemaVersion":2,"revision":3,"egress":{"mode":"off"},"groups":[{"key":"p3"}],"bindings":[]}}`, 0o600)
	writeTestFile(t, filepath.Join(netDir, "owner"), "incus-network-v1\n", 0o600)
	writeTestFile(t, filepath.Join(netDir, "groups/owner"), "incus-package-egress-v2\n", 0o600)
	writeTestFile(t, filepath.Join(netDir, "groups/registry.json"), `{"p3":{"key":"p3","slot":1}}`, 0o600)
	writeTestFile(t, filepath.Join(netDir, "network-worker"), "#!/bin/sh\n", 0o755)
	// Managed child PIDs sit inside their unit cgroup; PID 999 is an orphan outside incus.service.
	writeTestFile(t, filepath.Join(root, "proc/101/cgroup"), "0::/system.slice/incus.service\n", 0o444)
	writeTestFile(t, filepath.Join(root, "proc/202/cgroup"), "0::/system.slice/incus-egress-p3.service\n", 0o444)
	writeTestFile(t, filepath.Join(root, "proc/999/cgroup"), "0::/system.slice/user-0.slice\n", 0o444)

	psOut := "101 dnsmasq --interface=incusbr0\n202 xray-v26 /var/lib/incus/agent/network/xray-v26 run -config /var/lib/incus/agent/network/groups/p3/xray.json\n"
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		switch {
		case c.name == "systemctl" && len(c.args) > 1 && c.args[0] == "show":
			unit := c.args[1]
			switch unit {
			case "incus.service", "incus-agent.service", "incus-network.service", "incus-egress-p3.service":
				return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nUnitFileState=enabled"), nil
			default:
				return []byte("LoadState=not-found\nActiveState=inactive"), nil
			}
		case c.name == "ip" && slices.Equal(c.args, []string{"-4", "rule", "show"}):
			return []byte("1001:\tfrom all fwmark 0x420100/0xffff00 lookup 8001\n"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"-4", "route", "show", "table", "all"}):
			return []byte("default via 169.254.244.6 dev ipgh1 table 8001 metric 10 onlink\n"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"-o", "link", "show"}):
			return []byte("3: ipgh1@if2: <UP>\n"), nil
		case c.name == "ip" && slices.Equal(c.args, []string{"netns", "list"}):
			return []byte("incus-eg-p3 (id: 0)\n"), nil
		case c.name == "nft" && slices.Equal(c.args, []string{"list", "tables"}):
			return []byte("table inet incus_egress\ntable bridge incus_package_ports\ntable inet incus_package_egress\n"), nil
		case c.name == "ps":
			return []byte(psOut), nil
		}
		return nil, nil
	}}

	b := &backend{runner: f, rootDir: root, logs: &operationLog{}}
	lines, err := b.diagnoseNode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := strings.Join(lines, "\n")
	if !strings.Contains(report, "当前活动出口资源:") || !strings.Contains(report, "托管进程(dnsmasq=1, xray=1)") || !strings.Contains(report, "结论: 未发现明显阻塞残留") {
		t.Fatalf("healthy active node misclassified as residue:\n%s", report)
	}
	if strings.Contains(report, "发现问题:") {
		t.Fatalf("expected zero issues on healthy active node:\n%s", report)
	}

	psOut += "999 dnsmasq --interface=incusbr0\n"
	lines, err = b.diagnoseNode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report = strings.Join(lines, "\n")
	if !strings.Contains(report, "残留进程(dnsmasq=1, xray=0)") || !strings.Contains(report, "存在未受 systemd 托管的残留进程 (dnsmasq/xray)") {
		t.Fatalf("orphan out-of-cgroup dnsmasq was not flagged:\n%s", report)
	}
}
