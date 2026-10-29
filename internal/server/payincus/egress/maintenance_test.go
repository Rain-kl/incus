package egress

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXrayInspectReturnsOnlyNodeSummaries(t *testing.T) {
	m := newManager(t.TempDir())
	secret := "fixture-proxy-password"
	outbound := json.RawMessage(`{"protocol":"socks","settings":{"servers":[{"address":"proxy.example","port":1080,"users":[{"user":"fixture-user","pass":"` + secret + `"}]}]}}`)
	m.desired = &Instruction{
		Nodes:  []Node{{ID: 4, Outbound: outbound}},
		Groups: []EgressGroup{{Key: "p9", Nodes: []Node{{ID: 4, Outbound: outbound}, {ID: 5, Outbound: json.RawMessage(`{"protocol":"vless","settings":{"vnext":[{"address":"vless.example","port":443,"users":[{"id":"fixture-id"}]}]}}`)}}}},
	}
	encoded, err := json.Marshal(m.Inspect())
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, secret) || strings.Contains(text, "fixture-user") || strings.Contains(text, "fixture-id") {
		t.Fatalf("Xray inspection disclosed credentials: %s", text)
	}
	var result struct {
		Nodes []NodeSummary `json:"nodes"`
	}
	err = json.Unmarshal(encoded, &result)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 || result.Nodes[0] != (NodeSummary{ID: 4, Protocol: "socks", Address: "proxy.example", Port: 1080}) || result.Nodes[1] != (NodeSummary{ID: 5, Protocol: "vless", Address: "vless.example", Port: 443}) {
		t.Fatalf("inspection did not summarize/deduplicate configured nodes: %#v", result.Nodes)
	}
}

func TestXrayMaintenanceRejectsUnsafeStatesWithoutHostCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := newManager(t.TempDir())
	_, err := m.TestConfigured(context.Background())
	if err != nil {
		t.Fatalf("empty configured-node probe should return an empty result: %v", err)
	}
	_, err = m.Maintain(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "no network policy") {
		t.Fatalf("maintenance without policy was accepted: %v", err)
	}
	m.desired = &Instruction{Revision: 1, Egress: Policy{Mode: "off"}}
	_, err = m.Maintain(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("canceled maintenance was accepted: %v", err)
	}
	_, err = m.Maintain(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "ownership unavailable") {
		t.Fatalf("unowned maintenance directory was accepted: %v", err)
	}
	m.applyGate <- struct{}{}
	_, err = m.Maintain(context.Background(), false)
	<-m.applyGate
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("maintenance bypassed the shared apply gate: %v", err)
	}

	m = newManager(t.TempDir())
	m.desired = &Instruction{Revision: 1, Egress: Policy{Mode: "off"}}
	err = os.WriteFile(filepath.Join(m.dir, "owner"), []byte("incudal-network-v1\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(m.dir, "groups"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(m.dir, "groups", "registry.json"), []byte("{"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Maintain(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "invalid resource registry") {
		t.Fatalf("malformed runtime registry reached host commands: %v", err)
	}
}

func TestMaintenanceChecksAndStopsLegacyUnitEvenWhenPolicyIsOff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		loadState string
		wantStop  bool
	}{
		{name: "loaded", loadState: "loaded", wantStop: true},
		{name: "missing", loadState: "not-found", wantStop: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeErr := os.WriteFile(filepath.Join(dir, "owner"), []byte("incudal-network-v1\n"), 0o600)
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			fakeBin := filepath.Join(dir, "bin")
			writeErr = os.Mkdir(fakeBin, 0o700)
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			logPath := filepath.Join(dir, "commands.log")
			body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MAINTENANCE_LOG\"\ncase \"$1\" in\nshow) printf '%s\\n' '" + tc.loadState + "' ;;\nstop) exit 0 ;;\nesac\n"
			writeErr = os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(body), 0o700)
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			t.Setenv("MAINTENANCE_LOG", logPath)
			t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			m := newManager(dir)
			m.desired = &Instruction{Egress: Policy{Mode: "off"}}
			_, err := m.Maintain(context.Background(), false)
			if err == nil {
				t.Fatal("revision-zero policy should stop before applying; fixture unexpectedly reported success")
			}
			commands, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			text := string(commands)
			if !strings.Contains(text, "show --property=LoadState --value incudal-xray.service") {
				t.Fatalf("legacy unit LoadState was not checked with policy off: %s", text)
			}
			if strings.Contains(text, "stop incudal-xray.service") != tc.wantStop {
				t.Fatalf("legacy unit stop decision disagreed with LoadState=%q: %s", tc.loadState, text)
			}
		})
	}
}
