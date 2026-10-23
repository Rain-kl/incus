package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Constants define protocol identifiers and timing limits.
const (
	FrameTypeOpen            byte = 0x01 // 新建连接流（Server -> Agent），Payload 为 JSON { targetHost, targetPort }
	FrameTypeData            byte = 0x02 // 数据传输（双向），Payload 为数据分片（<= 64KB）
	FrameTypeClose           byte = 0x03 // 关闭流（双向，发送端结束 / EOF）
	FrameTypeReset           byte = 0x04 // 重置流（双向，连接中断或错误）
	FrameTypeConfig          byte = 0x05 // 动态热配置（Server -> Agent），StreamID = 0，Payload 为 JSON { targetHost, targetPort }
	FrameTypeLogCtl          byte = 0x06 // 日志流控制（Server -> Agent），StreamID = 0，Payload 为 JSON { action: 'start' | 'stop', lines?: number }
	FrameTypeLogData         byte = 0x07 // 日志流数据（Agent -> Server），StreamID = 0，Payload 为一行 UTF-8 文本；空 Payload 表示流结束
	FrameTypeControlRequest  byte = 0x08
	FrameTypeControlResponse byte = 0x09
	FrameTypeControlHello    byte = 0x0a
	FrameTypeControlCancel   byte = 0x0b
	FrameTypeTaskLogCtl      byte = 0x0c
	FrameTypeTaskLogData     byte = 0x0d
)

// HeaderLength is the byte size of the multiplexed frame header.
const HeaderLength = 5

// TargetConfig identifies the local reverse-tunnel destination.
type TargetConfig struct {
	TargetHost string `json:"targetHost"`
	TargetPort int    `json:"targetPort"`
}

// ErrFrameTooShort indicates a frame missing its mandatory header.
var ErrFrameTooShort = errors.New("frame too short")

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
