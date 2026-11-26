package panel

import (
	"context"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/comm"
)

// Transport identifiers describe which channel carried an endpoint request.
const (
	TransportHTTP    = comm.TransportHTTP
	TransportWS      = comm.TransportWS
	defaultWSTimeout = comm.DefaultWSTimeout
)

// RPCCaller abstracts an optional multiplexed tunnel transport for endpoint calls.
type RPCCaller = comm.WSTransport

// EndpointSpec describes a controller endpoint that prefers tunnel RPC with HTTP fallback.
type EndpointSpec = comm.Endpoint

// CallResult carries normalized response fields and transport metadata.
type CallResult struct {
	StatusCode     int
	Body           string
	OK             bool
	Transport      string
	LatencyMs      int64
	FallbackReason string
}

// SetTunnelTransport attaches or clears the multiplexed WebSocket RPC transport.
func (client *Client) SetTunnelTransport(caller RPCCaller) {
	if client == nil || client.comm == nil {
		return
	}
	client.comm.SetWSTransport(caller)
}

// CallEndpoint dispatches a request via tunnel RPC when negotiated, falling back to signed HTTP.
func (client *Client) CallEndpoint(ctx context.Context, spec EndpointSpec, payload any) (CallResult, error) {
	if client == nil || client.comm == nil {
		return CallResult{}, context.Canceled
	}
	if spec.WSTimeout <= 0 {
		spec.WSTimeout = time.Duration(defaultWSTimeout)
	}
	res, err := client.comm.Call(ctx, spec, payload)
	return CallResult{
		StatusCode:     res.StatusCode,
		Body:           string(res.Body),
		OK:             res.OK,
		Transport:      res.Transport,
		LatencyMs:      res.LatencyMs,
		FallbackReason: res.FallbackReason,
	}, err
}
