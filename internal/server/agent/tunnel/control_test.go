package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lxc/incus/v7/internal/server/agent/nodeclient"
	"github.com/lxc/incus/v7/internal/server/agent/protocol"
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
		expected := protocol.Signature(secret, protocol.SigningPayload(http.MethodGet, "/api/agent/tunnel", r.Header.Get("x-incus-timestamp"), r.Header.Get("x-incus-nonce"), hash))
		if r.Header.Get("x-incus-signature") != expected {
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

	triggered := make(chan string, 2)
	worker := NewWorker(WorkerConfig{PanelURL: server.URL, AgentID: "local-agent", AgentSecret: secret, TargetHost: "127.0.0.1", TargetPort: addr.Port, RequestHeartbeat: func(id string) { triggered <- id }})
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
	if err != nil || len(hello.Capabilities) != 4 || hello.Capabilities[0] != "incus_node_v1" || hello.Capabilities[1] != "heartbeat-trigger-v1" || hello.Capabilities[3] != RPCCapability {
		t.Fatal("unexpected node capabilities", hello, err)
	}
	send(FrameTypeHeartbeat, 0, nil)
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("heartbeat notification was not delivered")
	}
	requestID := "1e862c0a-0316-4a53-9c5f-a6be04ba2534"
	send(FrameTypeHeartbeat, 0, map[string]string{"requestId": requestID})
	select {
	case id := <-triggered:
		if id != requestID {
			t.Fatal("correlation lost", id)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed heartbeat not delivered")
	}
	send(FrameTypeHeartbeat, 0, map[string]string{"requestId": "invalid"})
	select {
	case <-triggered:
		t.Fatal("invalid heartbeat accepted")
	case <-time.After(10 * time.Millisecond):
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

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAgentJournalLogControlUsesIncusAgentUnit(t *testing.T) {
	var gotUnit string
	var gotLines string
	nodeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1.0/agent/journal" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotUnit = r.URL.Query().Get("unit")
		gotLines = r.URL.Query().Get("lines")
		if gotUnit != "incus-agent" {
			http.Error(w, "invalid journal request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: \"agent started\"\n\n"))
	}))
	defer nodeAPI.Close()

	targetURL, err := url.Parse(nodeAPI.URL)
	if err != nil {
		t.Fatal(err)
	}
	nodeClient := &nodeclient.Client{
		HTTP: &http.Client{
			Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				cloned := r.Clone(r.Context())
				cloned.URL.Scheme = targetURL.Scheme
				cloned.URL.Host = targetURL.Host
				return http.DefaultTransport.RoundTrip(cloned)
			}),
		},
	}

	connections := make(chan *websocket.Conn, 1)
	secret := "local-test-secret"
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e == nil {
			connections <- conn
		}
	}))
	defer panel.Close()

	worker := NewWorker(WorkerConfig{
		PanelURL:    panel.URL,
		AgentID:     "local-agent",
		AgentSecret: secret,
		TargetHost:  "127.0.0.1",
		TargetPort:  8443,
		NodeClient:  nodeClient,
	})
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

	payload, _ := json.Marshal(LogControl{Action: "start", Lines: 100})
	if err := conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(FrameTypeLogCtl, 0, payload)); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	kind, streamID, data, err := DecodeFrame(frame)
	if err != nil || kind != FrameTypeLogData || streamID != 0 {
		t.Fatalf("unexpected frame %d/%d: %v", kind, streamID, err)
	}
	if string(data) != "agent started" {
		t.Fatalf("unexpected log line: %q (unit=%q lines=%q)", string(data), gotUnit, gotLines)
	}
	if gotUnit != "incus-agent" || gotLines != "100" {
		t.Fatalf("unexpected journal query: unit=%q lines=%q", gotUnit, gotLines)
	}

	cancel()
	<-done
}

func TestReconnectNowWakesBackoffAndProbeDetectsDeadSocket(t *testing.T) {
	var attempts atomic.Int32
	var allowUpgrade atomic.Bool
	allowUpgrade.Store(true)
	connections := make(chan *websocket.Conn, 4)

	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if !allowUpgrade.Load() || n == 2 || n == 3 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			connections <- conn
		}
	}))
	defer panel.Close()

	worker := NewWorker(WorkerConfig{
		PanelURL:    panel.URL,
		AgentID:     "local-agent",
		AgentSecret: "local-test-secret",
	})
	worker.SyncConfig(true, "127.0.0.1", 8443)
	defer worker.SyncConfig(false, "", 0)

	var first *websocket.Conn
	select {
	case first = <-connections:
	case <-time.After(3 * time.Second):
		t.Fatal("initial connection timed out")
	}

	// Close first connection; attempts 2 & 3 fail so backoff reaches >= 4s.
	_ = first.Close()
	deadline := time.Now().Add(4 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if attempts.Load() < 3 {
		t.Fatalf("expected >= 3 dial attempts, got %d", attempts.Load())
	}

	// Wake reconnect immediately via Probe() instead of waiting for full backoff.
	start := time.Now()
	worker.Probe()
	select {
	case second := <-connections:
		_ = second.Close()
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Fatalf("expected immediate reconnect, took %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReconnectNow/Probe did not wake backoff")
	}
}

func TestTunnelRPCNegotiationAndConcurrency(t *testing.T) {
	connections := make(chan *websocket.Conn, 2)
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			connections <- conn
		}
	}))
	defer panel.Close()

	worker := NewWorker(WorkerConfig{
		PanelURL:    panel.URL,
		AgentID:     "rpc-agent",
		AgentSecret: "rpc-secret",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- worker.connectAndServe(ctx)
	}()

	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tunnel connection")
	}
	defer conn.Close()

	// Empty CONTROL_HELLO from legacy control plane keeps RPC disabled.
	err := conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(FrameTypeControlHello, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, helloBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	frameType, _, _, err := DecodeFrame(helloBytes)
	if err != nil || frameType != FrameTypeControlHello {
		t.Fatalf("expected client CONTROL_HELLO, got type=%d err=%v", frameType, err)
	}
	if worker.SupportsRPCMethod("agent.heartbeat") {
		t.Fatal("legacy empty CONTROL_HELLO must not enable RPC")
	}
	_, err = worker.CallRPC(context.Background(), "agent.heartbeat", map[string]any{})
	if !errors.Is(err, ErrRPCUnsupported) {
		t.Fatalf("expected ErrRPCUnsupported, got %v", err)
	}

	// Negotiated CONTROL_HELLO enables only advertised methods.
	helloPayload, err := json.Marshal(ControlHelloPayload{
		Capabilities: []string{RPCCapability},
		RPCMethods:   []string{"agent.heartbeat", "node.echo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(FrameTypeControlHello, 0, helloPayload))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !worker.SupportsRPCMethod("agent.heartbeat") || !worker.SupportsRPCMethod("node.echo") {
		t.Fatal("expected advertised methods to be supported")
	}
	if worker.SupportsRPCMethod("node.other") {
		t.Fatal("unadvertised method must not be supported")
	}

	// Concurrent requests receive out-of-order responses matched by streamID.
	var writeMu sync.Mutex
	receivedReqs := make(chan RPCRequestEnvelope, 4)
	receivedIDs := make(chan uint32, 4)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		type captured struct {
			id  uint32
			req RPCRequestEnvelope
		}
		var batch []captured
		for len(batch) < 2 {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			msgType, frame, rErr := conn.ReadMessage()
			if rErr != nil {
				return
			}
			if msgType != websocket.BinaryMessage {
				continue
			}
			kind, sID, payload, dErr := DecodeFrame(frame)
			if dErr != nil || kind != FrameTypeRPCRequest {
				continue
			}
			var req RPCRequestEnvelope
			_ = json.Unmarshal(payload, &req)
			batch = append(batch, captured{id: sID, req: req})
			receivedReqs <- req
			receivedIDs <- sID
		}
		// Reply in reverse order (second first, then first).
		for i := len(batch) - 1; i >= 0; i-- {
			data, _ := json.Marshal(map[string]any{"method": batch[i].req.Method, "id": batch[i].id})
			respBytes, _ := json.Marshal(RPCResponseEnvelope{
				V:      RPCProtocolVersion,
				OK:     true,
				Status: http.StatusOK,
				Data:   data,
			})
			writeMu.Lock()
			_ = conn.WriteMessage(websocket.BinaryMessage, EncodeFrame(FrameTypeRPCResponse, batch[i].id, respBytes))
			writeMu.Unlock()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]RPCResponseEnvelope, 2)
	errs := make([]error, 2)
	go func() {
		defer wg.Done()
		results[0], errs[0] = worker.CallRPC(context.Background(), "agent.heartbeat", map[string]any{"seq": 1})
	}()
	go func() {
		defer wg.Done()
		results[1], errs[1] = worker.CallRPC(context.Background(), "node.echo", map[string]any{"seq": 2})
	}()
	wg.Wait()
	<-readerDone

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent RPC errors: %v / %v", errs[0], errs[1])
	}
	var d0, d1 map[string]any
	_ = json.Unmarshal(results[0].Data, &d0)
	_ = json.Unmarshal(results[1].Data, &d1)
	if d0["method"] != "agent.heartbeat" || d1["method"] != "node.echo" {
		t.Fatalf("mismatched concurrent responses: d0=%v d1=%v", d0, d1)
	}

	// Oversized payload is rejected before queueing on the socket.
	oversized := map[string]any{"blob": strings.Repeat("x", MaxRPCPayloadBytes)}
	_, err = worker.CallRPC(context.Background(), "agent.heartbeat", oversized)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}

	// Caller timeout returns context.DeadlineExceeded without stalling.
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err = worker.CallRPC(timeoutCtx, "node.echo", map[string]any{"hang": true})
	timeoutCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, _ = conn.ReadMessage()

	// Closing the tunnel wakes pending CallRPC immediately with ErrTunnelClosed.
	closeErrCh := make(chan error, 1)
	go func() {
		_, cErr := worker.CallRPC(context.Background(), "node.echo", map[string]any{"waitClose": true})
		closeErrCh <- cErr
	}()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = conn.ReadMessage()
	_ = conn.Close()

	select {
	case cErr := <-closeErrCh:
		if !errors.Is(cErr, ErrTunnelClosed) {
			t.Fatalf("expected ErrTunnelClosed on disconnect, got %v", cErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending CallRPC was not woken when tunnel closed")
	}

	cancel()
	<-done
}
