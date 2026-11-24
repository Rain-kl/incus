package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Constants define protocol identifiers and timing limits.
const (
	FrameTypeOpen         byte = 0x01 // 新建连接流（Server -> Agent），Payload 为 JSON { targetHost, targetPort }
	FrameTypeData         byte = 0x02 // 数据传输（双向），Payload 为数据分片（<= 64KB）
	FrameTypeClose        byte = 0x03 // 关闭流（双向，发送端结束 / EOF）
	FrameTypeReset        byte = 0x04 // 重置流（双向，连接中断或错误）
	FrameTypeConfig       byte = 0x05 // 动态热配置（Server -> Agent），StreamID = 0，Payload 为 JSON { targetHost, targetPort }
	FrameTypeLogCtl       byte = 0x06 // 日志流控制（Server -> Agent），StreamID = 0，Payload 为 JSON { action: 'start' | 'stop', lines?: number }
	FrameTypeLogData      byte = 0x07 // 日志流数据（Agent -> Server），StreamID = 0，Payload 为一行 UTF-8 文本；空 Payload 表示流结束
	FrameTypeControlHello byte = 0x0a
	FrameTypeHeartbeat    byte = 0x0b
	FrameTypeTaskLogCtl   byte = 0x0c
	FrameTypeTaskLogData  byte = 0x0d
	FrameTypeRPCRequest   byte = 0x0e // 通用 RPC 请求（Agent -> Server），StreamID = 请求 ID
	FrameTypeRPCResponse  byte = 0x0f // 通用 RPC 响应（Server -> Agent），StreamID = 对应请求 ID
)

// HeaderLength is the byte size of the multiplexed frame header.
const HeaderLength = 5

// RPC protocol constants define size limits and capability negotiation tokens.
const (
	MaxRPCPayloadBytes = 900 * 1024
	RPCProtocolVersion = 1
	RPCCapability      = "tunnel-rpc-v1"
)

// TargetConfig identifies the local reverse-tunnel destination.
type TargetConfig struct {
	TargetHost string `json:"targetHost"`
	TargetPort int    `json:"targetPort"`
}

// ControlHelloPayload carries capability and method negotiation metadata on CONTROL_HELLO frames.
type ControlHelloPayload struct {
	Capabilities []string `json:"capabilities"`
	RPCMethods   []string `json:"rpcMethods,omitempty"`
}

// RPCRequestEnvelope represents a versioned RPC call carried inside FrameTypeRPCRequest.
type RPCRequestEnvelope struct {
	V       int               `json:"v"`
	Method  string            `json:"method"`
	Params  any               `json:"params,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// RPCResponseEnvelope represents a versioned RPC reply carried inside FrameTypeRPCResponse.
type RPCResponseEnvelope struct {
	V      int             `json:"v"`
	OK     bool            `json:"ok"`
	Status int             `json:"status"`
	Code   string          `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Sentinel errors classify tunnel RPC failures for transparent transport fallback.
var (
	ErrFrameTooShort      = errors.New("frame too short")
	ErrTunnelNotReady     = errors.New("tunnel is not ready")
	ErrRPCUnsupported     = errors.New("tunnel rpc method unsupported")
	ErrTunnelClosed       = errors.New("tunnel connection closed")
	ErrPayloadTooLarge    = errors.New("rpc payload exceeds frame limit")
	ErrInvalidRPCResponse = errors.New("invalid rpc response")
)

// EncodeFrame 编码 5 字节头部与 Payload.
func EncodeFrame(frameType byte, streamID uint32, payload []byte) []byte {
	buf := make([]byte, HeaderLength+len(payload))
	buf[0] = frameType
	binary.BigEndian.PutUint32(buf[1:5], streamID)
	if len(payload) > 0 {
		copy(buf[5:], payload)
	}

	return buf
}

// DecodeFrame 解码二进制数据帧.
func DecodeFrame(data []byte) (byte, uint32, []byte, error) {
	if len(data) < HeaderLength {
		return 0, 0, nil, fmt.Errorf("%w: got %d bytes, minimum %d", ErrFrameTooShort, len(data), HeaderLength)
	}

	frameType := data[0]
	streamID := binary.BigEndian.Uint32(data[1:5])
	payload := data[5:]
	return frameType, streamID, payload, nil
}
