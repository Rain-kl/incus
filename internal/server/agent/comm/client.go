package comm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/tunnel"
)

// Client dispatches authenticated endpoint requests over WS tunnel RPC when negotiated,
// automatically upgrading once the tunnel is ready and falling back to signed HTTP.
type Client struct {
	http          *httpTransport
	wsMu          sync.RWMutex
	ws            WSTransport
	transportMu   sync.Mutex
	lastTransport map[string]string
	onChange      func(endpoint, from, to, reason string)
}

// New constructs a unified communication client with isolated HTTP/1.1 transport.
func New(cfg Config) *Client {
	return &Client{
		http:          newHTTPTransport(cfg),
		lastTransport: make(map[string]string),
		onChange:      cfg.OnTransportChange,
	}
}

// SetWSTransport attaches or clears the multiplexed WebSocket RPC transport.
func (c *Client) SetWSTransport(ws WSTransport) {
	if c == nil {
		return
	}
	c.wsMu.Lock()
	c.ws = ws
	c.wsMu.Unlock()
}

func (c *Client) getWSTransport() WSTransport {
	if c == nil {
		return nil
	}
	c.wsMu.RLock()
	ws := c.ws
	c.wsMu.RUnlock()
	return ws
}

// HTTPTransport exposes the underlying HTTP RoundTripper for transport isolation tests.
func (c *Client) HTTPTransport() http.RoundTripper {
	if c == nil || c.http == nil || c.http.client == nil {
		return nil
	}
	return c.http.client.Transport
}

// SetBaseURL updates the HTTP base URL (intended for tests).
func (c *Client) SetBaseURL(rawURL string) {
	if c == nil || c.http == nil {
		return
	}
	c.http.setBaseURL(rawURL)
}

func (c *Client) recordTransport(endpoint, next, reason string) {
	if c == nil || next == "" {
		return
	}
	c.transportMu.Lock()
	prev := c.lastTransport[endpoint]
	if prev != next {
		c.lastTransport[endpoint] = next
	}
	c.transportMu.Unlock()

	if prev != next && c.onChange != nil {
		c.onChange(endpoint, prev, next, reason)
	}
}

// Call executes an endpoint request, preferring WS RPC and falling back to signed HTTP.
func (c *Client) Call(ctx context.Context, ep Endpoint, payload any) (CallResult, error) {
	if c == nil || c.http == nil {
		return CallResult{}, errors.New("communication client is nil")
	}
	if ep.RPCMethod == "" || ep.HTTPPath == "" {
		return CallResult{}, errors.New("endpoint must declare both RPCMethod and HTTPPath")
	}

	ws := c.getWSTransport()
	fallbackReason := ""

	if ws != nil {
		if !ws.SupportsRPCMethod(ep.RPCMethod) {
			fallbackReason = "tunnel_unavailable"
		} else {
			wsTimeout := ep.WSTimeout
			if wsTimeout <= 0 {
				wsTimeout = DefaultWSTimeout
			}
			wsCtx, cancel := context.WithTimeout(ctx, wsTimeout)
			startedAt := time.Now()
			resp, err := ws.CallRPC(wsCtx, ep.RPCMethod, payload)
			cancel()

			if err == nil {
				if shouldFallbackFromRPCResponse(resp) {
					fallbackReason = fallbackReasonFromStatus(resp)
				} else {
					latencyMs := time.Since(startedAt).Milliseconds()
					body := formatRPCResponseBody(resp)
					ok := resp.Status >= 200 && resp.Status < 300
					result := CallResult{
						StatusCode: resp.Status,
						Body:       body,
						OK:         ok,
						Transport:  TransportWS,
						LatencyMs:  latencyMs,
					}
					c.recordTransport(ep.RPCMethod, TransportWS, "")
					if !ok {
						return result, fmt.Errorf("%s failed: status=%d body=%s", ep.RPCMethod, resp.Status, string(body))
					}
					return result, nil
				}
			} else {
				if ctx.Err() != nil {
					return CallResult{Transport: TransportWS}, err
				}
				if errors.Is(err, context.DeadlineExceeded) {
					ws.Probe()
					fallbackReason = "ws_timeout"
				} else {
					fallbackReason = classifyRPCError(err)
				}
			}
		}
	}

	httpResult, err := c.http.call(ctx, ep.HTTPMethod, ep.HTTPPath, payload, ep.MaxResponseBytes)
	httpResult.Transport = TransportHTTP
	httpResult.FallbackReason = fallbackReason
	if httpResult.StatusCode > 0 || err == nil {
		c.recordTransport(ep.RPCMethod, TransportHTTP, fallbackReason)
	}
	return httpResult, err
}

func shouldFallbackFromRPCResponse(resp tunnel.RPCResponseEnvelope) bool {
	if resp.Status == http.StatusNotImplemented {
		return true
	}
	if resp.Status == http.StatusNotFound && resp.Code == "RPC_METHOD_NOT_FOUND" {
		return true
	}
	if resp.Status == http.StatusRequestEntityTooLarge && resp.Code == "PAYLOAD_TOO_LARGE" {
		return true
	}
	return false
}

func fallbackReasonFromStatus(resp tunnel.RPCResponseEnvelope) string {
	if resp.Status == http.StatusRequestEntityTooLarge {
		return "payload_too_large"
	}
	return "rpc_method_not_found"
}

func classifyRPCError(err error) string {
	switch {
	case errors.Is(err, tunnel.ErrPayloadTooLarge):
		return "payload_too_large"
	case errors.Is(err, tunnel.ErrTunnelClosed):
		return "ws_disconnected"
	case errors.Is(err, tunnel.ErrTunnelNotReady), errors.Is(err, tunnel.ErrRPCUnsupported):
		return "tunnel_unavailable"
	default:
		return "rpc_error"
	}
}

func formatRPCResponseBody(resp tunnel.RPCResponseEnvelope) []byte {
	trimmed := bytes.TrimSpace(resp.Data)
	if len(trimmed) > 0 {
		out := make([]byte, len(trimmed))
		copy(out, trimmed)
		return out
	}
	if resp.Status >= 400 {
		encoded, err := json.Marshal(map[string]any{
			"code":  resp.Code,
			"error": resp.Error,
		})
		if err == nil {
			return encoded
		}
	}
	return []byte("{}")
}
