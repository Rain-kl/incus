package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// ControlHandler exposes only fixed, typed operations over the authenticated
// Panel websocket. It never accepts executable names or arbitrary commands.
type (
	ControlHandler func(context.Context, string, json.RawMessage) (any, error)
	controlSession struct {
		nodeAPI bool
		mu      sync.Mutex
		ctx     context.Context
		handler ControlHandler
		output  chan<- []byte
		pending map[uint32]context.CancelFunc
	}
)

func newControlSession(ctx context.Context, handler ControlHandler, output chan<- []byte) *controlSession {
	return &controlSession{ctx: ctx, handler: handler, output: output, pending: map[uint32]context.CancelFunc{}}
}

func (s *controlSession) send(kind byte, id uint32, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		return
	}

	select {
	case s.output <- EncodeFrame(kind, id, payload):
	case <-s.ctx.Done():
	}
}

func (s *controlSession) reply(id uint32, result any, err error) {
	if err != nil {
		code := "operation_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "timeout"
		} else if errors.Is(err, context.Canceled) {
			code = "cancelled"
		} else if err.Error() == "busy" {
			code = "busy"
		}

		s.send(FrameTypeControlResponse, id, map[string]any{"ok": false, "error": code})
		return
	}

	s.send(FrameTypeControlResponse, id, map[string]any{"ok": true, "result": result})
}

func (s *controlSession) handle(kind byte, id uint32, payload []byte) {
	if kind == FrameTypeControlHello {
		if id != 0 {
			return
		}

		capabilities := []string{}
		if s.handler != nil {
			capabilities = []string{"network.apply", "network.apply.v2", "proxy.probe"}
		}

		if s.nodeAPI {
			capabilities = append(capabilities, "payincus_node_v1")
		}

		s.send(FrameTypeControlHello, 0, map[string]any{"capabilities": capabilities})
		return
	}

	if id == 0 {
		return
	}

	if kind == FrameTypeControlCancel {
		s.mu.Lock()
		cancel := s.pending[id]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}

		return
	}

	var request struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}

	if len(payload) > 1<<20 || json.Unmarshal(payload, &request) != nil || s.handler == nil || (request.Method != "network.apply" && request.Method != "network.apply.v2" && request.Method != "proxy.probe") {
		s.send(FrameTypeControlResponse, id, map[string]any{"ok": false, "error": "invalid_request"})
		return
	}

	s.mu.Lock()
	{
		_, exists := s.pending[id]
		if exists || len(s.pending) >= 4 {
			s.mu.Unlock()
			s.send(FrameTypeControlResponse, id, map[string]any{"ok": false, "error": "busy"})
			return
		}
	}

	timeout := 95 * time.Second
	if request.Method == "proxy.probe" {
		timeout = 40 * time.Second
	}

	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	s.pending[id] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.pending, id)
			s.mu.Unlock()
			if recover() != nil {
				s.send(FrameTypeControlResponse, id, map[string]any{"ok": false, "error": "operation_failed"})
			}
		}()
		result, err := s.handler(ctx, request.Method, request.Params)
		s.reply(id, result, err)
	}()
}
