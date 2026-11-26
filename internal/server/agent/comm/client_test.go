package comm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/protocol"
	"github.com/lxc/incus/v7/internal/server/agent/tunnel"
)

type mockWSTransport struct {
	methods    map[string]bool
	call       func(ctx context.Context, method string, params any) (tunnel.RPCResponseEnvelope, error)
	probeCount atomic.Int32
}

func (m *mockWSTransport) SupportsRPCMethod(method string) bool {
	return m != nil && m.methods[method]
}

func (m *mockWSTransport) CallRPC(ctx context.Context, method string, params any) (tunnel.RPCResponseEnvelope, error) {
	return m.call(ctx, method, params)
}

func (m *mockWSTransport) Probe() {
	m.probeCount.Add(1)
}

func TestCommClientRejectsMissingEndpointContract(t *testing.T) {
	client := New(Config{
		BaseURL:        "http://127.0.0.1:1",
		AgentID:        "agent-1",
		AgentSecret:    "secret-1",
		RequestTimeout: time.Second,
	})

	if _, err := client.Call(context.Background(), Endpoint{HTTPMethod: http.MethodPost, HTTPPath: "/api/agent/x"}, nil); err == nil {
		t.Fatal("expected error when RPCMethod is empty")
	}
	if _, err := client.Call(context.Background(), Endpoint{RPCMethod: "agent.x", HTTPMethod: http.MethodPost}, nil); err == nil {
		t.Fatal("expected error when HTTPPath is empty")
	}
}

func TestCommClientAutoUpgradesAndFallsBackAcrossAllReasons(t *testing.T) {
	var httpCalls atomic.Int32
	const secret = "comm-test-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		hash := protocol.BodySHA256(body)
		expected := protocol.Signature(secret, protocol.SigningPayload(r.Method, r.URL.Path, r.Header.Get("x-incus-timestamp"), r.Header.Get("x-incus-nonce"), hash))
		if r.Header.Get("x-incus-signature") != expected {
			t.Errorf("invalid hmac signature")
		}
		_, _ = w.Write([]byte(`{"ok":true,"via":"http"}`))
	}))
	defer server.Close()

	var mu sync.Mutex
	var transitions []string
	client := New(Config{
		BaseURL:        server.URL,
		AgentID:        "agent-comm",
		AgentSecret:    secret,
		RequestTimeout: time.Second,
		OnTransportChange: func(endpoint, from, to, reason string) {
			mu.Lock()
			transitions = append(transitions, endpoint+":"+from+"->"+to+":"+reason)
			mu.Unlock()
		},
	})

	ep := Endpoint{
		RPCMethod:  "agent.custom.report",
		HTTPMethod: http.MethodPost,
		HTTPPath:   "/api/agent/custom-report",
		WSTimeout:  time.Second,
	}

	// 1. No WS transport -> HTTP
	res, err := client.Call(context.Background(), ep, map[string]any{"x": 1})
	if err != nil || res.Transport != TransportHTTP {
		t.Fatalf("expected HTTP before WS attach, res=%+v err=%v", res, err)
	}

	// 2. Attach negotiated WS -> upgrades automatically
	ws := &mockWSTransport{
		methods: map[string]bool{"agent.custom.report": true},
		call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
			return tunnel.RPCResponseEnvelope{
				V:      tunnel.RPCProtocolVersion,
				OK:     true,
				Status: http.StatusOK,
				Data:   json.RawMessage(`{"ok":true,"via":"ws"}`),
			}, nil
		},
	}
	client.SetWSTransport(ws)

	res, err = client.Call(context.Background(), ep, map[string]any{"x": 2})
	if err != nil || res.Transport != TransportWS || !strings.Contains(string(res.Body), `"via":"ws"`) {
		t.Fatalf("expected WS upgrade, res=%+v err=%v", res, err)
	}
	if httpCalls.Load() != 1 {
		t.Fatalf("expected still 1 http call, got %d", httpCalls.Load())
	}

	// 3. Fallback scenarios
	fallbackCases := []struct {
		name       string
		setup      func()
		wantReason string
		wantProbe  int32
	}{
		{
			name:       "tunnel_unavailable",
			setup:      func() { ws.methods["agent.custom.report"] = false },
			wantReason: "tunnel_unavailable",
		},
		{
			name: "rpc_method_not_found",
			setup: func() {
				ws.methods["agent.custom.report"] = true
				ws.call = func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{V: 1, Status: http.StatusNotFound, Code: "RPC_METHOD_NOT_FOUND"}, nil
				}
			},
			wantReason: "rpc_method_not_found",
		},
		{
			name: "http_501",
			setup: func() {
				ws.call = func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{V: 1, Status: http.StatusNotImplemented}, nil
				}
			},
			wantReason: "rpc_method_not_found",
		},
		{
			name: "payload_too_large_remote",
			setup: func() {
				ws.call = func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{V: 1, Status: http.StatusRequestEntityTooLarge, Code: "PAYLOAD_TOO_LARGE"}, nil
				}
			},
			wantReason: "payload_too_large",
		},
		{
			name: "payload_too_large_local",
			setup: func() {
				ws.call = func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{}, tunnel.ErrPayloadTooLarge
				}
			},
			wantReason: "payload_too_large",
		},
		{
			name: "ws_disconnected",
			setup: func() {
				ws.call = func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{}, tunnel.ErrTunnelClosed
				}
			},
			wantReason: "ws_disconnected",
		},
		{
			name: "ws_timeout_probes",
			setup: func() {
				ws.call = func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
					return tunnel.RPCResponseEnvelope{}, context.DeadlineExceeded
				}
			},
			wantReason: "ws_timeout",
			wantProbe:  1,
		},
	}

	for _, tc := range fallbackCases {
		t.Run(tc.name, func(t *testing.T) {
			beforeProbe := ws.probeCount.Load()
			tc.setup()
			fbRes, fbErr := client.Call(context.Background(), ep, map[string]any{"x": 3})
			if fbErr != nil || fbRes.Transport != TransportHTTP || fbRes.FallbackReason != tc.wantReason {
				t.Fatalf("res=%+v err=%v, want transport=http reason=%s", fbRes, fbErr, tc.wantReason)
			}
			if ws.probeCount.Load()-beforeProbe != tc.wantProbe {
				t.Fatalf("probe delta=%d, want %d", ws.probeCount.Load()-beforeProbe, tc.wantProbe)
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(transitions) < 3 {
		t.Fatalf("expected transport transitions recorded, got %v", transitions)
	}
}

func TestCommClientAuthoritativeWSErrorDoesNotRetryHTTP(t *testing.T) {
	var httpCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(Config{
		BaseURL:        server.URL,
		AgentID:        "agent-auth",
		AgentSecret:    "secret",
		RequestTimeout: time.Second,
	})
	client.SetWSTransport(&mockWSTransport{
		methods: map[string]bool{"agent.security.report": true},
		call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
			return tunnel.RPCResponseEnvelope{
				V:      1,
				OK:     false,
				Status: http.StatusServiceUnavailable,
				Code:   "REPORT_UNAVAILABLE",
				Error:  "Report unavailable",
			}, nil
		},
	})

	res, err := client.Call(context.Background(), Endpoint{
		RPCMethod:  "agent.security.report",
		HTTPMethod: http.MethodPost,
		HTTPPath:   "/api/agent/security-report",
	}, map[string]any{})
	if err == nil {
		t.Fatal("expected authoritative 503 error")
	}
	if httpCalls.Load() != 0 {
		t.Fatalf("expected 0 HTTP calls on authoritative 503, got %d", httpCalls.Load())
	}
	if res.StatusCode != http.StatusServiceUnavailable || res.Transport != TransportWS {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestCommClientHonorsParentContextCancelAndConcurrency(t *testing.T) {
	var httpCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := New(Config{
		BaseURL:        server.URL,
		AgentID:        "agent-conc",
		AgentSecret:    "secret",
		RequestTimeout: time.Second,
	})
	client.SetWSTransport(&mockWSTransport{
		methods: map[string]bool{"agent.heartbeat": true},
		call: func(ctx context.Context, _ string, _ any) (tunnel.RPCResponseEnvelope, error) {
			<-ctx.Done()
			return tunnel.RPCResponseEnvelope{}, ctx.Err()
		},
	})

	parentCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Call(parentCtx, Endpoint{
		RPCMethod:  "agent.heartbeat",
		HTTPMethod: http.MethodPost,
		HTTPPath:   "/api/agent/heartbeat",
	}, map[string]any{})
	if err == nil || httpCalls.Load() != 0 {
		t.Fatalf("parent cancellation should not retry HTTP, err=%v httpCalls=%d", err, httpCalls.Load())
	}

	// Race test concurrent SetWSTransport + Call.
	client.SetWSTransport(&mockWSTransport{
		methods: map[string]bool{"agent.heartbeat": true},
		call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
			return tunnel.RPCResponseEnvelope{V: 1, OK: true, Status: 200, Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%4 == 0 {
				client.SetWSTransport(&mockWSTransport{
					methods: map[string]bool{"agent.heartbeat": true},
					call: func(context.Context, string, any) (tunnel.RPCResponseEnvelope, error) {
						return tunnel.RPCResponseEnvelope{V: 1, OK: true, Status: 200, Data: json.RawMessage(`{"ok":true}`)}, nil
					},
				})
			}
			res, callErr := client.Call(context.Background(), Endpoint{
				RPCMethod:  "agent.heartbeat",
				HTTPMethod: http.MethodPost,
				HTTPPath:   "/api/agent/heartbeat",
			}, map[string]any{"i": idx})
			if callErr != nil || !res.OK {
				t.Errorf("concurrent call failed: res=%+v err=%v", res, callErr)
			}
		}(i)
	}
	wg.Wait()
}
