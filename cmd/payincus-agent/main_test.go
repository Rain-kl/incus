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
				RequestID    string   `json:"heartbeatRequestId"`
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
			if body.RequestID != "1e862c0a-0316-4a53-9c5f-a6be04ba2534" || body.NodeAPI != "payincus_node_v1" || !found || (!locked && !metrics) {
				t.Error("native identity or capabilities missing")
			}
			received = true
			_, _ = w.Write([]byte(`{"ok":true,"tunnel":{"enabled":false}}`))
		}))
		cfg := config.Config{PanelURL: control.URL, AgentID: "fixture-agent", AgentSecret: "fixture-secret", HeartbeatIntervalSeconds: 60}
		worker := tunnel.NewWorker(tunnel.WorkerConfig{PanelURL: control.URL})
		var pending atomic.Bool
		err = heartbeat(context.Background(), nodeclient.New(socket), panel.New(cfg), cfg, worker, &pending, "1e862c0a-0316-4a53-9c5f-a6be04ba2534")
		control.Close()
		_ = localServer.Close()
		if err != nil || !received {
			t.Fatal("heartbeat failed", err)
		}
	}
}

func TestHeartbeatReportsDaemonFailureWithoutDroppingConnection(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	localServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) })}
	go func() { _ = localServer.Serve(listener) }()
	defer localServer.Close()
	received := false
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Incus   map[string]any `json:"incus"`
			Storage map[string]any `json:"storage"`
			NodeAPI string         `json:"nodeApi"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Incus["available"] != false || body.Storage["available"] != false || body.NodeAPI != "payincus_node_v1" {
			t.Error("missing failure heartbeat")
		}
		received = true
		_, _ = w.Write([]byte(`{"ok":true,"tunnel":{"enabled":false}}`))
	}))
	defer control.Close()
	cfg := config.Config{PanelURL: control.URL, AgentID: "fixture-agent", AgentSecret: "fixture-secret", HeartbeatIntervalSeconds: 10}
	worker := tunnel.NewWorker(tunnel.WorkerConfig{PanelURL: control.URL})
	var pending atomic.Bool
	if heartbeat(context.Background(), nodeclient.New(socket), panel.New(cfg), cfg, worker, &pending, "") == nil || !received {
		t.Fatal("daemon failure suppressed the node heartbeat")
	}
}
