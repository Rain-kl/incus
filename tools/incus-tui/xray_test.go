package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func xrayAPIServer(t *testing.T, handler http.Handler) {
	t.Helper()
	// Unix sockets cap out near 104 bytes on macOS, and t.TempDir() paths are
	// long enough to exceed it under a nested test name.
	root, err := os.MkdirTemp("", "ix")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
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
		data, err := (&backend{}).xray(ctx, action)
		if err != nil {
			t.Fatalf("Xray %s through local API: %v", action, err)
		}
		if len(data) == 0 {
			t.Fatalf("Xray %s returned no metadata", action)
		}
	}
	want := []request{{method: http.MethodGet, path: "/1.0/payincus/xray"}, {method: http.MethodPost, path: "/1.0/payincus/xray", action: "test"}, {method: http.MethodPost, path: "/1.0/payincus/xray", action: "restart"}, {method: http.MethodPost, path: "/1.0/payincus/xray", action: "reconfigure"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected Xray API calls %#v", calls)
	}
	if strings.Contains(logs.String(), "fixture-private-password") {
		t.Fatal("Xray response credentials reached operation log")
	}
	_, err := (&backend{}).xray(context.Background(), "shell")
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
			_, err := (&backend{}).xray(ctx, "inspect")
			if err == nil {
				t.Fatal("accepted failed, cancelled or malformed Xray response")
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
		})
	}
}

// Xray maintenance actions run immediately with no y/n prompt. The menu order and
// labels are presentation details, so the test locates entries by action.
func TestXrayMenuRunsDirectlyWithoutConfirmation(t *testing.T) {
	entries := func() map[string]int {
		m := model{backend: &backend{}, menuGroup: "xray", width: 100, height: 40}
		found := map[string]int{}
		for i, item := range m.currentMenu() {
			found[item.action] = i
		}
		return found
	}
	cursorOf := entries()
	for _, action := range []string{"xray-inspect", "xray-test", "xray-restart", "xray-reconfigure"} {
		cursor, ok := cursorOf[action]
		if !ok {
			t.Fatalf("Xray menu is missing %s", action)
		}
		m := model{backend: &backend{}, menuGroup: "xray", width: 100, height: 40, cursor: cursor}
		updated, cmd := m.selectMenu()
		m = asModel(t, updated)
		if cmd == nil || !m.busy || m.page == "confirm" || m.confirm != nil {
			t.Fatalf("%s asked for a confirmation instead of running", action)
		}
		if m.cancel == nil {
			t.Fatalf("%s did not schedule a cancellable operation", action)
		}
		m.cancel()
	}

	cursor, ok := cursorOf["xray-logs"]
	if !ok {
		t.Fatal("Xray menu is missing the journal action")
	}
	m := model{backend: &backend{}, menuGroup: "xray", width: 100, height: 40, cursor: cursor}
	updated, cmd := m.selectMenu()
	if cmd == nil || asModel(t, updated).page != "" {
		t.Fatal("journal action did not open the managed Xray log view")
	}
}

func TestXrayOperationKeepsTheLogOffScreen(t *testing.T) {
	m := model{backend: &backend{}, menuGroup: "xray", status: "服务正常", width: 100, height: 40}
	m.cursor = 1
	updated, cmd := m.selectMenu()
	m = asModel(t, updated)
	if cmd == nil || !m.busy || !m.quiet {
		t.Fatal("Xray operation did not enter the quiet progress state")
	}
	view := m.View()
	if strings.Contains(view, "操作日志") || !strings.Contains(view, "正在执行 Xray 操作") {
		t.Fatalf("Xray operation exposed the operation log: %q", view)
	}
	m.cancel()
}

func TestXrayReportRendersFriendlySummary(t *testing.T) {
	inspect := []byte(`{"status":{"state":"applied","appliedRevision":12,"error":"none","xrayAvailable":true,"activeNodeId":3,"healthyNodeIds":[1,3],"managedNetworks":["igw"],"routedFallback":false},"nodes":[{"id":1,"protocol":"socks","address":"proxy.example","port":1080,"password":"ignored"}]}`)
	lines, err := xrayReport("inspect", inspect)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{"Xray 节点与运行状态", "状态：已应用", "策略版本：12", "Xray 核心：可用", "活动节点：#3", "健康节点：#1 #3", "代理节点（1）", "#1", "socks", "proxy.example:1080"} {
		if !strings.Contains(text, want) {
			t.Fatalf("inspect report missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "ignored") {
		t.Fatal("inspect report leaked outbound credentials")
	}

	test := []byte(`{"items":[{"nodeId":2,"success":false,"latencyMs":0,"error":"timeout"},{"nodeId":1,"success":true,"latencyMs":42}]}`)
	lines, err = xrayReport("test", test)
	if err != nil {
		t.Fatal(err)
	}
	text = strings.Join(lines, "\n")
	for _, want := range []string{"Xray 代理节点测试", "共 2 个节点，成功 1，失败 1", "#1  成功  42 ms", "#2  失败  timeout"} {
		if !strings.Contains(text, want) {
			t.Fatalf("test report missing %q in %q", want, text)
		}
	}

	status := []byte(`{"state":"error","appliedRevision":9,"xrayAvailable":false,"error":"core_failed","errorStage":"health"}`)
	lines, err = xrayReport("reconfigure", status)
	if err != nil {
		t.Fatal(err)
	}
	text = strings.Join(lines, "\n")
	for _, want := range []string{"Xray 运行环境已重建", "状态：异常", "Xray 核心：不可用", "错误：core_failed（阶段：health）"} {
		if !strings.Contains(text, want) {
			t.Fatalf("reconfigure report missing %q in %q", want, text)
		}
	}

	if _, err := xrayReport("shell", []byte(`{}`)); err == nil {
		t.Fatal("unknown action produced a report")
	}

	idle := []byte(`{"state":"idle","appliedRevision":0,"xrayAvailable":true,"managedNetworks":[],"healthyNodeIds":[]}`)
	lines, err = xrayReport("reconfigure", idle)
	if err != nil {
		t.Fatal(err)
	}
	text = strings.Join(lines, "\n")
	if !strings.Contains(text, "Xray 运行环境已重建") || !strings.Contains(text, "尚未下发代理策略") {
		t.Fatalf("policy-free rebuild did not explain itself: %q", text)
	}
}

func TestXrayResultShowsReportInsteadOfLogs(t *testing.T) {
	m := model{backend: &backend{runner: &fakeRunner{}}, menuGroup: "xray", busy: true}
	updated, _ := m.Update(resultMsg{report: []string{"Xray 节点与运行状态", "", "状态：已应用"}})
	m = asModel(t, updated)
	if m.showLogs || m.page != "report" || m.message != "" || !strings.Contains(m.View(), "状态：已应用") {
		t.Fatal("Xray result fell back to the operation log")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, updated)
	if m.page != "" || m.report != nil || m.menuGroup != "xray" {
		t.Fatal("report page did not return to the Xray menu")
	}
	if !strings.Contains(m.View(), "Xray 管理") {
		t.Fatal("returning from the report did not show the Xray menu")
	}
}
