package report

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type apiTransport struct{ target string }

func (t apiTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.URL.Scheme = "http"
	cloned.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestInstanceListAndStateAPIContracts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error(r.Method)
		}

		switch r.URL.RequestURI() {
		case "/1.0/instances?recursion=1":
			_, _ = fmt.Fprint(w, `{"type":"sync","metadata":[{"name":"c1","status":"Running","status_code":103,"type":"container"}]}`)
		case "/1.0/instances/c1/state":
			_, _ = fmt.Fprint(w, `{"type":"sync","metadata":{"status":"Running","network":{"eth0":{"counters":{"bytes_received":9007199254740993,"bytes_sent":42}}}}}`)
		default:
			t.Error(r.URL)
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: apiTransport{server.URL}}
	instances, err := listIncusInstances(client)
	if err != nil || len(instances) != 1 || instances[0].Name != "c1" {
		t.Fatal(instances, err)
	}

	state, err := getIncusInstanceState(client, "c1")
	if err != nil || state.Network["eth0"].Counters.BytesReceived.String() != "9007199254740993" {
		t.Fatal(state, err)
	}
}

func TestInstanceAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{{403, `{"type":"error","error":"denied"}`}, {500, `error`}, {200, `{"type":"async"}`}, {200, `{"type":"sync","metadata":{}}`}, {200, `broken`}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); _, _ = fmt.Fprint(w, tc.body) }))
		_, err := listIncusInstances(&http.Client{Transport: apiTransport{server.URL}})
		server.Close()
		if err == nil {
			t.Fatal("accepted invalid contract", tc)
		}
	}
}
