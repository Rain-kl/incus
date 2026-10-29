package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/config"
	"github.com/lxc/incus/v7/internal/server/payincus/nodeclient"
	"github.com/lxc/incus/v7/internal/server/payincus/panel"
)

func runStartupRecoveryFixture(t *testing.T, state map[string]any, blockLocal bool, wantGrant bool) (map[string]any, int32) {
	t.Helper()
	const secret = "fixture-only-agent-secret"
	const agentID = "fixture-agent"
	localPath := filepath.Join(t.TempDir(), "incus.socket")
	listener, err := net.Listen("unix", localPath)
	if err != nil {
		t.Fatal(err)
	}
	localCalled := make(chan struct{}, 1)
	posted := make(chan map[string]any, 1)
	localServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case localCalled <- struct{}{}:
		default:
		}
		if r.Method == http.MethodGet {
			if blockLocal {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if state == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"type":"error","error":"fixture state unavailable"}`))
				return
			}
			metadata, marshalErr := json.Marshal(state)
			if marshalErr != nil {
				t.Error(marshalErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(append([]byte(`{"type":"sync","metadata":`), append(metadata, '}')...))
			return
		}
		var body map[string]any
		decodeErr := json.NewDecoder(r.Body).Decode(&body)
		if decodeErr != nil {
			t.Error(decodeErr)
		}
		posted <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"sync","metadata":{}}`))
	})}
	go func() { _ = localServer.Serve(listener) }()
	t.Cleanup(func() { _ = localServer.Close(); _ = listener.Close() })
	var remoteCalls atomic.Int32
	panelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls.Add(1)
		var requestEnvelope panel.RecoveryEnvelope
		decodeErr := json.NewDecoder(r.Body).Decode(&requestEnvelope)
		if decodeErr != nil {
			t.Error(decodeErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		plain, openErr := panel.OpenRecovery(secret, agentID, "request", requestEnvelope)
		if openErr != nil {
			t.Error(openErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var proof struct {
			Nonce string `json:"nonce"`
		}
		decodeErr = json.Unmarshal(plain, &proof)
		if decodeErr != nil {
			t.Error(decodeErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		grant, marshalErr := json.Marshal(map[string]any{"nonce": proof.Nonce, "locked": false, "generation": 4, "keys": []map[string]string{{"dataset": "pool/incus", "key": "fixture-zfs-key"}}})
		if marshalErr != nil {
			t.Error(marshalErr)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		response, sealErr := panel.SealRecovery(secret, agentID, "response", grant)
		if sealErr != nil {
			t.Error(sealErr)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(panelServer.Close)
	cfg := config.Config{PanelURL: panelServer.URL, AgentID: agentID, AgentSecret: secret, HeartbeatInterval: time.Second, RequestTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { startupRecovery(ctx, nodeclient.New(localPath), panel.New(cfg)); close(done) }()
	select {
	case <-localCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("startup recovery did not read local state")
	}
	var payload map[string]any
	if wantGrant {
		select {
		case payload = <-posted:
		case <-time.After(2 * time.Second):
			t.Fatal("startup recovery did not apply the control-plane grant locally")
		}
	} else {
		select {
		case payload = <-posted:
			t.Fatalf("unexpected local startup POST: %#v", payload)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startup recovery did not exit after context cancellation")
	}
	return payload, remoteCalls.Load()
}

func TestStartupRecoveryKeepsLockedAndUnavailableNodesOffline(t *testing.T) {
	_, calls := runStartupRecoveryFixture(t, map[string]any{"locked": true, "recoveryPending": true}, false, false)
	if calls != 0 {
		t.Fatalf("locally locked node requested remote keys %d times", calls)
	}
	_, calls = runStartupRecoveryFixture(t, nil, false, false)
	if calls != 0 {
		t.Fatalf("unreadable local state requested remote keys %d times", calls)
	}
	_, calls = runStartupRecoveryFixture(t, nil, true, false)
	if calls != 0 {
		t.Fatalf("cancelled local request reached remote panel %d times", calls)
	}
	_, calls = runStartupRecoveryFixture(t, map[string]any{"locked": false, "recoveryPending": false, "startupRecovering": false, "resumeInstances": []map[string]string{{"project": "default", "name": "manual", "uuid": "manual-id"}}}, false, false)
	if calls != 0 {
		t.Fatalf("manual recovery identities triggered an automatic key request %d times", calls)
	}
}

func TestStartupRecoveryAppliesBoundGrantAndClearsPayload(t *testing.T) {
	payload, calls := runStartupRecoveryFixture(t, map[string]any{"locked": false, "recoveryPending": true}, false, true)
	if calls != 1 {
		t.Fatalf("expected one remote key request, got %d", calls)
	}
	if payload["locked"] != false {
		t.Fatalf("startup lock decision was not passed to local API: %#v", payload)
	}
	if payload["generation"] != float64(4) {
		t.Fatalf("startup lock generation was not passed to local API: %#v", payload)
	}
	keys, ok := payload["keys"].([]any)
	if !ok || len(keys) != 1 || keys[0].(map[string]any)["key"] != "fixture-zfs-key" {
		t.Fatalf("control-plane key grant was not passed to local startup API: %#v", payload)
	}
}
