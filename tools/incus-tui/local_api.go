package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type localClient struct {
	http  *http.Client
	etags map[string]string
}

func newLocalClient() *localClient {
	root := os.Getenv("INCUS_DIR")
	if root == "" {
		root = "/var/lib/incus"
	}
	socket := filepath.Join(root, "unix.socket")
	return &localClient{http: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}}}
}

func (c *localClient) call(ctx context.Context, method, path string, input any) (json.RawMessage, error) {
	var payload []byte
	var err error
	if input != nil {
		payload, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
	}
	logOperation(ctx, "Incus API -> %s %s", method, path)
	if len(payload) > 0 {
		logOperation(ctx, "request: %s", safePayload(ctx, payload))
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://incus"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if method == "PUT" && c.etags != nil && c.etags[path] != "" {
		request.Header.Set("If-Match", c.etags[path])
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Incus API %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	if method == "GET" && response.Header.Get("ETag") != "" {
		if c.etags == nil {
			c.etags = map[string]string{}
		}
		c.etags[path] = response.Header.Get("ETag")
	}
	var envelope struct {
		Type      string          `json:"type"`
		Metadata  json.RawMessage `json:"metadata"`
		Operation string          `json:"operation"`
		Error     string          `json:"error"`
	}
	data, err := io.ReadAll(response.Body)
	logOperation(ctx, "Incus API <- %s %s HTTP %d\n%s", method, path, response.StatusCode, safePayload(ctx, data))
	if err != nil {
		return nil, fmt.Errorf("Incus API %s %s HTTP %d: reading response: %w", method, path, response.StatusCode, err)
	}

	err = json.Unmarshal(data, &envelope)
	if err != nil {
		return nil, fmt.Errorf("Incus API %s %s HTTP %d: decoding response: %w\n%s", method, path, response.StatusCode, err, safePayload(ctx, data))
	}
	if response.StatusCode >= 400 || envelope.Type == "error" {
		return nil, fmt.Errorf("Incus API %s %s HTTP %d: %s", method, path, response.StatusCode, safePayload(ctx, data))
	}
	if envelope.Type == "async" {
		if !strings.HasPrefix(envelope.Operation, "/1.0/operations/") || strings.ContainsAny(envelope.Operation, "?#") {
			return nil, errors.New("无效操作响应")
		}
		for {
			data, err := c.call(ctx, "GET", envelope.Operation+"/wait?timeout=30", nil)
			if err != nil {
				return nil, err
			}
			var operation struct {
				StatusCode int    `json:"status_code"`
				Err        string `json:"err"`
			}
			err = json.Unmarshal(data, &operation)
			if err != nil {
				return nil, err
			}
			switch operation.StatusCode {
			case 200:
				return data, nil
			case 100, 103, 104, 105:
			case 400, 401:
				return nil, fmt.Errorf("Incus API %s %s operation %s status %d: %s", method, path, envelope.Operation, operation.StatusCode, contextLog(ctx).redact(operation.Err))
			default:
				return nil, fmt.Errorf("Incus API %s %s operation %s: unexpected status %d\n%s", method, path, envelope.Operation, operation.StatusCode, safePayload(ctx, data))
			}
		}
	}
	if envelope.Type != "sync" {
		return nil, fmt.Errorf("Incus API %s %s HTTP %d: unexpected response type %q\n%s", method, path, response.StatusCode, envelope.Type, safePayload(ctx, data))
	}
	return envelope.Metadata, nil
}

func (c *localClient) config(ctx context.Context) (map[string]string, error) {
	data, err := c.call(ctx, "GET", "/1.0", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Config map[string]string `json:"config"`
	}
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, fmt.Errorf("解析本机配置：%w", err)
	}
	if result.Config == nil {
		return nil, errors.New("本机配置格式不兼容")
	}
	return result.Config, nil
}

// runIncusAdapter replaces TUI subprocess CLI calls so ordinary setup cannot trip its own policy.
func runIncusAdapter(ctx context.Context, args []string, input string) ([]byte, error) {
	c := newLocalClient()
	defer c.http.CloseIdleConnections()
	return c.runAdapter(ctx, args, input)
}

func (c *localClient) runAdapter(ctx context.Context, args []string, input string) ([]byte, error) {
	if len(args) > 0 && args[0] == "--force-local" {
		args = args[1:]
	}
	if len(args) == 2 && args[0] == "query" {
		return c.call(ctx, "GET", args[1], nil)
	}
	if len(args) == 3 && args[0] == "admin" && args[1] == "init" && args[2] == "--preseed" {
		if input != preseed {
			return nil, errors.New("不支持的初始化配置")
		}
		_, err := c.call(ctx, "POST", "/1.0/storage-pools", map[string]any{"name": "default", "driver": "dir", "config": map[string]string{}})
		if err != nil {
			return nil, err
		}
		_, err = c.call(ctx, "POST", "/1.0/networks", map[string]any{"name": "incusbr0", "type": "bridge", "config": map[string]string{"ipv4.address": "auto", "ipv4.nat": "true", "ipv6.address": "none"}})
		if err != nil {
			return nil, err
		}
		profile, err := c.call(ctx, "GET", "/1.0/profiles/default", nil)
		if err != nil {
			return nil, err
		}
		var current struct {
			Config  map[string]string `json:"config"`
			Devices map[string]any    `json:"devices"`
		}
		err = json.Unmarshal(profile, &current)
		if err != nil {
			return nil, fmt.Errorf("解析默认 profile：%w", err)
		}
		if current.Config == nil || current.Devices == nil || len(current.Config) > 0 || len(current.Devices) > 0 {
			return nil, errors.New("默认 profile 已变化，拒绝覆盖")
		}
		return c.call(ctx, "PUT", "/1.0/profiles/default", map[string]any{"config": map[string]string{}, "devices": map[string]any{"root": map[string]string{"type": "disk", "path": "/", "pool": "default"}, "eth0": map[string]string{"type": "nic", "name": "eth0", "network": "incusbr0"}}})
	}
	if len(args) >= 3 && args[0] == "config" {
		if args[1] == "get" && len(args) == 3 {
			config, err := c.config(ctx)
			if err != nil {
				return nil, err
			}
			return []byte(config[args[2]]), nil
		}
		if args[1] == "set" && len(args) == 4 {
			config, err := c.config(ctx)
			if err != nil {
				return nil, err
			}
			config[args[2]] = args[3]
			return c.call(ctx, "PUT", "/1.0", map[string]any{"config": config})
		}
		if args[1] == "trust" && args[2] == "add-certificate" && len(args) == 6 {
			data, err := os.ReadFile(args[3])
			if err != nil {
				return nil, err
			}
			block, _ := pem.Decode(data)
			if block == nil {
				return nil, errors.New("无效证书")
			}
			return c.call(ctx, "POST", "/1.0/certificates", map[string]any{"type": "client", "name": "panel", "certificate": base64.StdEncoding.EncodeToString(block.Bytes)})
		}
	}
	return nil, fmt.Errorf("不支持的本机 API 适配操作")
}
