package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInventoryAPIExtensionsAndEndpoints(t *testing.T) {
	for _, supported := range []bool{true, false} {
		calls := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.URL.RequestURI())
			metadata := any([]any{})
			if r.URL.Path == "/1.0" {
				extensions := []string{}
				if supported {
					extensions = []string{"network_acl", "network_bridge_acl_devices"}
				}

				metadata = map[string]any{"api_extensions": extensions}
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": metadata})
		}))
		c := &incusClient{server.Client()}
		// These production requests use http://incus; only the test transport rewrites the destination.
		c.client.Transport = apiTransport{server.URL, http.DefaultTransport}
		_, err := c.inventory(context.Background())
		server.Close()
		if supported && (err != nil || len(calls) != 3 || calls[1] != "/1.0/networks?recursion=1" || calls[2] != "/1.0/instances?recursion=1") {
			t.Fatal(calls, err)
		}

		if !supported && (err == nil || len(calls) != 1) {
			t.Fatal("missing capability did not stop access", calls, err)
		}
	}
}

type apiTransport struct {
	target string
	base   http.RoundTripper
}

func (t apiTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.URL.Scheme = "http"
	cloned.URL.Host = t.target[len("http://"):]
	return t.base.RoundTrip(cloned)
}

func TestACLAndNICAPIRequests(t *testing.T) {
	for _, exists := range []bool{false, true} {
		calls := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.Path)
			if r.Method == "GET" {
				if !exists {
					w.WriteHeader(404)
					return
				}

				_, _ = fmt.Fprint(w, `{"type":"sync","metadata":{"config":{"user.incudal.managed":"1"}}}`)
				return
			}

			var body map[string]any
			err := json.NewDecoder(r.Body).Decode(&body)
			if err != nil {
				t.Error(err)
			}

			if r.Method == "PATCH" && body["config"] == nil {
				t.Error("missing config patch")
			}

			if r.URL.Path == "/1.0/instances/c1" && body["devices"] == nil {
				t.Error("missing NIC patch")
			}

			_, _ = fmt.Fprint(w, `{"type":"sync","metadata":{}}`)
		}))
		c := &incusClient{&http.Client{Transport: apiTransport{server.URL, http.DefaultTransport}}}
		inv := inventory{Networks: []incusNetwork{{Name: "incusbr0", Type: "bridge", Managed: true, Config: map[string]string{}}}, Instances: []incusInstance{{Name: "c1", Config: map[string]string{}, ExpandedDevices: map[string]map[string]string{"eth0": {"type": "nic", "nictype": "bridged", "parent": "incusbr0"}}}}}
		err := c.applyACL(context.Background(), inv, Firewall{Enabled: true, IngressAction: "allow", EgressAction: "allow"}, false)
		server.Close()
		expected := "POST /1.0/network-acls"
		if exists {
			expected = "PUT /1.0/network-acls/" + aclName
		}

		if err != nil || len(calls) != 4 || calls[1] != expected || calls[2] != "PATCH /1.0/networks/incusbr0" || calls[3] != "PATCH /1.0/instances/c1" {
			t.Fatal(calls, err)
		}
	}
}

func TestOperationLifecycleAndErrorContracts(t *testing.T) {
	for _, final := range []int{200, 400, 401} {
		polls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PATCH" {
				_, _ = fmt.Fprint(w, `{"type":"async","operation":"/1.0/operations/test"}`)
				return
			}

			if r.URL.Path != "/1.0/operations/test/wait" || r.URL.Query().Get("timeout") != "10" {
				t.Error(r.URL)
			}

			polls++
			status := 105
			if polls > 1 {
				status = final
			}

			_, _ = fmt.Fprintf(w, `{"type":"sync","metadata":{"status_code":%d}}`, status)
		}))
		c := &incusClient{&http.Client{Transport: apiTransport{server.URL, http.DefaultTransport}}}
		err := c.request(context.Background(), "PATCH", "/1.0/networks/incusbr0", map[string]any{"config": map[string]string{}}, nil)
		server.Close()
		if polls != 2 || (final == 200) != (err == nil) {
			t.Fatal(final, polls, err)
		}
	}

	for _, body := range []string{`garbage`, `{"type":"error","error":"denied"}`, `{"type":"async","operation":"/unexpected"}`, `{"type":"other"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
		c := &incusClient{&http.Client{Transport: apiTransport{server.URL, http.DefaultTransport}}}
		err := c.request(context.Background(), "GET", "/1.0", nil, nil)
		server.Close()
		if err == nil {
			t.Fatal("accepted", body)
		}
	}
}
