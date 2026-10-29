package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func xrayAPIServer(t *testing.T, handler http.Handler) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("INCUS_DIR", root)
	listener, err := net.Listen("unix", filepath.Join(root, "unix.socket"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
}

func TestXrayBackendUsesLocalIncusAPIAndRedactsResponse(t *testing.T) {
	type request struct {
		method string
		path   string
		action string
	}
	var calls []request
	xrayAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := request{method: r.Method, path: r.URL.Path}
		if r.Method == http.MethodPost {
			var body map[string]string
			err := json.NewDecoder(r.Body).Decode(&body)
			if err != nil {
				t.Error(err)
			}
			call.action = body["action"]
		}
		calls = append(calls, call)
		metadata := map[string]any{"state": "applied", "nodes": []any{map[string]any{"id": 1, "protocol": "socks", "address": "proxy.example", "port": 1080, "password": "fixture-private-password"}}}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": metadata})
	}))
	var logs strings.Builder
	ctx := context.WithValue(context.Background(), operationLogKey{}, &operationLog{writer: &logs})
	for _, action := range []string{"inspect", "test", "restart", "reconfigure"} {
		err := (&backend{}).xray(ctx, action)
		if err != nil {
			t.Fatalf("Xray %s through local API: %v", action, err)
		}
	}
	want := []request{{method: http.MethodGet, path: "/1.0/payincus/xray"}, {method: http.MethodPost, path: "/1.0/payincus/xray", action: "test"}, {method: http.MethodPost, path: "/1.0/payincus/xray", action: "restart"}, {method: http.MethodPost, path: "/1.0/payincus/xray", action: "reconfigure"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected Xray API calls %#v", calls)
	}
	if strings.Contains(logs.String(), "fixture-private-password") {
		t.Fatal("Xray response credentials reached operation log")
	}
	err := (&backend{}).xray(context.Background(), "shell")
	if err == nil {
		t.Fatal("unsupported Xray action reached the local API")
	}
}

func TestXrayBackendPreservesAPIErrorCancellationAndDecodeFailure(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		cancel bool
	}{
		{name: "HTTP failure", status: http.StatusServiceUnavailable, body: `{"type":"error","error":"maintenance fixture failed"}`},
		{name: "invalid response", status: http.StatusOK, body: `not-json`},
		{name: "cancelled", status: http.StatusOK, body: ``, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			xrayAPIServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			ctx := context.Background()
			if test.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			err := (&backend{}).xray(ctx, "inspect")
			if err == nil {
				t.Fatal("accepted failed, cancelled or malformed Xray response")
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
		})
	}
}

func TestXrayMenuRequiresConfirmationAndOffersLogs(t *testing.T) {
	m := model{backend: &backend{}, menuGroup: "xray", width: 100, height: 40}
	items := m.currentMenu()
	if len(items) != 6 || items[0].action != "xray-inspect" || items[1].action != "xray-test" || items[2].action != "xray-restart" || items[3].action != "xray-logs" || items[4].action != "xray-reconfigure" {
		t.Fatalf("unexpected Xray menu: %#v", items)
	}
	if !strings.Contains(m.View(), "Xray 管理") || !strings.Contains(m.View(), "查看 Xray 日志") {
		t.Fatal("Xray actions are not visible in the TUI")
	}
	m.cursor = 2
	updated, cmd := m.selectMenu()
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" || m.confirm == nil || !strings.Contains(m.message, "restart") {
		t.Fatal("Xray restart bypassed the confirmation page")
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = asModel(t, updated)
	if cmd != nil || m.confirm != nil {
		t.Fatal("cancel left the Xray action armed")
	}
	m.menuGroup, m.cursor = "xray", 3
	updated, cmd = m.selectMenu()
	if cmd == nil || asModel(t, updated).page != "" {
		t.Fatal("journal action did not open the managed Xray log view")
	}
}
