// Package payincus contains node extensions without changing upstream API contracts.
package payincus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/caddy"
	"github.com/lxc/incus/v7/internal/server/payincus/config"
	"github.com/lxc/incus/v7/internal/server/payincus/diagnostics"
	"github.com/lxc/incus/v7/internal/server/payincus/egress"
	"github.com/lxc/incus/v7/internal/server/payincus/report"
	nodesecurity "github.com/lxc/incus/v7/internal/server/payincus/security"
)

// Constants define protocol identifiers and timing limits.
const (
	Prefix                   = "/1.0/payincus"
	Extension                = "payincus_node_v1"
	XrayMaintenanceExtension = "payincus_xray_maintenance_v1"
)

// Engine is the boundary between HTTP contracts and host-side networking.
type Engine interface {
	HandleControl(context.Context, string, json.RawMessage) (any, error)
	Report() egress.Status
	Run(context.Context)
}

// Service serves authenticated node extension requests inside incusd.
type Service struct {
	ctx           context.Context
	guard         *nodesecurity.Manager
	engine        Engine
	enabled       bool
	caddyGate     chan struct{}
	reportGate    chan struct{}
	logs          chan struct{}
	collect       func(string, int) map[string]any
	collectLocked func() map[string]any
	caddyAction   func(context.Context, string) error
	diagnostics   func(context.Context, string, func(string))
	journal       func(context.Context, string, int, func(string))
}

// New creates an API client using the supplied connection configuration.
func New(ctx context.Context, enabled bool) *Service {
	if !enabled {
		return newService(ctx, false, nil)
	}

	return newService(ctx, enabled, egress.NewManager())
}

func newService(ctx context.Context, enabled bool, engine Engine) *Service {
	return &Service{
		ctx: ctx, enabled: enabled, engine: engine, guard: nodesecurity.Default(), caddyGate: make(chan struct{}, 1), reportGate: make(chan struct{}, 1), logs: make(chan struct{}, 32), collect: report.HeartbeatPayload, collectLocked: report.LockedStoragePayload, diagnostics: diagnostics.Stream, journal: diagnostics.Journal,
		caddyAction: func(ctx context.Context, action string) error {
			if action == "install" {
				return caddy.Install(ctx)
			}

			return caddy.Uninstall(ctx)
		},
	}
}

// Run reconciles persisted network state until shutdown.
func (s *Service) Run() {
	if s.enabled {
		s.engine.Run(s.ctx)
	}
}

type envelope struct {
	Type       string `json:"type"`
	Status     string `json:"status,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	Metadata   any    `json:"metadata,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorCode  int    `json:"error_code,omitempty"`
}

func respond(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	result := envelope{Type: "sync", Status: "Success", StatusCode: 200, Metadata: value}
	if code != http.StatusOK {
		result = envelope{Type: "error", Error: fmt.Sprint(value), ErrorCode: code}
	}

	_ = json.NewEncoder(w).Encode(result)
}

func decode(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, (1<<20)+1))
	err := decoder.Decode(target)
	if err != nil {
		return err
	}

	var extra any
	err = decoder.Decode(&extra)
	if err != io.EOF {
		return errors.New("expected one JSON value")
	}

	return nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.enabled {
		respond(w, 503, "PayIncus extensions disabled; set INCUS_PAYINCUS=1")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path == Prefix+"/storage" || strings.HasPrefix(r.URL.Path, Prefix+"/storage/") {
		code, value := s.guard.HandleStorage(r)
		respond(w, code, value)
		return
	}
	if strings.HasPrefix(r.URL.Path, Prefix+"/security") {
		code, value := s.guard.Handle(r)
		// PayIncus: preserve sanitized management errors; daemon lock enforcement masks blocked requests.
		respond(w, code, value)
		return
	}
	switch r.URL.Path {
	case Prefix:
		if r.Method != http.MethodGet {
			break
		}

		respond(w, 200, map[string]any{"api_extensions": []string{Extension, XrayMaintenanceExtension, nodesecurity.StartupExtension, nodesecurity.Extension, nodesecurity.ResumeExtension, nodesecurity.StorageExtension, nodesecurity.StoragePoolDirExtension, nodesecurity.StoragePoolAllocExtension, nodesecurity.StoragePoolResizeExtension}, "api_version": "1.0", "transport_only_agent": true})
		return
	case Prefix + "/report":
		if r.Method != http.MethodGet {
			break
		}

		select {
		case s.reportGate <- struct{}{}:
			defer func() { <-s.reportGate }()
		default:
			respond(w, 409, "busy")
			return
		}

		interval, err := strconv.Atoi(r.URL.Query().Get("interval"))
		if err != nil || interval < config.MinHeartbeatIntervalSeconds || interval > config.MaxHeartbeatIntervalSeconds {
			respond(w, 400, "interval must be between 5 and 3600")
			return
		}

		payload := map[string]any{}
		guardErr := s.guard.Check()
		if guardErr == nil {
			payload = s.collect("payincus-node-v1", interval)
		} else if errors.Is(guardErr, nodesecurity.ErrLocked) {
			payload = s.collectLocked()
		}
		payload["nodeSecurity"] = s.guard.Report()
		payload["networkStatus"] = s.engine.Report()
		payload["nodeApi"] = Extension
		respond(w, 200, payload)
		return
	case Prefix + "/xray":
		if r.TLS != nil {
			respond(w, 403, "local maintenance only")
			return
		}
		method := "xray.inspect"
		if r.Method == http.MethodPost {
			var input struct {
				Action string `json:"action"`
			}
			if decode(r, &input) != nil || (input.Action != "test" && input.Action != "restart" && input.Action != "reconfigure") {
				respond(w, 400, "invalid_request")
				return
			}
			method = "xray." + input.Action
		} else if r.Method != http.MethodGet {
			break
		}
		s.control(w, r, method, json.RawMessage(`{}`), 5*time.Minute)
		return
	case Prefix + "/network":
		if r.Method == http.MethodGet {
			respond(w, 200, s.engine.Report())
			return
		}

		if r.Method != http.MethodPut {
			break
		}

		var params json.RawMessage
		err := decode(r, &params)
		if err != nil {
			respond(w, 400, "invalid_request")
			return
		}

		var version struct {
			SchemaVersion int `json:"schemaVersion"`
		}

		err = json.Unmarshal(params, &version)
		if err != nil {
			respond(w, 400, "invalid_request")
			return
		}

		method := "network.apply"
		if version.SchemaVersion == 2 {
			method = "network.apply.v2"
		} else if version.SchemaVersion != 0 && version.SchemaVersion != 1 {
			respond(w, 400, "unsupported network schema")
			return
		}

		s.control(w, r, method, params, 95*time.Second)
		return
	case Prefix + "/proxy-probes":
		if r.Method != http.MethodPost {
			break
		}

		var params json.RawMessage
		err := decode(r, &params)
		if err != nil {
			respond(w, 400, "invalid_request")
			return
		}

		s.control(w, r, "proxy.probe", params, 40*time.Second)
		return
	case Prefix + "/caddy":
		if r.Method != http.MethodPost {
			break
		}

		var input struct {
			Action string `json:"action"`
		}

		err := decode(r, &input)
		if err != nil || (input.Action != "install" && input.Action != "uninstall") {
			respond(w, 400, "invalid_request")
			return
		}

		select {
		case s.caddyGate <- struct{}{}:
			defer func() { <-s.caddyGate }()
		default:
			respond(w, 409, "busy")
			return
		}

		operationCtx, done := context.WithTimeout(ctx, 15*time.Minute)
		defer done()
		err = s.caddyAction(operationCtx, input.Action)
		if err != nil {
			respond(w, 500, "Caddy operation failed; inspect incusd journal")
			return
		}

		respond(w, 200, map[string]any{"action": input.Action})
		return
	case Prefix + "/logs", Prefix + "/journal":
		if r.Method != http.MethodGet {
			break
		}

		name := r.URL.Query().Get("instance")
		journal := r.URL.Path == Prefix+"/journal"
		unit := r.URL.Query().Get("unit")
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		if journal && ((unit != "incus" && unit != "incudal-agent") || lines < 1 || lines > 1000) {
			respond(w, 400, "invalid journal request")
			return
		}

		if !journal && !diagnostics.InstanceName.MatchString(name) {
			respond(w, 400, "invalid instance name")
			return
		}

		select {
		case s.logs <- struct{}{}:
			defer func() { <-s.logs }()
		default:
			respond(w, 409, "busy")
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			respond(w, 500, "streaming unavailable")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		flusher.Flush()
		var mu sync.Mutex
		emit := func(line string) {
			mu.Lock()
			defer mu.Unlock()
			data, err := json.Marshal(line)
			if err != nil {
				return
			}

			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
			_, err = fmt.Fprintf(w, "data: %s\n\n", data)
			if err != nil {
				cancel()
				return
			}

			flusher.Flush()
		}

		streamCtx, end := context.WithTimeout(ctx, 15*time.Minute)
		defer end()
		if journal {
			s.journal(streamCtx, unit, lines, emit)
		} else {
			s.diagnostics(streamCtx, name, emit)
		}

		return
	default:
		respond(w, 404, "unknown PayIncus endpoint")
		return
	}

	respond(w, 405, "method not allowed")
}

func (s *Service) control(w http.ResponseWriter, r *http.Request, method string, params json.RawMessage, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	value, err := s.engine.HandleControl(ctx, method, params)
	if err != nil {
		code := 500
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			code = 504
		case errors.Is(err, context.Canceled):
			code = 408
		case err.Error() == "invalid_request":
			code = 400
		case err.Error() == "busy":
			code = 409
		}

		message := "node operation failed; inspect incusd journal"
		if code != 500 {
			message = http.StatusText(code)
		}

		respond(w, code, message)
		return
	}

	respond(w, 200, value)
}
