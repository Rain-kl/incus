package tunnel

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/lxc/incus/v7/internal/server/agent/protocol"
)

type rpcDelivery struct {
	envelope RPCResponseEnvelope
	err      error
}

type rpcSession struct {
	connCtx       context.Context
	writeChan     chan<- []byte
	mu            sync.Mutex
	helloReady    bool
	serverCaps    map[string]bool
	serverMethods map[string]bool
	nextReqID     uint32
	pending       map[uint32]chan rpcDelivery
	closed        bool
}

func newRPCSession(connCtx context.Context, writeChan chan<- []byte) *rpcSession {
	return &rpcSession{
		connCtx:       connCtx,
		writeChan:     writeChan,
		serverCaps:    make(map[string]bool),
		serverMethods: make(map[string]bool),
		pending:       make(map[uint32]chan rpcDelivery),
	}
}

func (s *rpcSession) setServerHello(hello ControlHelloPayload) {
	caps := make(map[string]bool, len(hello.Capabilities))
	for _, capName := range hello.Capabilities {
		if capName != "" {
			caps[capName] = true
		}
	}

	methods := make(map[string]bool, len(hello.RPCMethods))
	for _, method := range hello.RPCMethods {
		if method != "" {
			methods[method] = true
		}
	}

	s.mu.Lock()
	s.helloReady = true
	s.serverCaps = caps
	s.serverMethods = methods
	s.mu.Unlock()
}

func (s *rpcSession) supportsMethod(method string) bool {
	if s == nil || method == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.helloReady {
		return false
	}
	return s.serverCaps[RPCCapability] && s.serverMethods[method]
}

func (s *rpcSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pending := s.pending
	s.pending = make(map[uint32]chan rpcDelivery)
	s.mu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- rpcDelivery{err: ErrTunnelClosed}:
		default:
		}
	}
}

func (s *rpcSession) cancelPending(requestID uint32) {
	s.mu.Lock()
	delete(s.pending, requestID)
	s.mu.Unlock()
}

func (s *rpcSession) handleResponse(requestID uint32, payload []byte) {
	if requestID == 0 {
		return
	}

	var envelope RPCResponseEnvelope
	var decodeErr error
	err := json.Unmarshal(payload, &envelope)
	if err != nil || envelope.V != RPCProtocolVersion {
		decodeErr = ErrInvalidRPCResponse
	}

	s.mu.Lock()
	ch, exists := s.pending[requestID]
	if exists {
		delete(s.pending, requestID)
	}
	s.mu.Unlock()

	if !exists {
		return
	}
	select {
	case ch <- rpcDelivery{envelope: envelope, err: decodeErr}:
	default:
	}
}

func (s *rpcSession) call(ctx context.Context, method string, params any) (RPCResponseEnvelope, error) {
	req := RPCRequestEnvelope{
		V:      RPCProtocolVersion,
		Method: method,
		Params: params,
	}
	payload, err := protocol.CanonicalJSON(req)
	if err != nil {
		return RPCResponseEnvelope{}, err
	}
	if len(payload) > MaxRPCPayloadBytes {
		return RPCResponseEnvelope{}, ErrPayloadTooLarge
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RPCResponseEnvelope{}, ErrTunnelClosed
	}
	if !s.helloReady || !s.serverCaps[RPCCapability] || !s.serverMethods[method] {
		s.mu.Unlock()
		return RPCResponseEnvelope{}, ErrRPCUnsupported
	}
	var reqID uint32
	for {
		s.nextReqID++
		if s.nextReqID == 0 {
			s.nextReqID = 1
		}
		_, collision := s.pending[s.nextReqID]
		if !collision {
			reqID = s.nextReqID
			break
		}
	}
	respCh := make(chan rpcDelivery, 1)
	s.pending[reqID] = respCh
	s.mu.Unlock()

	frame := EncodeFrame(FrameTypeRPCRequest, reqID, payload)
	select {
	case s.writeChan <- frame:
	case <-s.connCtx.Done():
		s.cancelPending(reqID)
		return RPCResponseEnvelope{}, ErrTunnelClosed
	case <-ctx.Done():
		s.cancelPending(reqID)
		return RPCResponseEnvelope{}, ctx.Err()
	}

	select {
	case delivery := <-respCh:
		return delivery.envelope, delivery.err
	case <-s.connCtx.Done():
		s.cancelPending(reqID)
		return RPCResponseEnvelope{}, ErrTunnelClosed
	case <-ctx.Done():
		s.cancelPending(reqID)
		return RPCResponseEnvelope{}, ctx.Err()
	}
}

// SupportsRPCMethod reports whether an active tunnel supports the given RPC method.
func (w *Worker) SupportsRPCMethod(method string) bool {
	if w == nil || !w.connected.Load() {
		return false
	}
	sess := w.getSession()
	return sess.supportsMethod(method)
}

// CallRPC invokes a named endpoint over the active reverse WebSocket tunnel.
func (w *Worker) CallRPC(ctx context.Context, method string, params any) (RPCResponseEnvelope, error) {
	if w == nil || !w.connected.Load() {
		return RPCResponseEnvelope{}, ErrTunnelNotReady
	}
	sess := w.getSession()
	if sess == nil {
		return RPCResponseEnvelope{}, ErrTunnelNotReady
	}
	return sess.call(ctx, method, params)
}

func (w *Worker) getSession() *rpcSession {
	w.sessionMu.Lock()
	defer w.sessionMu.Unlock()
	return w.session
}

func (w *Worker) setSession(sess *rpcSession) {
	w.sessionMu.Lock()
	w.session = sess
	w.sessionMu.Unlock()
}

func (w *Worker) clearSession(sess *rpcSession) {
	w.sessionMu.Lock()
	if w.session == sess {
		w.session = nil
	}
	w.sessionMu.Unlock()
}
