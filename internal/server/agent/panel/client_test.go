package panel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/config"
	"github.com/lxc/incus/v7/internal/server/agent/protocol"
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
	if client.httpClient.Transport == nil || client.httpClient.Transport == http.DefaultTransport {
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
	client.panelURL = server.URL
	res, err := client.Heartbeat(context.Background(), map[string]any{"version": "test"})
	if err != nil || !res.OK {
		t.Fatalf("expected recovery after transport reset, got res=%+v err=%v", res, err)
	}
}
