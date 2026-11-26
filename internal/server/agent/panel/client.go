package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/comm"
	"github.com/lxc/incus/v7/internal/server/agent/config"
)

const heartbeatPath = "/api/agent/heartbeat"

var heartbeatEndpointSpec = EndpointSpec{
	RPCMethod:  "agent.heartbeat",
	HTTPMethod: http.MethodPost,
	HTTPPath:   heartbeatPath,
	WSTimeout:  defaultWSTimeout,
}

var securityReportEndpointSpec = EndpointSpec{
	RPCMethod:        "agent.security.report",
	HTTPMethod:       http.MethodPost,
	HTTPPath:         "/api/agent/security-report",
	WSTimeout:        10 * time.Second,
	MaxResponseBytes: 32768,
}

// Client provides authenticated access to its peer API via the unified comm layer.
type Client struct {
	comm               *comm.Client
	agentID            string
	agentSecret        string
	recoveryBlocked    atomic.Bool
	recoveryGeneration atomic.Uint64
}

// HeartbeatResult contains panel connection instructions and response timing.
type HeartbeatResult struct {
	StatusCode     int
	Body           string
	OK             bool
	Tunnel         *TunnelInstruction
	Caddy          *CaddyInstruction
	LatencyMs      int64
	Transport      string
	FallbackReason string
}

// CaddyInstruction requests a fixed Caddy lifecycle action.
type CaddyInstruction struct {
	Command string `json:"command"`
	Port    int    `json:"port"`
}

// TunnelInstruction configures the reverse connection target.
type TunnelInstruction struct {
	Enabled    bool   `json:"enabled"`
	TargetHost string `json:"targetHost"`
	TargetPort int    `json:"targetPort"`
}

type heartbeatResponse struct {
	Tunnel *TunnelInstruction `json:"tunnel"`
	Caddy  *CaddyInstruction  `json:"caddy"`
}

// New creates an API client using the supplied connection configuration.
func New(cfg config.Config) *Client {
	return &Client{
		comm: comm.New(comm.Config{
			BaseURL:        cfg.PanelURL,
			AgentID:        cfg.AgentID,
			AgentSecret:    cfg.AgentSecret,
			RequestTimeout: cfg.RequestTimeout,
			OnTransportChange: func(endpoint, from, to, reason string) {
				if endpoint == heartbeatEndpointSpec.RPCMethod {
					return
				}
				if to == comm.TransportWS {
					log.Printf("%s transport upgraded to %s", endpoint, to)
				} else if from == comm.TransportWS {
					log.Printf("%s transport fell back to %s (%s)", endpoint, to, reason)
				}
			},
		}),
		agentID:     cfg.AgentID,
		agentSecret: cfg.AgentSecret,
	}
}

// Heartbeat sends a signed report via tunnel RPC when available, falling back to HTTP.
func (client *Client) Heartbeat(ctx context.Context, payload map[string]any) (HeartbeatResult, error) {
	res, err := client.CallEndpoint(ctx, heartbeatEndpointSpec, payload)
	result := HeartbeatResult{
		StatusCode:     res.StatusCode,
		Body:           res.Body,
		OK:             res.OK,
		LatencyMs:      res.LatencyMs,
		Transport:      res.Transport,
		FallbackReason: res.FallbackReason,
	}
	if err != nil {
		return result, err
	}

	rawBytes := []byte(result.Body)
	var parsedResponse heartbeatResponse
	parseErr := json.Unmarshal(rawBytes, &parsedResponse)
	if parseErr == nil {
		result.Tunnel = parsedResponse.Tunnel
		result.Caddy = parsedResponse.Caddy
	}

	var parsed map[string]any
	mapErr := json.Unmarshal(rawBytes, &parsed)
	if mapErr == nil {
		parsed["latencyMs"] = result.LatencyMs
		compact, marshalErr := json.Marshal(parsed)
		if marshalErr == nil {
			result.Body = string(compact)
		}
	}

	return result, nil
}

// SecurityReport authenticates event batches via WS tunnel RPC when negotiated with signed HTTP fallback.
func (client *Client) SecurityReport(ctx context.Context, payload map[string]any) ([]string, error) {
	res, err := client.CallEndpoint(ctx, securityReportEndpointSpec, payload)
	if err != nil {
		if res.StatusCode > 0 {
			return nil, fmt.Errorf("report status %d", res.StatusCode)
		}
		return nil, fmt.Errorf("report transport failed")
	}

	var result struct {
		Ack                []string `json:"ack"`
		RecoveryBlocked    bool     `json:"recoveryBlocked"`
		RecoveryGeneration uint64   `json:"recoveryGeneration"`
	}
	if json.Unmarshal([]byte(res.Body), &result) != nil || result.Ack == nil || len(result.Ack) > 128 {
		return nil, fmt.Errorf("invalid acknowledgement")
	}
	client.recoveryGeneration.Store(result.RecoveryGeneration)
	client.recoveryBlocked.Store(result.RecoveryBlocked)
	return result.Ack, nil
}

// RecoveryBlocked exposes the authenticated controller decision from the last acknowledged report.
func (client *Client) RecoveryBlocked() bool { return client.recoveryBlocked.Load() }

// RecoveryGeneration identifies the latest authenticated controller decision.
func (client *Client) RecoveryGeneration() uint64 { return client.recoveryGeneration.Load() }
