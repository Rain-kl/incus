package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/lxc/incus/v7/internal/server/payincus/config"
	"github.com/lxc/incus/v7/internal/server/payincus/nodeclient"
	"github.com/lxc/incus/v7/internal/server/payincus/panel"
	"github.com/lxc/incus/v7/internal/server/payincus/tunnel"
)

func TestNativeHeartbeatContractIncludesLockedNodes(t *testing.T) {
	for _, locked := range []bool{false, true} {
		socket := filepath.Join(t.TempDir(), "incus.socket")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		localServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			value := map[string]any{"api_extensions": []string{"payincus_node_v1"}}
			if r.URL.Path == "/1.0/payincus/report" {
				value = map[string]any{"nodeSecurity": map[string]any{"state": map[string]any{"locked": locked}}}
				if !locked {
					value["capabilities"] = []string{"host-metrics"}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": value})
		})}
		go func() { _ = localServer.Serve(listener) }()
		received := false
		control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				NodeAPI      string   `json:"nodeApi"`
				Capabilities []string `json:"capabilities"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid heartbeat body")
			}
			found := false
			metrics := false
			for _, capability := range body.Capabilities {
				if capability == "payincus_node_v1" {
					found = true
				}
				if capability == "host-metrics" {
					metrics = true
				}
			}
			if body.NodeAPI != "payincus_node_v1" || !found || (!locked && !metrics) {
				t.Error("native identity or capabilities missing")
			}
			received = true
			_, _ = w.Write([]byte(`{"ok":true,"tunnel":{"enabled":false}}`))
		}))
		cfg := config.Config{PanelURL: control.URL, AgentID: "fixture-agent", AgentSecret: "fixture-secret", HeartbeatIntervalSeconds: 60}
		worker := tunnel.NewWorker(tunnel.WorkerConfig{PanelURL: control.URL})
		var pending atomic.Bool
		err = heartbeat(context.Background(), nodeclient.New(socket), panel.New(cfg), cfg, worker, &pending)
		control.Close()
		_ = localServer.Close()
		if err != nil || !received {
			t.Fatal("heartbeat failed", err)
		}
	}
}
