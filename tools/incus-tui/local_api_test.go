package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func apiFixture(t *testing.T, handler http.HandlerFunc) *localClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// Keep request paths and bodies unchanged while routing the fixture to the test server.
	return &localClient{http: &http.Client{Transport: rewriteTransport{base: http.DefaultTransport, url: server.URL}}}
}

type rewriteTransport struct {
	base http.RoundTripper
	url  string
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	destination := strings.TrimPrefix(r.url, "http://")
	clone.URL.Host = destination
	return r.base.RoundTrip(clone)
}

func syncJSON(w http.ResponseWriter, value any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": value})
}

func TestLocalAdapterQueryInitAndAsyncContract(t *testing.T) {
	calls := []string{}
	wait := 0
	c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Method == "GET" && (r.URL.Path == "/1.0/storage-pools" || r.URL.Path == "/1.0/networks") {
			syncJSON(w, []any{})
			return
		}
		if r.URL.Path == "/1.0/operations/op/wait" {
			wait++
			code := 105
			if wait > 1 {
				code = 200
			}
			syncJSON(w, map[string]any{"status_code": code})
			return
		}
		if r.URL.Path == "/1.0/profiles/default" && r.Method == "GET" {
			w.Header().Set("ETag", "profile-1")
			syncJSON(w, map[string]any{"config": map[string]string{}, "devices": map[string]any{}})
			return
		}
		if r.Method == "POST" {
			var data map[string]any
			if json.NewDecoder(r.Body).Decode(&data) != nil {
				t.Error("invalid JSON body")
			}
			if r.URL.Path == "/1.0/storage-pools" && data["driver"] != "dir" {
				t.Error("unexpected default driver")
			}
			if r.URL.Path == "/1.0/networks" {
				config, _ := data["config"].(map[string]any)
				if config["ipv4.address"] != panelGateway || config["ipv4.nat"] != "true" {
					t.Error("initialization must use the panel IPv4 pool, never a random subnet")
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "operation": "/1.0/operations/op"})
			return
		}
		if r.Method == "PUT" && r.Header.Get("If-Match") != "profile-1" {
			t.Error("missing profile precondition")
		}
		syncJSON(w, map[string]any{})
	})
	_, err := c.runAdapter(context.Background(), []string{"--force-local", "admin", "init", "--preseed"}, preseed)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"GET /1.0/storage-pools", "GET /1.0/profiles/default", "GET /1.0/networks?recursion=1", "POST /1.0/networks", "GET /1.0/operations/op/wait?timeout=30", "GET /1.0/operations/op/wait?timeout=30", "POST /1.0/storage-pools", "GET /1.0/operations/op/wait?timeout=30", "PUT /1.0/profiles/default"}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatal(calls)
	}
	{
		_, err := c.runAdapter(context.Background(), []string{"query", "/1.0/payincus"}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalAdapterConfigAndTrust(t *testing.T) {
	c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("ETag", "server-1")
			syncJSON(w, map[string]any{"config": map[string]string{"existing": "keep"}})
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatal("invalid body")
		}
		if r.Method == "PUT" {
			cfg, ok := body["config"].(map[string]any)
			if !ok {
				t.Fatal("invalid config type")
			}
			if cfg["existing"] != "keep" || cfg["core.https_address"] != "127.0.0.1:8443" || r.Header.Get("If-Match") != "server-1" {
				t.Error("config or concurrency guard lost")
			}
		}
		if r.URL.Path == "/1.0/certificates" && (body["name"] != "panel" || body["type"] != "client" || body["certificate"] != "YWJj") {
			t.Error("certificate contract changed")
		}
		syncJSON(w, nil)
	})
	_, err := c.runAdapter(context.Background(), []string{"config", "set", "core.https_address", "127.0.0.1:8443"}, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.crt")
	if os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\nYWJj\n-----END CERTIFICATE-----\n"), 0o600) != nil {
		t.Fatal("fixture write failed")
	}
	_, err = c.runAdapter(context.Background(), []string{"config", "trust", "add-certificate", path, "--name", "panel"}, "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalAdapterFailureAndCancellation(t *testing.T) {
	for _, body := range []string{`{"type":"error","error":"bridge creation denied"}`, `not json`, `{"type":"async","operation":"https://other.invalid/"}`, `{"type":"async","operation":"/1.0/operations/o?other=x"}`} {
		c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		_, err := c.call(context.Background(), "GET", "/1.0", nil)
		if err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	{
		_, err := c.call(context.Background(), "GET", "/1.0", nil)
		if err == nil {
			t.Fatal("ignored permission failure")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		_, err := c.call(ctx, "GET", "/1.0", nil)
		if err == nil {
			t.Fatal("ignored cancellation")
		}
	}
}

func TestLocalAPIPreservesErrorsAndFullLogs(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		var logs strings.Builder
		ctx := context.WithValue(context.Background(), operationLogKey{}, &operationLog{writer: &logs})
		c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if asynchronous && r.Method == http.MethodPost {
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "operation": "/1.0/operations/op"})
				return
			}
			if asynchronous {
				syncJSON(w, map[string]any{"status_code": 400, "err": "dnsmasq: permission denied creating incusbr0"})
				return
			}

			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "dnsmasq: permission denied creating incusbr0", "error_code": 500})
		})
		_, err := c.call(ctx, http.MethodPost, "/1.0/networks", map[string]any{"name": "incusbr0"})
		if err == nil || !strings.Contains(err.Error(), "dnsmasq: permission denied creating incusbr0") || !strings.Contains(err.Error(), "POST /1.0/networks") {
			t.Fatalf("original API diagnostics were lost: %v", err)
		}
		if !strings.Contains(logs.String(), "permission denied creating incusbr0") || !strings.Contains(logs.String(), `"name":"incusbr0"`) {
			t.Fatal("request or error response missing from operation log")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := c.call(ctx, http.MethodGet, "/1.0", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
}

func TestZFSConfirmationFingerprintAndReadOnlyKey(t *testing.T) {
	m := model{backend: &backend{runner: &fakeRunner{}}, menuGroup: "zfs", cursor: 2}
	updated, cmd := m.zfsMenu("zfs-create")
	m = asModel(t, updated)
	if cmd != nil || m.page != "zfs-create" || len(m.fields) != 3 || !m.fields[1].readonly {
		t.Fatal("missing enrollment step")
	}
	key := m.fields[1].value
	m.focus = 1
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = asModel(t, updated)
	if m.fields[1].value != key {
		t.Fatal("generated key was editable")
	}
	updated, cmd = m.submitZFS([]string{"tank/private", key, "wrong"})
	m = asModel(t, updated)
	if cmd != nil || m.page == "confirm" {
		t.Fatal("accepted missing control-plane storage confirmation")
	}
	updated, cmd = m.submitZFS([]string{"tank/private", key, keyFingerprint(key)})
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" || strings.Contains(m.View(), key) {
		t.Fatal("creation bypassed confirmation or retained key on display")
	}
	b := &backend{runner: &fakeRunner{reply: func(call) ([]byte, error) { return nil, errors.New("unexpected") }}}
	if b.createPool(context.Background(), "pool", "/dev/test", "1") == nil || b.createPool(context.Background(), "../pool", "", "1") == nil {
		t.Fatal("unsafe pool request accepted")
	}
}

func TestZFSDependencyInstallationContracts(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "os-release")
	source := filepath.Join(dir, "debian.sources")
	original := "Types: deb\nURIs: https://deb.debian.org/debian\nSuites: trixie\nComponents: main\nSigned-By: /keyring\n"
	if os.WriteFile(release, []byte("ID=debian\nVERSION_ID=13\n"), 0o600) != nil || os.WriteFile(source, []byte(original), 0o600) != nil {
		t.Fatal("fixture write failed")
	}
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		if c.name == "uname" {
			return []byte("6.12-fixture\n"), nil
		}
		if c.name == "apt-cache" {
			return []byte("Package: linux-headers-6.12-fixture\n"), nil
		}
		return nil, nil
	}}
	b := &backend{runner: f, releasePath: release, sourcePath: source, modulesPath: filepath.Join(dir, "modules"), sysModulesPath: filepath.Join(dir, "sys-module")}
	{
		err := b.prepareZFS(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	saved, err := os.ReadFile(source + ".before-incus-tui")
	if err != nil || string(saved) != original {
		t.Fatal("APT backup changed")
	}
	updated, err := os.ReadFile(source)
	if err != nil || !strings.Contains(string(updated), "Components: main contrib") || !strings.Contains(string(updated), "Signed-By: /keyring") {
		t.Fatal("APT source identity lost")
	}
	install := false
	for _, c := range f.calls {
		if c.name == "apt-get" && strings.Contains(strings.Join(c.args, " "), "linux-headers-6.12-fixture") {
			install = true
		}
	}
	if !install {
		t.Fatal("missing matching kernel dependency")
	}
	if os.WriteFile(release, []byte("ID=ubuntu\nVERSION_ID=24.04\n"), 0o600) != nil {
		t.Fatal("fixture write failed")
	}
	moduleCalls := 0
	f.reply = func(c call) ([]byte, error) {
		if c.name == "uname" {
			return []byte("6.8-fixture"), nil
		}
		if c.name == "modprobe" {
			moduleCalls++
		}
		if c.name == "apt-cache" {
			return []byte("Package: linux-headers-6.8-fixture\n"), nil
		}
		return nil, nil
	}
	{
		err := b.prepareZFS(context.Background())
		if err != nil || moduleCalls != 1 {
			t.Fatal("Ubuntu DKMS dependency check failed", err)
		}
	}
	if os.WriteFile(release, []byte("ID=debian\nVERSION_ID=12\n"), 0o600) != nil {
		t.Fatal("fixture write failed")
	}
	f.calls = nil
	if b.prepareZFS(context.Background()) == nil || len(f.calls) > 0 {
		t.Fatal("unsupported target changed host")
	}
}
