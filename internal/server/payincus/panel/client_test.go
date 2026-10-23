package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/config"
	"github.com/lxc/incus/v7/internal/server/payincus/protocol"
)

func TestSignedHeartbeatContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != heartbeatPath || r.Header.Get("x-incudal-agent-id") != "node-test" {
			t.Error("invalid heartbeat route or identity")
		}

		payload := protocol.SigningPayload(r.Method, r.URL.Path, r.Header.Get("x-incudal-timestamp"), r.Header.Get("x-incudal-nonce"), r.Header.Get("x-incudal-body-sha256"))
		if r.Header.Get("x-incudal-signature") != protocol.Signature("test-only-secret", payload) {
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
