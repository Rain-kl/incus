package tunnel

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lxc/incus/v7/internal/server/payincus/protocol"
)

// Exercise authenticated native transport and ensure retired RPC cannot run business operations.
func TestNativeIncusStreamAndRejectedRPC(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = listener.Close() }()
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}

			go func() { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	connections := make(chan *websocket.Conn, 1)
	secret := "local-test-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := protocol.BodySHA256(nil)
		expected := protocol.Signature(secret, protocol.SigningPayload(http.MethodGet, "/api/agent/tunnel", r.Header.Get("x-incudal-timestamp"), r.Header.Get("x-incudal-nonce"), hash))
		if r.Header.Get("x-incudal-signature") != expected {
			http.Error(w, "authentication", http.StatusUnauthorized)
			return
		}

		conn, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e == nil {
			connections <- conn
		}
	}))
	defer server.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("expected TCP listener")
	}

	triggered := make(chan struct{}, 1)
	worker := NewWorker(WorkerConfig{PanelURL: server.URL, AgentID: "local-agent", AgentSecret: secret, TargetHost: "127.0.0.1", TargetPort: addr.Port, RequestHeartbeat: func() { triggered <- struct{}{} }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.connectAndServe(ctx) }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(3 * time.Second):
		t.Fatal("no websocket")
	}

	defer conn.Close()
	send := func(kind byte, id uint32, body any) {
		t.Helper()
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		{
			err := conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(kind, id, payload))
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	receive := func(kind byte, id uint32) []byte {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, frame, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}

		got, stream, payload, e := DecodeFrame(frame)
		if e != nil || got != kind || stream != id {
			t.Fatalf("unexpected frame %d/%d: %v", got, stream, e)
		}

		return payload
	}

	send(FrameTypeControlHello, 0, nil)
	var hello struct {
		Capabilities []string `json:"capabilities"`
	}
	err = json.Unmarshal(receive(FrameTypeControlHello, 0), &hello)
	if err != nil || len(hello.Capabilities) != 2 || hello.Capabilities[0] != "payincus_node_v1" || hello.Capabilities[1] != "heartbeat-trigger-v1" {
		t.Fatal("unexpected node capabilities", hello, err)
	}
	send(FrameTypeHeartbeat, 0, nil)
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("heartbeat notification was not delivered")
	}
	// Obsolete business RPC frames cannot execute operations or interfere with the byte relay.
	send(0x08, 7, map[string]any{"method": "network.apply", "params": map[string]any{"revision": 1}})
	send(FrameTypeOpen, 2, nil)
	send(FrameTypeData, 2, nil)
	err = conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(FrameTypeData, 2, []byte("native-api-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if string(receive(FrameTypeData, 2)) != "native-api-bytes" {
		t.Fatal("byte relay failed")
	}
	cancel()
	select {
	case connectionErr := <-done:
		if connectionErr != nil && ctx.Err() == nil {
			t.Fatal(connectionErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel leaked after cancellation")
	}
}
