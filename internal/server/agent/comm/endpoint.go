package comm

import (
	"context"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/tunnel"
)

// Transport identifiers describe which channel carried an endpoint request.
const (
	TransportHTTP           = "http"
	TransportWS             = "ws-tunnel"
	DefaultWSTimeout        = 20 * time.Second
	DefaultMaxResponseBytes = int64(1024 * 1024)
)

// WSTransport abstracts an optional multiplexed WebSocket RPC channel.
type WSTransport interface {
	SupportsRPCMethod(method string) bool
	CallRPC(ctx context.Context, method string, params any) (tunnel.RPCResponseEnvelope, error)
	Probe()
}

// Endpoint describes a node-to-controller endpoint that prefers WS RPC with signed HTTP fallback.
type Endpoint struct {
	RPCMethod        string
	HTTPMethod       string
	HTTPPath         string
	WSTimeout        time.Duration
	MaxResponseBytes int64
}

// CallResult carries normalized response fields and transport metadata.
type CallResult struct {
	StatusCode     int
	Body           []byte
	OK             bool
	Transport      string
	LatencyMs      int64
	FallbackReason string
}

// Config configures a unified node-to-controller communication client.
type Config struct {
	BaseURL           string
	AgentID           string
	AgentSecret       string
	RequestTimeout    time.Duration
	OnTransportChange func(endpoint, from, to, reason string)
}
