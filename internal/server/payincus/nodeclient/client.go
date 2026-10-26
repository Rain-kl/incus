// Package nodeclient connects the transport agent to the local, authenticated Incus API.
package nodeclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const prefix = "/1.0/payincus"

// Client provides authenticated access to its peer API.
type Client struct{ HTTP *http.Client }

// New creates an API client using the supplied connection configuration.
func New(socket string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}, ResponseHeaderTimeout: 0}
	return &Client{HTTP: &http.Client{Transport: transport}}
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://incus"+prefix+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	return c.HTTP.Do(req)
}

// Call decodes a synchronous node response and propagates transport errors.
func (c *Client) Call(ctx context.Context, method, path string, body any, target any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	var result struct {
		Type     string          `json:"type"`
		Error    string          `json:"error"`
		Metadata json.RawMessage `json:"metadata"`
	}

	err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&result)
	if err != nil {
		return fmt.Errorf("invalid node API response: %w", err)
	}

	if resp.StatusCode != 200 || result.Type != "sync" {
		return fmt.Errorf("node API status %d: %s", resp.StatusCode, result.Error)
	}

	if target != nil {
		return json.Unmarshal(result.Metadata, target)
	}

	return nil
}

// Check verifies the local daemon advertises the required node extension.
func (c *Client) Check(ctx context.Context) error {
	var result struct {
		Extensions []string `json:"api_extensions"`
	}

	err := c.Call(ctx, http.MethodGet, "", nil, &result)
	if err != nil {
		return err
	}

	for _, extension := range result.Extensions {
		if extension == "payincus_node_v1" {
			return nil
		}
	}

	return fmt.Errorf("local incusd does not support payincus_node_v1")
}

// Stream follows bounded instance diagnostics until cancellation.
func (c *Client) Stream(ctx context.Context, instance string, emit func(string)) error {
	return c.stream(ctx, "/logs?instance="+url.QueryEscape(instance), emit)
}

// Journal relays a fixed service journal over SSE.
func (c *Client) Journal(ctx context.Context, unit string, lines int, emit func(string)) error {
	return c.stream(ctx, fmt.Sprintf("/journal?unit=%s&lines=%d", url.QueryEscape(unit), lines), emit)
}

func (c *Client) stream(ctx context.Context, path string, emit func(string)) error {
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("node log stream status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 8192), 64<<10)
	for scanner.Scan() {
		text := scanner.Text()
		if !strings.HasPrefix(text, "data: ") {
			continue
		}

		var line string
		err = json.Unmarshal([]byte(strings.TrimPrefix(text, "data: ")), &line)
		if err != nil {
			return err
		}

		emit(line)
	}

	return scanner.Err()
}
