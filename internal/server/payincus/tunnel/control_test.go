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

// Exercise the actual websocket pumps with multiplexed Incus bytes while a
// control request remains blocked; control work must never block the read pump.
func TestRealtimeControlAndIncusStream(t *testing.T) {
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
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	handler := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method == "proxy.probe" {
			started <- struct{}{}
			<-ctx.Done()
			cancelled <- struct{}{}
			return nil, ctx.Err()
		}

		var in struct {
			Revision int `json:"revision"`
		}
		{
			err := json.Unmarshal(params, &in)
			if err != nil {
				return nil, err
			}
		}

		return map[string]any{"appliedRevision": in.Revision, "state": "applied"}, nil
	}

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

	worker := NewWorker(WorkerConfig{PanelURL: server.URL, AgentID: "local-agent", AgentSecret: secret, TargetHost: "127.0.0.1", TargetPort: addr.Port, ControlHandler: handler})
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
	{
		err := json.Unmarshal(receive(FrameTypeControlHello, 0), &hello)
		if err != nil || len(hello.Capabilities) != 3 {
			t.Fatalf("missing handshake: %v", err)
		}
	}

	send(FrameTypeControlRequest, 1, map[string]any{"method": "proxy.probe", "params": map[string]any{}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start immediately")
	}

	send(FrameTypeOpen, 2, nil)
	{
		err := conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(FrameTypeData, 2, []byte("incus-bytes")))
		if err != nil {
			t.Fatal(err)
		}
	}

	if string(receive(FrameTypeData, 2)) != "incus-bytes" {
		t.Fatal("Incus multiplexing interrupted")
	}

	send(FrameTypeControlRequest, 3, map[string]any{"method": "network.apply", "params": map[string]any{"revision": 4}})
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Revision int `json:"appliedRevision"`
		} `json:"result"`
	}
	{
		err := json.Unmarshal(receive(FrameTypeControlResponse, 3), &response)
		if err != nil || !response.OK || response.Result.Revision != 4 {
			t.Fatalf("bad realtime response: %+v %v", response, err)
		}
	}

	send(FrameTypeControlCancel, 1, nil)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not reach handler")
	}

	var failure struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}

	_ = json.Unmarshal(receive(FrameTypeControlResponse, 1), &failure)
	if failure.OK || failure.Error != "cancelled" {
		t.Fatalf("unexpected cancel: %+v", failure)
	}

	send(FrameTypeControlRequest, 4, map[string]any{"method": "shell.exec", "params": map[string]any{}})
	_ = json.Unmarshal(receive(FrameTypeControlResponse, 4), &failure)
	if failure.Error != "invalid_request" {
		t.Fatal("unknown method accepted")
	}

	send(FrameTypeControlRequest, 5, map[string]any{"method": "proxy.probe", "params": map[string]any{}})
	<-started
	_ = conn.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel execution")
	}

	select {
	case err := <-done:
		_ = err
	case <-time.After(time.Second):
		t.Fatal("worker did not exit")
	}
}

func TestControlLimitsAndRedaction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := make(chan []byte, 16)
	session := newControlSession(ctx, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, output)
	for id := uint32(1); id <= 4; id++ {
		session.handle(FrameTypeControlRequest, id, []byte(`{"method":"proxy.probe","params":{}}`))
	}

	session.handle(FrameTypeControlRequest, 5, []byte(`{"method":"proxy.probe","params":{}}`))
	select {
	case frame := <-output:
		_, id, payload, _ := DecodeFrame(frame)
		if id != 5 || string(payload) != `{"error":"busy","ok":false}` {
			t.Fatalf("unbounded execution: %s", payload)
		}

	case <-time.After(time.Second):
		t.Fatal("missing busy response")
	}
}
