package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/config"
	"github.com/lxc/incus/v7/internal/server/agent/tunnel"
)

type mockRPCCaller struct {
	methods    map[string]bool
	call       func(ctx context.Context, method string, params any) (tunnel.RPCResponseEnvelope, error)
	probeCount atomic.Int32
}

func (m *mockRPCCaller) SupportsRPCMethod(method string) bool {
	return m != nil && m.methods[method]
}

func (m *mockRPCCaller) CallRPC(ctx context.Context, method string, params any) (tunnel.RPCResponseEnvelope, error) {
	return m.call(ctx, method, params)
}

func (m *mockRPCCaller) Probe() {
	m.probeCount.Add(1)
}

func TestHeartbeatPrefersWSTunnelWhenNegotiated(t *testing.T) {
	var httpCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"tunnel":{"enabled":false}}`))
	}))
	defer server.Close()

	client := New(config.Config{
		PanelURL:       server.URL,
		AgentID:        "agent-ws",
		AgentSecret:    "secret-ws",
		RequestTimeout: time.Second,
	})
	caller := &mockRPCCaller{
		methods: map[string]bool{"agent.heartbeat": true},
		call: func(_ context.Context, method string, _ any) (tunnel.RPCResponseEnvelope, error) {
			if method != "agent.heartbeat" {
				t.Fatalf("unexpected method: %s", method)
			}
			return tunnel.RPCResponseEnvelope{
				V:      tunnel.RPCProtocolVersion,
				OK:     true,
				Status: http.StatusOK,
				Data:   json.RawMessage(`{"ok":true,"tunnel":{"enabled":true,"targetHost":"127.0.0.1","targetPort":8443},"caddy":{"command":"install","port":2019}}`),
			}, nil
		},
	}
	client.SetTunnelTransport(caller)

	res, err := client.Heartbeat(context.Background(), map[string]any{"version": "2.0.1"})
	if err != nil {
		t.Fatalf("heartbeat failed: %v", err)
	}
	if httpCalls.Load() != 0 {
		t.Fatalf("expected 0 HTTP calls when WS succeeded, got %d", httpCalls.Load())
	}
	if res.Transport != TransportWS || !res.OK || res.Tunnel == nil || !res.Tunnel.Enabled {
		t.Fatalf("unexpected ws heartbeat result: %+v", res)
	}
	if res.Caddy == nil || res.Caddy.Command != "install" || res.Caddy.Port != 2019 {
		t.Fatalf("unexpected caddy instruction: %+v", res.Caddy)
	}
	var decoded map[string]any
	err = json.Unmarshal([]byte(res.Body), &decoded)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_, hasLatency := decoded["latencyMs"]
	if !hasLatency {
		t.Fatalf("expected latencyMs injected in body, got %s", res.Body)
	}
}

func TestHeartbeatFallbacksToHTTP(t *testing.T) {
	cases := []struct {
		name           string
		caller         *mockRPCCaller
		wantReason     string
		wantProbeCount int32
	}{
		{
			name: "tunnel_unavailable",
			caller: &mockRPCCaller{
				methods: map[string]bool{},
			},
			wantReason: "tunnel_unavailable",
		},
		{
			name: "rpc_method_not_found",
			caller: &mockRPCCaller{
				methods: map[string]bool{"agent.heartbeat": true},
				call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{
						V:      tunnel.RPCProtocolVersion,
						OK:     false,
						Status: http.StatusNotFound,
						Code:   "RPC_METHOD_NOT_FOUND",
					}, nil
				},
			},
			wantReason: "rpc_method_not_found",
		},
		{
			name: "http_501_not_implemented",
			caller: &mockRPCCaller{
				methods: map[string]bool{"agent.heartbeat": true},
				call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{
						V:      tunnel.RPCProtocolVersion,
						OK:     false,
						Status: http.StatusNotImplemented,
					}, nil
				},
			},
			wantReason: "rpc_method_not_found",
		},
		{
			name: "payload_too_large_local",
			caller: &mockRPCCaller{
				methods: map[string]bool{"agent.heartbeat": true},
				call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{}, tunnel.ErrPayloadTooLarge
				},
			},
			wantReason: "payload_too_large",
		},
		{
			name: "payload_too_large_remote",
			caller: &mockRPCCaller{
				methods: map[string]bool{"agent.heartbeat": true},
				call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{
						V:      tunnel.RPCProtocolVersion,
						OK:     false,
						Status: http.StatusRequestEntityTooLarge,
						Code:   "PAYLOAD_TOO_LARGE",
					}, nil
				},
			},
			wantReason: "payload_too_large",
		},
		{
			name: "ws_disconnected",
			caller: &mockRPCCaller{
				methods: map[string]bool{"agent.heartbeat": true},
				call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{}, tunnel.ErrTunnelClosed
				},
			},
			wantReason: "ws_disconnected",
		},
		{
			name: "ws_timeout_triggers_probe",
			caller: &mockRPCCaller{
				methods: map[string]bool{"agent.heartbeat": true},
				call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{}, context.DeadlineExceeded
				},
			},
			wantReason:     "ws_timeout",
			wantProbeCount: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var httpCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpCalls.Add(1)
				_, _ = w.Write([]byte(`{"ok":true,"tunnel":{"enabled":true,"targetHost":"127.0.0.1","targetPort":8443}}`))
			}))
			defer server.Close()

			client := New(config.Config{
				PanelURL:       server.URL,
				AgentID:        "agent-fb",
				AgentSecret:    "secret-fb",
				RequestTimeout: time.Second,
			})
			client.SetTunnelTransport(tc.caller)

			res, err := client.Heartbeat(context.Background(), map[string]any{"version": "2.0.1"})
			if err != nil {
				t.Fatalf("expected HTTP fallback success, got %v", err)
			}
			if httpCalls.Load() != 1 {
				t.Fatalf("expected 1 HTTP fallback call, got %d", httpCalls.Load())
			}
			if res.Transport != TransportHTTP || res.FallbackReason != tc.wantReason {
				t.Fatalf("transport=%q reason=%q, want http / %q", res.Transport, res.FallbackReason, tc.wantReason)
			}
			if tc.caller.probeCount.Load() != tc.wantProbeCount {
				t.Fatalf("probeCount=%d, want %d", tc.caller.probeCount.Load(), tc.wantProbeCount)
			}
		})
	}
}

func TestHeartbeatAuthoritativeWSErrorDoesNotRetryHTTP(t *testing.T) {
	var httpCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(config.Config{
		PanelURL:       server.URL,
		AgentID:        "agent-auth",
		AgentSecret:    "secret-auth",
		RequestTimeout: time.Second,
	})
	client.SetTunnelTransport(&mockRPCCaller{
		methods: map[string]bool{"agent.heartbeat": true},
		call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
			return tunnel.RPCResponseEnvelope{
				V:      tunnel.RPCProtocolVersion,
				OK:     false,
				Status: http.StatusConflict,
				Code:   "NODE_API_REQUIRED",
				Error:  "Install the Incus node package",
				Data:   json.RawMessage(`{"code":"NODE_API_REQUIRED","error":"Install the Incus node package"}`),
			}, nil
		},
	})

	res, err := client.Heartbeat(context.Background(), map[string]any{"version": "2.0.1"})
	if err == nil {
		t.Fatal("expected error for authoritative 409 response")
	}
	if httpCalls.Load() != 0 {
		t.Fatalf("expected 0 HTTP calls on authoritative 409, got %d", httpCalls.Load())
	}
	if res.StatusCode != http.StatusConflict || res.Transport != TransportWS || !strings.Contains(res.Body, "NODE_API_REQUIRED") {
		t.Fatalf("unexpected authoritative error result: %+v", res)
	}
}

func TestCallEndpointSupportsCustomEndpoints(t *testing.T) {
	var gotHTTPMethod, gotHTTPPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHTTPMethod = r.Method
		gotHTTPPath = r.URL.Path
		_, _ = w.Write([]byte(`{"applied":true,"source":"http"}`))
	}))
	defer server.Close()

	client := New(config.Config{
		PanelURL:       server.URL,
		AgentID:        "agent-custom",
		AgentSecret:    "secret-custom",
		RequestTimeout: time.Second,
	})

	spec := EndpointSpec{
		RPCMethod:  "node.custom.sync",
		HTTPMethod: http.MethodPut,
		HTTPPath:   "/api/agent/custom-sync",
		WSTimeout:  time.Second,
	}

	caller := &mockRPCCaller{
		methods: map[string]bool{"node.custom.sync": true},
		call: func(_ context.Context, method string, params any) (tunnel.RPCResponseEnvelope, error) {
			payload, _ := json.Marshal(map[string]any{"method": method, "params": params, "source": "ws"})
			return tunnel.RPCResponseEnvelope{
				V:      tunnel.RPCProtocolVersion,
				OK:     true,
				Status: http.StatusOK,
				Data:   payload,
			}, nil
		},
	}
	client.SetTunnelTransport(caller)

	res, err := client.CallEndpoint(context.Background(), spec, map[string]any{"rev": 9})
	if err != nil || res.Transport != TransportWS || !strings.Contains(res.Body, `"source":"ws"`) {
		t.Fatalf("expected WS execution for custom endpoint, res=%+v err=%v", res, err)
	}

	// Disable custom method on server -> seamless PUT HTTP fallback.
	caller.methods["node.custom.sync"] = false
	res, err = client.CallEndpoint(context.Background(), spec, map[string]any{"rev": 9})
	if err != nil || res.Transport != TransportHTTP || gotHTTPMethod != http.MethodPut || gotHTTPPath != "/api/agent/custom-sync" {
		t.Fatalf("expected HTTP PUT fallback for custom endpoint, res=%+v method=%s path=%s err=%v", res, gotHTTPMethod, gotHTTPPath, err)
	}
}
