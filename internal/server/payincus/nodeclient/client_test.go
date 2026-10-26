package nodeclient

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnixSocketAndCapabilityCheck(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prefix {
			t.Error(r.URL.Path)
		}

		_, _ = w.Write([]byte(`{"type":"sync","metadata":{"api_extensions":["payincus_node_v1"]}}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	err = New(socket).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequestsAndResponseContracts(t *testing.T) {
	for _, tc := range []struct{ method, path, wireMethod, wirePath string }{
		{"network.apply", "", "PUT", "/network"},
		{"network.apply.v2", "", "PUT", "/network"},
		{"proxy.probe", "", "POST", "/proxy-probes"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.wireMethod || r.URL.Path != prefix+tc.wirePath {
					t.Error(r.Method, r.URL.Path)
				}

				var body map[string]any
				err := json.NewDecoder(r.Body).Decode(&body)
				if err != nil || body["revision"] != float64(4) {
					t.Error(body, err)
				}

				_, _ = w.Write([]byte(`{"type":"sync","metadata":{"appliedRevision":4}}`))
			}))
			defer server.Close()
			c := testClient(server)
			var value map[string]any
			err := c.Call(context.Background(), tc.wireMethod, tc.wirePath, json.RawMessage(`{"revision":4}`), &value)
			if err != nil || value["appliedRevision"] != float64(4) {
				t.Fatal(value, err)
			}
		})
	}

	for _, body := range []string{`{"type":"sync","metadata":{"api_extensions":[]}}`, `{"type":"async"}`, `garbage`, `{"type":"error","error":"denied"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		err := testClient(server).Check(context.Background())
		server.Close()
		if err == nil {
			t.Fatalf("accepted invalid contract %s", body)
		}
	}
}

// Redirect test requests without changing the production Unix-socket transport.
type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = "http"
	cloned.URL.Host = strings.TrimPrefix(r.target, "http://")
	return r.base.RoundTrip(cloned)
}

func testClient(s *httptest.Server) *Client {
	return &Client{HTTP: &http.Client{Transport: rewriteTransport{s.URL, http.DefaultTransport}}}
}

func TestDeniedAndCancelledRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"type":"error","error":"Forbidden"}`))
	}))
	defer server.Close()
	c := testClient(server)
	err := c.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = c.Check(ctx)
	if err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

func TestSSEAndCancellation(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if r.URL.Query().Get("instance") != "vm-1" {
			t.Error(r.URL)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: \"hello\"\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := testClient(server).Stream(ctx, "vm-1", func(line string) {
		if line != "hello" {
			t.Error(line)
		}

		cancel()
	})
	if err == nil {
		t.Fatal("expected cancelled stream")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream leaked")
	}
}

func TestJournalEndpointContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prefix+"/journal" || r.URL.Query().Get("unit") != "incudal-agent" || r.URL.Query().Get("lines") != "100" {
			t.Error(r.URL)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: \"journal\"\n\n"))
	}))
	defer server.Close()
	count := 0
	err := testClient(server).Journal(context.Background(), "incudal-agent", 100, func(line string) {
		if line != "journal" {
			t.Error(line)
		}

		count++
	})
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
