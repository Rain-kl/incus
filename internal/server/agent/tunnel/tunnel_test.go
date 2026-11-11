package tunnel

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

func TestFraming(t *testing.T) {
	payload := []byte("hello-tunnel-payload")
	frame := EncodeFrame(FrameTypeData, 42, payload)

	if len(frame) != 5+len(payload) {
		t.Fatalf("expected frame length %d, got %d", 5+len(payload), len(frame))
	}

	frameType, streamID, decodedPayload, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("failed to decode frame: %v", err)
	}

	if frameType != FrameTypeData {
		t.Fatalf("expected frame type %d, got %d", FrameTypeData, frameType)
	}

	if streamID != 42 {
		t.Fatalf("expected stream ID 42, got %d", streamID)
	}

	if !bytes.Equal(decodedPayload, payload) {
		t.Fatalf("expected payload %q, got %q", payload, decodedPayload)
	}
}

func TestConfigFrame(t *testing.T) {
	cfg := TargetConfig{
		TargetHost: "127.0.0.1",
		TargetPort: 8443,
	}

	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json marshal error: %v", err)
	}

	frame := EncodeFrame(FrameTypeConfig, 0, cfgBytes)
	frameType, streamID, payload, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("failed to decode frame: %v", err)
	}

	if frameType != FrameTypeConfig || streamID != 0 {
		t.Fatalf("unexpected frame header: type=%d, stream=%d", frameType, streamID)
	}

	var parsed TargetConfig
	{
		err := json.Unmarshal(payload, &parsed)
		if err != nil {
			t.Fatalf("json unmarshal error: %v", err)
		}
	}

	if parsed.TargetHost != "127.0.0.1" || parsed.TargetPort != 8443 {
		t.Fatalf("unexpected parsed config: %+v", parsed)
	}
}

func TestMalformedFrame(t *testing.T) {
	_, _, _, err := DecodeFrame([]byte{0x01, 0x02})
	if err == nil {
		t.Fatalf("expected error on short frame, got nil")
	}
}

func TestLocalTCPDial(t *testing.T) {
	// 启动一个本地 Echo TCP 服务器
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}

	defer func() { _ = listener.Close() }()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}()

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("expected TCP listener")
	}

	worker := NewWorker(WorkerConfig{
		TargetHost: tcpAddr.IP.String(),
		TargetPort: tcpAddr.Port,
	})

	host, port := worker.getTarget()
	if host != tcpAddr.IP.String() || port != tcpAddr.Port {
		t.Fatalf("worker target mismatch: %s:%d vs %s:%d", host, port, tcpAddr.IP.String(), tcpAddr.Port)
	}

	// 拨号并测试 echo
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 1*time.Second)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}

	defer func() { _ = conn.Close() }()

	testData := []byte("echo-test-bytes")
	{
		_, err := conn.Write(testData)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
	}

	buf := make([]byte, len(testData))
	{
		_, err := io.ReadFull(conn, buf)
		if err != nil {
			t.Fatalf("read error: %v", err)
		}
	}

	if !bytes.Equal(buf, testData) {
		t.Fatalf("expected %q, got %q", testData, buf)
	}
}
