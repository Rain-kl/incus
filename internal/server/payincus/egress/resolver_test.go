package egress

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNativeBridgeDNSRestoredIndependentlyOfACL(t *testing.T) {
	patches := make(chan map[string]string, 2)
	nextPatch := func() map[string]string {
		t.Helper()
		select {
		case patch := <-patches:
			return patch
		case <-time.After(time.Second):
			t.Fatal("expected Incus DNS patch was not sent")
			return nil
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/1.0/network-acls/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if r.Method != "PATCH" || r.URL.Path != "/1.0/networks/incusbr0" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var patched struct {
			Config map[string]string `json:"config"`
		}
		{
			err := json.NewDecoder(r.Body).Decode(&patched)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		patches <- patched.Config
		_, _ = w.Write([]byte(`{"type":"sync","metadata":{}}`))
	}))
	defer server.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	client := &incusClient{client: &http.Client{Transport: transport}}
	const original = "domain-needed\n"
	inv := inventory{Networks: []incusNetwork{{Name: "incusbr0", Managed: true, Type: "bridge", Config: map[string]string{"raw.dnsmasq": original + "\n# incudal-xray-dns\nserver=/#/169.254.242.2#53\n"}}}}
	{
		err := client.applyACL(context.Background(), inv, Firewall{}, false)
		if err != nil {
			t.Fatal(err)
		}
	}

	patched := nextPatch()
	if patched["raw.dnsmasq"] != original {
		t.Fatal("native DNS not restored, or unrelated dnsmasq settings lost")
	}

	inv.Networks[0].Config["raw.dnsmasq"] = patched["raw.dnsmasq"]
	{
		err := client.applyACL(context.Background(), inv, Firewall{}, true)
		if err != nil {
			t.Fatal(err)
		}
	}

	patched = nextPatch()
	if !strings.Contains(patched["raw.dnsmasq"], "server=/#/169.254.242.2#53") || !strings.HasPrefix(patched["raw.dnsmasq"], original) {
		t.Fatal("proxy DNS not delivered with the firewall disabled")
	}
}

func TestBootstrapUsesRealHostUpstreams(t *testing.T) {
	files := map[string]string{
		"/etc/resolv.conf":                 "nameserver 127.0.0.53\nnameserver ::1\nnameserver 10.10.0.1\nnameserver 169.254.242.2\n",
		"/run/systemd/resolve/resolv.conf": "nameserver 8.8.8.8\nnameserver 114.114.114.114 # secondary\nnameserver 114.114.114.114\nnameserver 218.197.176.8\nnameserver 211.137.58.20\n",
	}

	read := func(path string) ([]byte, error) {
		{
			value, ok := files[path]
			if ok {
				return []byte(value), nil
			}
		}

		return nil, os.ErrNotExist
	}

	inv := inventory{Networks: []incusNetwork{{Managed: true, Type: "bridge", Config: map[string]string{"ipv4.address": "10.10.0.1/22"}}}}
	servers, err := readBootstrapResolvers(read, inv)
	if err != nil || !reflect.DeepEqual(servers, []string{"8.8.8.8", "114.114.114.114", "218.197.176.8"}) {
		t.Fatalf("host upstream fallback failed: %v %v", servers, err)
	}

	files["/etc/resolv.conf"] = "nameserver 192.168.1.1\nnameserver 2001:db8::53\n"
	servers, err = readBootstrapResolvers(read, inv)
	if err != nil || !reflect.DeepEqual(servers, []string{"192.168.1.1", "2001:db8::53"}) {
		t.Fatalf("explicit host upstreams not retained: %v %v", servers, err)
	}

	files["/etc/resolv.conf"] = "nameserver 169.254.169.253\n"
	servers, err = readBootstrapResolvers(read, inv)
	if err != nil || len(servers) != 1 || servers[0] != "169.254.169.253" {
		t.Fatalf("provider link-local IPv4 DNS rejected: %v %v", servers, err)
	}

	delete(files, "/run/systemd/resolve/resolv.conf")
	files["/etc/resolv.conf"] = "nameserver 127.0.0.1\nnameserver 0.0.0.0\nnameserver fe80::1%eth0\nnameserver 224.0.0.1\nnameserver invalid\n"
	{
		_, err := readBootstrapResolvers(read, inv)
		if err == nil {
			t.Fatal("unreachable stubs silently replaced with a public resolver")
		}
	}

	files["/run/NetworkManager/no-stub-resolv.conf"] = "nameserver 192.168.2.1\n"
	servers, err = readBootstrapResolvers(read, inv)
	if err != nil || len(servers) != 1 || servers[0] != "192.168.2.1" {
		t.Fatalf("NetworkManager upstreams unavailable: %v %v", servers, err)
	}

	body := string(namespaceResolverConfig(servers))
	if !strings.Contains(body, "timeout:1 attempts:2 rotate") || strings.Contains(body, "1.1.1.1") {
		t.Fatal("bootstrap must bound retries without forcing a public resolver")
	}
}
