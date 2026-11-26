package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/config"
	"github.com/lxc/incus/v7/internal/server/agent/protocol"
	"github.com/lxc/incus/v7/internal/server/agent/tunnel"
)

func TestSignedHeartbeatContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != heartbeatPath || r.Header.Get("x-incus-agent-id") != "node-test" {
			t.Error("invalid heartbeat route or identity")
		}

		payload := protocol.SigningPayload(r.Method, r.URL.Path, r.Header.Get("x-incus-timestamp"), r.Header.Get("x-incus-nonce"), r.Header.Get("x-incus-body-sha256"))
		if r.Header.Get("x-incus-signature") != protocol.Signature("test-only-secret", payload) {
			t.Error("invalid signature")
		}

		_, _ = w.Write([]byte(`{"tunnel":{"enabled":true,"targetHost":"127.0.0.1","targetPort":8443},"caddy":{"command":"idle"}}`))
	}))
	defer server.Close()
	client := New(config.Config{PanelURL: server.URL, AgentID: "node-test", AgentSecret: "test-only-secret", RequestTimeout: time.Second})
	result, err := client.Heartbeat(context.Background(), map[string]any{"version": "test"})
	if err != nil || result.Tunnel == nil || !result.Tunnel.Enabled || result.Tunnel.TargetPort != 8443 {
		t.Fatal(result, err)
	}
}

func TestHeartbeatDenialAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	client := New(config.Config{PanelURL: server.URL, AgentID: "node-test", AgentSecret: "test-only-secret", RequestTimeout: time.Second})
	_, err := client.Heartbeat(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("accepted denied heartbeat")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Heartbeat(ctx, map[string]any{})
	if err == nil {
		t.Fatal("accepted cancelled heartbeat")
	}
}

func TestSecurityReportSignedContract(t *testing.T) {
	const secret = "fixture-signature-material"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/security-report" || r.Method != "POST" {
			t.Error("report endpoint mismatch")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		hash := protocol.BodySHA256(body)
		expected := protocol.Signature(secret, protocol.SigningPayload("POST", r.URL.Path, r.Header.Get("x-incus-timestamp"), r.Header.Get("x-incus-nonce"), hash))
		if r.Header.Get("x-incus-body-sha256") != hash || r.Header.Get("x-incus-signature") != expected {
			t.Error("unsigned or mismatched report")
		}
		_, _ = w.Write([]byte(`{"ack":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],"recoveryBlocked":true,"recoveryGeneration":11}`))
	}))
	defer server.Close()
	client := New(config.Config{PanelURL: server.URL, AgentID: "fixture", AgentSecret: secret, RequestTimeout: time.Second})
	ids, err := client.SecurityReport(context.Background(), map[string]any{"state": map[string]any{"locked": true, "revision": 3}, "events": []any{}})
	if err != nil || len(ids) != 1 {
		t.Fatal(err)
	}
	if !client.RecoveryBlocked() || client.RecoveryGeneration() != 11 {
		t.Fatal("authenticated security report did not update the controller lock generation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.SecurityReport(ctx, map[string]any{})
	if err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestPanelClientUsesIsolatedTransportAndRecoversAfterError(t *testing.T) {
	client := New(config.Config{PanelURL: "http://127.0.0.1:1", AgentID: "node-test", AgentSecret: "test-only-secret", RequestTimeout: 300 * time.Millisecond})
	if client.comm.HTTPTransport() == nil || client.comm.HTTPTransport() == http.DefaultTransport {
		t.Fatal("panel client must use an isolated http.Transport")
	}
	_, err := client.Heartbeat(context.Background(), map[string]any{"version": "test"})
	if err == nil {
		t.Fatal("expected dial error for closed port")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tunnel":{"enabled":false}}`))
	}))
	defer server.Close()
	client.comm.SetBaseURL(server.URL)
	res, err := client.Heartbeat(context.Background(), map[string]any{"version": "test"})
	if err != nil || !res.OK {
		t.Fatalf("expected recovery after transport reset, got res=%+v err=%v", res, err)
	}
}

func TestSecurityReportUpgradesToWSTunnelAndFallsBackToHTTP(t *testing.T) {
	var httpCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		_, _ = w.Write([]byte(`{"ack":["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"],"recoveryBlocked":false,"recoveryGeneration":12}`))
	}))
	defer server.Close()

	client := New(config.Config{PanelURL: server.URL, AgentID: "fixture", AgentSecret: "secret", RequestTimeout: time.Second})
	caller := &mockRPCCaller{
		methods: map[string]bool{"agent.security.report": true},
		call: func(_ context.Context, method string, _ any) (tunnel.RPCResponseEnvelope, error) {
			if method != "agent.security.report" {
				t.Fatalf("unexpected rpc method: %s", method)
			}
			return tunnel.RPCResponseEnvelope{
				V:      tunnel.RPCProtocolVersion,
				OK:     true,
				Status: http.StatusOK,
				Data:   json.RawMessage(`{"ack":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],"recoveryBlocked":true,"recoveryGeneration":11}`),
			}, nil
		},
	}
	client.SetTunnelTransport(caller)

	ids, err := client.SecurityReport(context.Background(), map[string]any{"state": map[string]any{"locked": true, "revision": 3}, "events": []any{}})
	if err != nil || len(ids) != 1 || ids[0] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("expected WS SecurityReport success, ids=%v err=%v", ids, err)
	}
	if httpCalls.Load() != 0 {
		t.Fatalf("expected 0 HTTP calls when WS succeeded, got %d", httpCalls.Load())
	}
	if !client.RecoveryBlocked() || client.RecoveryGeneration() != 11 {
		t.Fatalf("unexpected recovery state after WS report: blocked=%v gen=%d", client.RecoveryBlocked(), client.RecoveryGeneration())
	}

	// Disable method -> seamless fallback to signed HTTP
	caller.methods["agent.security.report"] = false
	ids, err = client.SecurityReport(context.Background(), map[string]any{"state": map[string]any{"locked": false, "revision": 4}, "events": []any{}})
	if err != nil || len(ids) != 1 || ids[0] != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("expected HTTP fallback SecurityReport success, ids=%v err=%v", ids, err)
	}
	if httpCalls.Load() != 1 || client.RecoveryBlocked() || client.RecoveryGeneration() != 12 {
		t.Fatalf("unexpected state after HTTP fallback: httpCalls=%d blocked=%v gen=%d", httpCalls.Load(), client.RecoveryBlocked(), client.RecoveryGeneration())
	}
}
