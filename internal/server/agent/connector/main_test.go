package connector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lxc/incus/v7/internal/server/agent/config"
	"github.com/lxc/incus/v7/internal/server/agent/nodeclient"
	"github.com/lxc/incus/v7/internal/server/agent/panel"
	"github.com/lxc/incus/v7/internal/server/agent/tunnel"
)

func TestNativeHeartbeatContractIncludesLockedNodes(t *testing.T) {
	for _, locked := range []bool{false, true} {
		socket := filepath.Join(t.TempDir(), "incus.socket")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		localServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			value := map[string]any{"api_extensions": []string{"incus_node_v1"}}
			if r.URL.Path == "/1.0/agent/report" {
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
				if capability == "incus_node_v1" {
					found = true
				}
				if capability == "host-metrics" {
					metrics = true
				}
			}
			if body.RequestID != "1e862c0a-0316-4a53-9c5f-a6be04ba2534" || body.NodeAPI != "incus_node_v1" || !found || (!locked && !metrics) {
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
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Incus["available"] != false || body.Storage["available"] != false || body.NodeAPI != "incus_node_v1" {
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

func TestConnectorHeartbeatUpgradesToWSTunnelAndFallsBack(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	localServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := map[string]any{"api_extensions": []string{"incus_node_v1"}}
		if r.URL.Path == "/1.0/agent/report" {
			value = map[string]any{
				"incus":     map[string]any{"available": true},
				"instances": map[string]any{"available": true, "items": []any{}},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": value})
	})}
	go func() {
		_ = localServer.Serve(listener)
	}()
	defer localServer.Close()

	var httpHeartbeats atomic.Int32
	var wsHeartbeats atomic.Int32
	var allowWSUpgrade atomic.Bool
	allowWSUpgrade.Store(true)
	wsConns := make(chan *websocket.Conn, 2)

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/heartbeat":
			httpHeartbeats.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"tunnel":{"enabled":true,"targetHost":"127.0.0.1","targetPort":8443}}`))
		case "/api/agent/tunnel":
			if !allowWSUpgrade.Load() {
				http.Error(w, "ws unavailable", http.StatusServiceUnavailable)
				return
			}
			conn, upErr := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if upErr == nil {
				wsConns <- conn
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()

	cfg := config.Config{
		PanelURL:                 control.URL,
		AgentID:                  "e2e-agent",
		AgentSecret:              "e2e-secret",
		HeartbeatIntervalSeconds: 5,
		RequestTimeout:           2 * time.Second,
	}
	local := nodeclient.New(socket)
	remote := panel.New(cfg)
	worker := tunnel.NewWorker(tunnel.WorkerConfig{
		PanelURL:    cfg.PanelURL,
		AgentID:     cfg.AgentID,
		AgentSecret: cfg.AgentSecret,
		NodeClient:  local,
	})
	remote.SetTunnelTransport(worker)
	defer worker.SyncConfig(false, "", 0)

	var pending atomic.Bool

	// 1. First heartbeat runs over HTTP and enables the reverse tunnel.
	res, err := heartbeatRoundTrip(context.Background(), local, remote, cfg, worker, &pending, "")
	if err != nil || res.Transport != panel.TransportHTTP || httpHeartbeats.Load() != 1 {
		t.Fatalf("step 1 expected HTTP heartbeat, res=%+v err=%v httpCount=%d", res, err, httpHeartbeats.Load())
	}

	var conn *websocket.Conn
	select {
	case conn = <-wsConns:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reverse WS tunnel")
	}

	helloPayload, err := json.Marshal(tunnel.ControlHelloPayload{
		Capabilities: []string{tunnel.RPCCapability},
		RPCMethods:   []string{"agent.heartbeat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = conn.WriteMessage(websocket.BinaryMessage, tunnel.EncodeFrame(tunnel.FrameTypeControlHello, 0, helloPayload))
	if err != nil {
		t.Fatal(err)
	}

	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		for {
			msgType, frame, rErr := conn.ReadMessage()
			if rErr != nil {
				return
			}
			if msgType != websocket.BinaryMessage {
				continue
			}
			kind, sID, payload, dErr := tunnel.DecodeFrame(frame)
			if dErr != nil {
				continue
			}
			if kind == tunnel.FrameTypeRPCRequest {
				var req tunnel.RPCRequestEnvelope
				_ = json.Unmarshal(payload, &req)
				if req.Method == "agent.heartbeat" {
					wsHeartbeats.Add(1)
					respBytes, _ := json.Marshal(tunnel.RPCResponseEnvelope{
						V:      tunnel.RPCProtocolVersion,
						OK:     true,
						Status: http.StatusOK,
						Data:   json.RawMessage(`{"ok":true,"tunnel":{"enabled":true,"targetHost":"127.0.0.1","targetPort":8443}}`),
					})
					_ = conn.WriteMessage(websocket.BinaryMessage, tunnel.EncodeFrame(tunnel.FrameTypeRPCResponse, sID, respBytes))
				}
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !worker.SupportsRPCMethod("agent.heartbeat") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !worker.SupportsRPCMethod("agent.heartbeat") {
		t.Fatal("worker did not negotiate agent.heartbeat RPC support")
	}

	// 2. Second heartbeat automatically upgrades to WS tunnel with zero HTTP calls.
	res, err = heartbeatRoundTrip(context.Background(), local, remote, cfg, worker, &pending, "")
	if err != nil || res.Transport != panel.TransportWS || wsHeartbeats.Load() != 1 || httpHeartbeats.Load() != 1 {
		t.Fatalf("step 2 expected WS upgrade, res=%+v err=%v wsCount=%d httpCount=%d",
			res, err, wsHeartbeats.Load(), httpHeartbeats.Load())
	}

	// 3. Close tunnel and block reconnect -> third heartbeat falls back cleanly to HTTP.
	allowWSUpgrade.Store(false)
	_ = conn.Close()
	<-wsDone
	deadline = time.Now().Add(2 * time.Second)
	for worker.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	res, err = heartbeatRoundTrip(context.Background(), local, remote, cfg, worker, &pending, "")
	if err != nil || res.Transport != panel.TransportHTTP || httpHeartbeats.Load() != 2 {
		t.Fatalf("step 3 expected HTTP fallback, res=%+v err=%v httpCount=%d", res, err, httpHeartbeats.Load())
	}
}
