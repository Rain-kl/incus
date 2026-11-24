package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/protocol"
	"github.com/lxc/incus/v7/internal/server/agent/tunnel"
)

// Transport identifiers describe which channel carried an endpoint request.
const (
	TransportHTTP    = "http"
	TransportWS      = "ws-tunnel"
	defaultWSTimeout = 20 * time.Second
)

// RPCCaller abstracts an optional multiplexed tunnel transport for endpoint calls.
type RPCCaller interface {
	SupportsRPCMethod(method string) bool
	CallRPC(ctx context.Context, method string, params any) (tunnel.RPCResponseEnvelope, error)
	Probe()
}

// EndpointSpec describes a controller endpoint that prefers tunnel RPC with HTTP fallback.
type EndpointSpec struct {
	RPCMethod  string
	HTTPMethod string
	HTTPPath   string
	WSTimeout  time.Duration
}

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
	if client == nil {
		return
	}
	client.mu.Lock()
	client.tunnelTransport = caller
	client.mu.Unlock()
}

func (client *Client) getTunnelTransport() RPCCaller {
	if client == nil {
		return nil
	}
	client.mu.RLock()
	caller := client.tunnelTransport
	client.mu.RUnlock()
	return caller
}

// CallEndpoint dispatches a request via tunnel RPC when negotiated, falling back to signed HTTP.
func (client *Client) CallEndpoint(ctx context.Context, spec EndpointSpec, payload any) (CallResult, error) {
	caller := client.getTunnelTransport()
	fallbackReason := ""

	if caller != nil && spec.RPCMethod != "" {
		if !caller.SupportsRPCMethod(spec.RPCMethod) {
			fallbackReason = "tunnel_unavailable"
		} else {
			wsTimeout := spec.WSTimeout
			if wsTimeout <= 0 {
				wsTimeout = defaultWSTimeout
			}
			wsCtx, cancel := context.WithTimeout(ctx, wsTimeout)
			startedAt := time.Now()
			resp, err := caller.CallRPC(wsCtx, spec.RPCMethod, payload)
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
					if !ok {
						return result, fmt.Errorf("%s failed: status=%d body=%s", spec.RPCMethod, resp.Status, body)
					}
					return result, nil
				}
			} else {
				if ctx.Err() != nil {
					return CallResult{Transport: TransportWS}, err
				}
				if errors.Is(err, context.DeadlineExceeded) {
					caller.Probe()
					fallbackReason = "ws_timeout"
				} else {
					fallbackReason = classifyRPCError(err)
				}
			}
		}
	}

	httpResult, err := client.callHTTP(ctx, spec.HTTPMethod, spec.HTTPPath, payload)
	httpResult.Transport = TransportHTTP
	httpResult.FallbackReason = fallbackReason
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

func formatRPCResponseBody(resp tunnel.RPCResponseEnvelope) string {
	trimmed := bytes.TrimSpace(resp.Data)
	if len(trimmed) > 0 {
		return string(trimmed)
	}
	if resp.Status >= 400 {
		encoded, err := json.Marshal(map[string]any{
			"code":  resp.Code,
			"error": resp.Error,
		})
		if err == nil {
			return string(encoded)
		}
	}
	return "{}"
}

func (client *Client) callHTTP(ctx context.Context, method string, path string, payload any) (CallResult, error) {
	if method == "" {
		method = http.MethodPost
	}

	var body []byte
	var err error
	if method != http.MethodGet || payload != nil {
		if payload == nil {
			body = []byte("{}")
		} else {
			body, err = protocol.CanonicalJSON(payload)
			if err != nil {
				return CallResult{Transport: TransportHTTP}, err
			}
		}
	}

	timestamp := protocol.NewTimestamp()
	nonce, err := protocol.NewNonce()
	if err != nil {
		return CallResult{Transport: TransportHTTP}, err
	}

	bodyHash := protocol.BodySHA256(body)
	signingPayload := protocol.SigningPayload(method, path, timestamp, nonce, bodyHash)
	signature := protocol.Signature(client.agentSecret, signingPayload)

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.panelURL+path, bodyReader)
	if err != nil {
		return CallResult{Transport: TransportHTTP}, err
	}

	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("x-incus-agent-id", client.agentID)
	request.Header.Set("x-incus-timestamp", timestamp)
	request.Header.Set("x-incus-nonce", nonce)
	request.Header.Set("x-incus-body-sha256", bodyHash)
	request.Header.Set("x-incus-signature", signature)

	startedAt := time.Now()
	response, err := client.httpClient.Do(request)
	if err != nil {
		client.resetConnections()
		return CallResult{Transport: TransportHTTP}, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		client.resetConnections()
		return CallResult{Transport: TransportHTTP}, err
	}

	result := CallResult{
		StatusCode: response.StatusCode,
		Body:       string(responseBody),
		OK:         response.StatusCode >= 200 && response.StatusCode < 300,
		Transport:  TransportHTTP,
		LatencyMs:  time.Since(startedAt).Milliseconds(),
	}
	if !result.OK {
		return result, fmt.Errorf("request failed: status=%d body=%s", response.StatusCode, result.Body)
	}
	return result, nil
}
