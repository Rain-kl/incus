package comm

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lxc/incus/v7/internal/server/agent/protocol"
)

type httpTransport struct {
	mu          sync.RWMutex
	baseURL     string
	agentID     string
	agentSecret string
	client      *http.Client
}

func newHTTPTransport(cfg Config) *httpTransport {
	dialer := &net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 15 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		MaxIdleConns:          10,
		IdleConnTimeout:       20 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}

	return &httpTransport{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		agentID:     cfg.AgentID,
		agentSecret: cfg.AgentSecret,
		client: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
	}
}

func (t *httpTransport) getBaseURL() string {
	t.mu.RLock()
	url := t.baseURL
	t.mu.RUnlock()
	return url
}

func (t *httpTransport) setBaseURL(rawURL string) {
	t.mu.Lock()
	t.baseURL = strings.TrimRight(rawURL, "/")
	t.mu.Unlock()
}

func (t *httpTransport) resetConnections() {
	if t == nil || t.client == nil {
		return
	}
	t.client.CloseIdleConnections()
}

func (t *httpTransport) call(ctx context.Context, method, path string, payload any, maxResponseBytes int64) (CallResult, error) {
	if method == "" {
		method = http.MethodPost
	}
	if maxResponseBytes <= 0 {
		maxResponseBytes = DefaultMaxResponseBytes
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
	signature := protocol.Signature(t.agentSecret, signingPayload)

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, t.getBaseURL()+path, bodyReader)
	if err != nil {
		return CallResult{Transport: TransportHTTP}, err
	}

	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("x-incus-agent-id", t.agentID)
	request.Header.Set("x-incus-timestamp", timestamp)
	request.Header.Set("x-incus-nonce", nonce)
	request.Header.Set("x-incus-body-sha256", bodyHash)
	request.Header.Set("x-incus-signature", signature)

	startedAt := time.Now()
	response, err := t.client.Do(request)
	if err != nil {
		t.resetConnections()
		return CallResult{Transport: TransportHTTP}, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		t.resetConnections()
		return CallResult{Transport: TransportHTTP}, err
	}

	result := CallResult{
		StatusCode: response.StatusCode,
		Body:       responseBody,
		OK:         response.StatusCode >= 200 && response.StatusCode < 300,
		Transport:  TransportHTTP,
		LatencyMs:  time.Since(startedAt).Milliseconds(),
	}
	if !result.OK {
		return result, fmt.Errorf("request failed: status=%d body=%s", response.StatusCode, string(responseBody))
	}
	return result, nil
}
