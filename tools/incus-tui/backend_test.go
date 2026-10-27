package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type call struct {
	name  string
	args  []string
	input string
}
type fakeRunner struct {
	calls []call
	reply func(call) ([]byte, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, input string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c := call{name, args, input}
	f.calls = append(f.calls, c)
	if f.reply != nil {
		if name == "/usr/bin/incus" {
			if len(c.args) == 0 || c.args[0] != "--force-local" {
				return nil, errors.New("missing local socket guard")
			}
			c.args = c.args[1:]
		}
		return f.reply(c)
	}
	return nil, nil
}

func fixture(c call) ([]byte, error) {
	switch strings.Join(c.args, " ") {
	case "query /1.0":
		return []byte(`{"environment":{"server_clustered":false}}`), nil
	case "query /1.0/storage-pools", "query /1.0/instances?all-projects=true":
		return []byte(`[]`), nil
	case "query /1.0/networks?recursion=1":
		return []byte(`[{"name":"eth0","managed":false}]`), nil
	case "query /1.0/profiles/default":
		return []byte(`{"config":{},"devices":{}}`), nil
	}
	return nil, nil
}

func TestInitializeContract(t *testing.T) {
	f := &fakeRunner{reply: fixture}
	b := &backend{runner: f}
	{
		err := b.initialize(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	expected := []call{
		{"systemctl", []string{"enable", "--now", "incus"}, ""},
		{"/usr/bin/incus", []string{"query", "/1.0"}, ""},
		{"/usr/bin/incus", []string{"query", "/1.0/storage-pools"}, ""},
		{"/usr/bin/incus", []string{"query", "/1.0/instances?all-projects=true"}, ""},
		{"/usr/bin/incus", []string{"query", "/1.0/networks?recursion=1"}, ""},
		{"/usr/bin/incus", []string{"query", "/1.0/profiles/default"}, ""},
		{"/usr/bin/incus", []string{"admin", "init", "--preseed"}, preseed},
	}
	for i := range expected {
		if expected[i].name == "/usr/bin/incus" {
			expected[i].args = append([]string{"--force-local"}, expected[i].args...)
		}
	}
	if !reflect.DeepEqual(f.calls, expected) {
		t.Fatalf("unexpected contract: %#v", f.calls)
	}
}

func TestInitializeRefusesExistingAndMalformedState(t *testing.T) {
	for _, tt := range []struct{ query, body string }{
		{"/1.0/storage-pools", `["/1.0/storage-pools/default"]`},
		{"/1.0/storage-pools", `{"metadata":[]}`},
		{"/1.0/storage-pools", `null`},
		{"/1.0", `{"environment":{"server_clustered":true}}`},
		{"/1.0", `{}`},
		{"/1.0/networks?recursion=1", `[{"name":"eth0"}]`},
		{"/1.0/profiles/default", `{"config":null,"devices":{}}`},
		{"/1.0/instances?all-projects=true", `["/1.0/instances/c1"]`},
		{"/1.0/networks?recursion=1", `[{"name":"br0","managed":true}]`},
		{"/1.0/networks?recursion=1", `[{"name":"incusbr0","managed":false}]`},
		{"/1.0/networks?recursion=1", `{}`},
		{"/1.0/profiles/default", `{"config":{"limits.cpu":"2"},"devices":{}}`},
		{"/1.0/profiles/default", `{"config":{},"devices":{"eth0":{"type":"nic"}}}`},
		{"/1.0/profiles/default", `{}`},
	} {
		t.Run(tt.query+tt.body, func(t *testing.T) {
			f := &fakeRunner{reply: func(c call) ([]byte, error) {
				if strings.Join(c.args, " ") == "query "+tt.query {
					return []byte(tt.body), nil
				}
				return fixture(c)
			}}
			if (&backend{runner: f}).initialize(context.Background()) == nil {
				t.Fatal("accepted unsafe or incompatible state")
			}
			for _, c := range f.calls {
				if c.input != "" {
					t.Fatal("initialization write occurred")
				}
			}
		})
	}
}

func TestInitializePropagatesInterfaceErrors(t *testing.T) {
	for _, fail := range []string{"enable --now incus", "query /1.0/storage-pools", "query /1.0/instances?all-projects=true", "query /1.0/networks?recursion=1", "query /1.0/profiles/default", "admin init --preseed"} {
		f := &fakeRunner{reply: func(c call) ([]byte, error) {
			if strings.Join(c.args, " ") == fail {
				return nil, errors.New("permission denied")
			}
			return fixture(c)
		}}
		if (&backend{runner: f}).initialize(context.Background()) == nil {
			t.Fatalf("ignored %s failure", fail)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if (&backend{runner: &fakeRunner{}}).initialize(ctx) == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestServicesAndListenerContracts(t *testing.T) {
	for _, unit := range []string{"incus", "incudal-agent"} {
		for _, action := range []string{"start", "stop", "restart", "enable", "disable"} {
			f := &fakeRunner{}
			b := &backend{runner: f}
			if b.service(context.Background(), unit, action) != nil {
				t.Fatal("valid service rejected")
			}
			args := []string{action, unit}
			if action == "enable" {
				args = []string{"enable", "--now", unit}
			}
			if !reflect.DeepEqual(f.calls, []call{{"systemctl", args, ""}}) {
				t.Fatal(f.calls)
			}
		}
	}
	f := &fakeRunner{}
	b := &backend{runner: f}
	if b.service(context.Background(), "sshd", "stop") == nil || b.service(context.Background(), "incus", "reset-failed") == nil || len(f.calls) != 0 {
		t.Fatal("unbounded service interface")
	}
	for _, current := range []string{"", "127.0.0.1:8443", "[::]:8443"} {
		f := &fakeRunner{reply: func(c call) ([]byte, error) { return []byte(current), nil }}
		err := (&backend{runner: f}).listener(context.Background())
		if current == "[::]:8443" {
			if err == nil || len(f.calls) != 1 {
				t.Fatal("overwrote existing listener")
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(f.calls[1].args, []string{"--force-local", "config", "set", "core.https_address", "127.0.0.1:8443"}) {
			t.Fatal(f.calls, err)
		}
	}
	f = &fakeRunner{reply: func(call) ([]byte, error) { return nil, errors.New("denied") }}
	if (&backend{runner: f}).listener(context.Background()) == nil {
		t.Fatal("ignored listener error")
	}
}

func goodConnection() connection {
	return connection{"https://panel.example.com", "agt_" + strings.Repeat("a", 32), "ias_" + strings.Repeat("b", 43)}
}

func TestAtomicPrivateConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent", "config.yaml")
	b := &backend{path: path}
	c := goodConnection()
	{
		err := b.saveConnection(c)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		got := b.loadConnection()
		if got != c {
			t.Fatal("configuration round trip failed")
		}
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatal("configuration not private")
	}
	before, _ := os.ReadFile(path)
	invalid := c
	invalid.Secret = "bad\nsecret"
	if b.saveConnection(invalid) == nil {
		t.Fatal("accepted invalid credentials")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("damaged existing configuration")
	}
	for _, address := range []string{"ftp://panel.example.com", "https://example.com/path", "https://u:p@example.com", "https://example.com?secret=x", "https://example.com#x"} {
		{
			_, err := panelURL(address)
			if err == nil {
				t.Fatal("accepted unsafe URL", address)
			}
		}
	}
}

func TestPanelTokenProtocol(t *testing.T) {
	c := goodConnection()
	payload, _, _ := enrollmentFixture(t, c)
	token := "ait_" + strings.Repeat("x", 43)
	for _, tt := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{"success", payload, 200, true},
		{"expired", "# expired", 403, false},
		{"unauthorized", "", 401, false},
		{"server error", "", 500, false},
		{"JSON instead of assignments", `{"credentials":{}}`, 200, false},
		{"shell injection", "INCUDAL_AGENT_ID='$(touch /tmp/unsafe)'\nINCUDAL_AGENT_SECRET='bad'", 200, false},
		{"extra assignment", fmt.Sprintf("INCUDAL_AGENT_ID='%s'\nINCUDAL_AGENT_SECRET='%s'\nEXEC='evil'", c.ID, c.Secret), 200, false},
		{"oversized", strings.Repeat("x", 131073), 200, false},
		{"duplicate", fmt.Sprintf("INCUDAL_AGENT_ID='%s'\nINCUDAL_AGENT_ID='%s'", c.ID, c.ID), 200, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/agent/install-config/"+token || r.URL.RawQuery != "format=json" {
					t.Error("request contract changed")
				}
				w.WriteHeader(tt.status)
				_, writeErr := fmt.Fprint(w, tt.body)
				if writeErr != nil {
					t.Error(writeErr)
				}
			}))
			defer server.Close()
			b := newBackend()
			b.client.Transport = server.Client().Transport
			got, err := b.exchange(context.Background(), server.URL, token)
			if (err == nil) != tt.want {
				t.Fatal("unexpected result", err)
			}
			if tt.want && (got.ID != c.ID || got.Secret != c.Secret || got.Panel != server.URL) {
				t.Fatal("wrong credential mapping")
			}
			if err != nil && (strings.Contains(err.Error(), token) || strings.Contains(err.Error(), c.Secret)) {
				t.Fatal("credential leaked")
			}
		})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com", http.StatusFound)
	}))
	defer server.Close()
	b := newBackend()
	b.client.Transport = server.Client().Transport
	{
		_, err := b.exchange(context.Background(), server.URL, token)
		if err == nil {
			t.Fatal("followed credential redirect")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		_, err := b.exchange(ctx, server.URL, token)
		if err == nil {
			t.Fatal("ignored cancellation")
		}
	}
}

func TestCertificateTrustContract(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "panel.crt")
	err = os.WriteFile(path, cert, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	data, fp, err := readCertificate(path)
	if err != nil || len(fp) != 64 {
		t.Fatal(err)
	}
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		if len(c.args) != 6 || !reflect.DeepEqual(c.args[:3], []string{"config", "trust", "add-certificate"}) || !reflect.DeepEqual(c.args[4:], []string{"--name", "panel"}) {
			t.Fatal(c.args)
		}
		actual, err := os.ReadFile(c.args[3])
		if err != nil || string(actual) != string(cert) {
			t.Fatal("wrong certificate")
		}
		return nil, nil
	}}
	b := &backend{runner: f}
	{
		err := b.trust(context.Background(), data, fp)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		_, err := os.Stat(f.calls[0].args[3])
		if !os.IsNotExist(err) {
			t.Fatal("temporary certificate not cleaned")
		}
	}
	if b.trust(context.Background(), data, "bad") == nil {
		t.Fatal("accepted wrong fingerprint")
	}
	err = os.WriteFile(path, append(cert, []byte("PRIVATE KEY")...), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	{
		_, _, err := readCertificate(path)
		if err == nil {
			t.Fatal("accepted extra certificate data")
		}
	}
}

func TestConnectorPreflightAndConnection(t *testing.T) {
	c := goodConnection()
	f := &fakeRunner{reply: func(call) ([]byte, error) { return []byte("/usr/local/bin/incudal-agent"), nil }}
	b := &backend{runner: f, path: filepath.Join(t.TempDir(), "config.yaml")}
	if b.connect(context.Background(), c) == nil {
		t.Fatal("accepted legacy service override")
	}
	{
		_, err := os.Stat(b.path)
		if !os.IsNotExist(err) {
			t.Fatal("changed config before checking connector")
		}
	}
	f = &fakeRunner{reply: func(call) ([]byte, error) {
		return []byte("{ path=/usr/libexec/incus/payincus-agent ; argv[]=... }"), nil
	}}
	b.runner = f
	{
		err := b.connect(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(f.calls) != 3 || !reflect.DeepEqual(f.calls[1].args, []string{"enable", "--now", "incudal-agent"}) || !reflect.DeepEqual(f.calls[2].args, []string{"restart", "incudal-agent"}) {
		t.Fatal(f.calls)
	}
}

func TestStatusCapabilityAndFailure(t *testing.T) {
	for _, body := range []string{`{"api_extensions":["payincus_node_v1"]}`, `{"api_extensions":[]}`, `{"api_extensions":"payincus_node_v1"}`, `{}`} {
		f := &fakeRunner{reply: func(c call) ([]byte, error) {
			if c.name == "systemctl" {
				return []byte("ActiveState=active\nSubState=running"), nil
			}
			if reflect.DeepEqual(c.args, []string{"query", "/1.0"}) {
				return []byte(`{"environment":{"certificate_fingerprint":"` + strings.Repeat("a", 64) + `"}}`), nil
			}
			return []byte(body), nil
		}}
		result := (&backend{runner: f}).status(context.Background())
		want := body == `{"api_extensions":["payincus_node_v1"]}`
		if strings.Contains(result, "节点 API: 可连接") != want {
			t.Fatal("incorrect capability state", result)
		}
		if !strings.Contains(result, strings.Repeat("a", 64)) {
			t.Fatal("node identity missing")
		}
		if len(f.calls) != 4 {
			t.Fatal("status contract missing")
		}
	}
	f := &fakeRunner{reply: func(call) ([]byte, error) { return nil, errors.New("denied") }}
	{
		result := (&backend{runner: f}).status(context.Background())
		if !strings.Contains(result, "状态读取失败") || !strings.Contains(result, "不可用") {
			t.Fatal(result)
		}
	}
}

func TestRunnerCancellationAndFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		_, err := (commandRunner{}).Run(ctx, "/bin/true", nil, "")
		if err == nil {
			t.Fatal("ignored cancellation")
		}
	}
	{
		_, err := (commandRunner{}).Run(context.Background(), "/bin/false", nil, "")
		if err == nil {
			t.Fatal("ignored failure")
		}
	}
}
