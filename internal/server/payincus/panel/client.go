package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/config"
	"github.com/lxc/incus/v7/internal/server/payincus/protocol"
)

const heartbeatPath = "/api/agent/heartbeat"

// Client provides authenticated access to its peer API.
type Client struct {
	panelURL    string
	agentID     string
	agentSecret string
	httpClient  *http.Client
}

// HeartbeatResult contains panel connection instructions and response timing.
type HeartbeatResult struct {
	StatusCode int
	Body       string
	OK         bool
	Tunnel     *TunnelInstruction
	Caddy      *CaddyInstruction
	LatencyMs  int64
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
		panelURL:    strings.TrimRight(cfg.PanelURL, "/"),
		agentID:     cfg.AgentID,
		agentSecret: cfg.AgentSecret,
		httpClient: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
	}
}

// Heartbeat sends a signed report and reads panel instructions.
func (client *Client) Heartbeat(ctx context.Context, payload map[string]any) (HeartbeatResult, error) {
	body, err := protocol.CanonicalJSON(payload)
	if err != nil {
		return HeartbeatResult{}, err
	}

	timestamp := protocol.NewTimestamp()
	nonce, err := protocol.NewNonce()
	if err != nil {
		return HeartbeatResult{}, err
	}

	bodyHash := protocol.BodySHA256(body)
	signingPayload := protocol.SigningPayload(http.MethodPost, heartbeatPath, timestamp, nonce, bodyHash)
	signature := protocol.Signature(client.agentSecret, signingPayload)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.panelURL+heartbeatPath, bytes.NewReader(body))
	if err != nil {
		return HeartbeatResult{}, err
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-incudal-agent-id", client.agentID)
	request.Header.Set("x-incudal-timestamp", timestamp)
	request.Header.Set("x-incudal-nonce", nonce)
	request.Header.Set("x-incudal-body-sha256", bodyHash)
	request.Header.Set("x-incudal-signature", signature)

	startedAt := time.Now()
	response, err := client.httpClient.Do(request)
	if err != nil {
		return HeartbeatResult{}, err
	}

	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return HeartbeatResult{}, err
	}

	result := HeartbeatResult{
		StatusCode: response.StatusCode,
		Body:       string(responseBody),
		OK:         response.StatusCode >= 200 && response.StatusCode < 300,
		LatencyMs:  time.Since(startedAt).Milliseconds(),
	}

	if !result.OK {
		return result, fmt.Errorf("heartbeat failed: status=%d body=%s", response.StatusCode, result.Body)
	}

	var parsedResponse heartbeatResponse
	{
		err := json.Unmarshal(responseBody, &parsedResponse)
		if err == nil {
			result.Tunnel = parsedResponse.Tunnel
			result.Caddy = parsedResponse.Caddy
		}
	}

	var parsed map[string]any
	{
		err := json.Unmarshal(responseBody, &parsed)
		if err == nil {
			parsed["latencyMs"] = result.LatencyMs
			{
				compact, err := json.Marshal(parsed)
				if err == nil {
					result.Body = string(compact)
				}
			}
		}
	}

	return result, nil
}

// SecurityReport authenticates event batches and accepts only explicit durable-delivery acknowledgements.
func (client *Client) SecurityReport(ctx context.Context, payload map[string]any) ([]string, error) {
	const path = "/api/agent/security-report"
	body, err := protocol.CanonicalJSON(payload)
	if err != nil {
		return nil, err
	}
	timestamp := protocol.NewTimestamp()
	nonce, err := protocol.NewNonce()
	if err != nil {
		return nil, err
	}
	hash := protocol.BodySHA256(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.panelURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-incudal-agent-id", client.agentID)
	request.Header.Set("x-incudal-timestamp", timestamp)
	request.Header.Set("x-incudal-nonce", nonce)
	request.Header.Set("x-incudal-body-sha256", hash)
	request.Header.Set("x-incudal-signature", protocol.Signature(client.agentSecret, protocol.SigningPayload("POST", path, timestamp, nonce, hash)))
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("report transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("report status %d", response.StatusCode)
	}
	var result struct {
		Ack []string `json:"ack"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 32768)).Decode(&result)
	if err != nil || result.Ack == nil || len(result.Ack) > 128 {
		return nil, fmt.Errorf("invalid acknowledgement")
	}
	return result.Ack, nil
}
