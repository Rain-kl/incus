package payincus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lxc/incus/v7/internal/server/payincus/egress"
	nodesecurity "github.com/lxc/incus/v7/internal/server/payincus/security"
)

type fakeEngine struct {
	method string
	params json.RawMessage
	err    error
}

func (e *fakeEngine) HandleControl(_ context.Context, method string, params json.RawMessage) (any, error) {
	e.method = method
	e.params = params
	return map[string]any{"appliedRevision": 4}, e.err
}

func (e *fakeEngine) Report() egress.Status {
	return egress.Status{State: "applied", AppliedRevision: 4}
}

func (e *fakeEngine) Run(ctx context.Context) { <-ctx.Done() }

func TestEndpointContracts(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, control string
		code                        int
	}{
		{"GET", "", "", "", 200},
		{"GET", "/network", "", "", 200},
		{"PUT", "/network", `{"schemaVersion":2,"revision":4}`, "network.apply.v2", 200},
		{"PUT", "/network", `{"revision":4}`, "network.apply", 200},
		{"PUT", "/network", `{"schemaVersion":99}`, "", 400},
		{"PUT", "/network", `{} {}`, "", 400},
		{"PUT", "/network", `broken`, "", 400},
		{"POST", "/proxy-probes", `{"nodes":[]}`, "proxy.probe", 200},
		{"POST", "/proxy-probes", `broken`, "", 400},
		{"GET", "/report?interval=60", "", "", 200},
		{"GET", "/report?interval=10", "", "", 200},
		{"GET", "/report?interval=5", "", "", 200},
		{"GET", "/report?interval=4", "", "", 400},
		{"GET", "/report?interval=-1", "", "", 400},
		{"POST", "/caddy", `{"action":"install"}`, "", 200},
		{"POST", "/caddy", `{"action":"uninstall"}`, "", 200},
		{"POST", "/caddy", `{"action":"shell"}`, "", 400},
		{"GET", "/logs?instance=../escape", "", "", 400},
		{"GET", "/journal?unit=ssh&lines=100", "", "", 400},
		{"GET", "/journal?unit=incus&lines=1001", "", "", 400},
		{"DELETE", "/network", "", "", 405},
		{"GET", "/missing", "", "", 404},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			engine := &fakeEngine{}
			service := newService(context.Background(), true, engine)
			service.collect = func(_ string, interval int) map[string]any { return map[string]any{"interval": interval} }
			service.caddyAction = func(_ context.Context, action string) error { return nil }
			recorder := httptest.NewRecorder()
			service.ServeHTTP(recorder, httptest.NewRequest(tc.method, Prefix+tc.path, strings.NewReader(tc.body)))
			if recorder.Code != tc.code {
				t.Fatalf("status %d, body %s", recorder.Code, recorder.Body.String())
			}

			var result envelope
			err := json.Unmarshal(recorder.Body.Bytes(), &result)
			if err != nil {
				t.Fatal(err)
			}

			if tc.code == 200 && result.Type != "sync" {
				t.Fatal(result)
			}

			if tc.code != 200 && (result.Type != "error" || result.ErrorCode != tc.code) {
				t.Fatal(result)
			}

			if engine.method != tc.control {
				t.Fatalf("called %q, expected %q", engine.method, tc.control)
			}
		})
	}
}

func TestControlErrorsAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{{errors.New("invalid_request"), 400}, {errors.New("busy"), 409}, {context.Canceled, 408}, {context.DeadlineExceeded, 504}, {errors.New("secret credentials"), 500}} {
		s := newService(context.Background(), true, &fakeEngine{err: tc.err})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("PUT", Prefix+"/network", strings.NewReader(`{}`)))
		if w.Code != tc.code || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("unexpected error response %d %s", w.Code, w.Body.String())
		}
	}

	s := newService(context.Background(), false, &fakeEngine{})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", Prefix, nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestStreamsAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newService(ctx, true, &fakeEngine{})
	s.diagnostics = func(ctx context.Context, name string, emit func(string)) {
		if name != "vm-1" {
			t.Fatal(name)
		}

		emit("line\nwith newline")
		cancel()
		<-ctx.Done()
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/logs?instance=vm-1", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data: "line\nwith newline"`) {
		t.Fatal(w.Code, w.Body.String())
	}

	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal(w.Header())
	}
}

func TestBusyGates(t *testing.T) {
	s := newService(context.Background(), true, &fakeEngine{})
	s.reportGate <- struct{}{}
	s.caddyGate <- struct{}{}
	for i := 0; i < cap(s.logs); i++ {
		s.logs <- struct{}{}
	}

	for _, tc := range []struct{ method, path, body string }{{"GET", "/report?interval=60", ""}, {"POST", "/caddy", `{"action":"install"}`}, {"GET", "/logs?instance=vm-1", ""}} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(tc.method, Prefix+tc.path, strings.NewReader(tc.body)))
		if w.Code != http.StatusConflict {
			t.Fatal(tc.path, w.Code)
		}
	}
}

func TestJournalStreamContract(t *testing.T) {
	service := newService(context.Background(), true, &fakeEngine{})
	service.journal = func(_ context.Context, unit string, lines int, emit func(string)) {
		if unit != "incudal-agent" || lines != 100 {
			t.Fatal(unit, lines)
		}

		emit("journal line")
	}

	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, httptest.NewRequest("GET", Prefix+"/journal?unit=incudal-agent&lines=100", nil))
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "journal line") {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
}

// Public failures deliberately share the same contract regardless of the internal cause.
func TestPublicNodeErrorsRemainGeneric(t *testing.T) {
	service := newService(context.Background(), true, &fakeEngine{})
	service.guard = nodesecurity.New(filepath.Join(t.TempDir(), "guard"))
	for _, tc := range []struct{ path, body string }{
		{"/policy", `{"enabled":true,"revision":0}`},
		{"/recover", `{"revision":0}`},
		{"/resume", `{"revision":0,"instance":{}}`},
		{"/unlock", `{"dataset":"tank/private","key":"invalid"}`},
		{"/ack", `{"ids":["../state"]}`},
		{"/missing", `{}`},
	} {
		w := httptest.NewRecorder()
		service.ServeHTTP(w, httptest.NewRequest("POST", Prefix+"/security"+tc.path, strings.NewReader(tc.body)))
		var result envelope
		err := json.Unmarshal(w.Body.Bytes(), &result)
		if err != nil || w.Code != 500 || result.Error != "Internal server error" || result.ErrorCode != 500 {
			t.Fatalf("unexpected public response: %d %s", w.Code, w.Body.String())
		}
	}
}
