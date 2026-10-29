package panel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/config"
	"github.com/lxc/incus/v7/internal/server/payincus/protocol"
)

func sealRecoveryJSON(t *testing.T, secret, agent, direction string, value any) RecoveryEnvelope {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := SealRecovery(secret, agent, direction, data)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestRecoveryEnvelopeBindsIdentityDirectionAndAuthenticationTag(t *testing.T) {
	const secret = "fixture-only-secret"
	plain := []byte(`{"token":"fixture-only-secret","timestamp":123,"nonce":"fixture-nonce-1234"}`)
	envelope, err := SealRecovery(secret, "agent-a", "request", plain)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := OpenRecovery(secret, "agent-a", "request", envelope)
	if err != nil || string(decoded) != string(plain) {
		t.Fatalf("envelope round trip failed: %q, %v", decoded, err)
	}
	for _, test := range []struct {
		name      string
		secret    string
		agent     string
		direction string
	}{
		{name: "wrong secret", secret: "other-secret", agent: "agent-a", direction: "request"},
		{name: "wrong agent", secret: secret, agent: "agent-b", direction: "request"},
		{name: "wrong direction", secret: secret, agent: "agent-a", direction: "response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := OpenRecovery(test.secret, test.agent, test.direction, envelope)
			if err == nil {
				t.Fatal("accepted envelope without matching AEAD authority")
			}
		})
	}
	tampered := envelope
	tampered.Ciphertext = tampered.Ciphertext[:len(tampered.Ciphertext)-4] + "AAAA"
	_, err = OpenRecovery(secret, "agent-a", "request", tampered)
	if err == nil {
		t.Fatal("accepted modified GCM ciphertext/tag")
	}
	for _, malformed := range []RecoveryEnvelope{{IV: "***", Ciphertext: envelope.Ciphertext}, {IV: envelope.IV, Ciphertext: "***"}, {IV: envelope.IV, Ciphertext: ""}} {
		_, err = OpenRecovery(secret, "agent-a", "request", malformed)
		if err == nil {
			t.Fatal("accepted malformed envelope", malformed)
		}
	}
}

func TestRecoveryEnvelopeSupportsStartupPayloadAndRejectsOversize(t *testing.T) {
	const secret = "fixture-only-secret"
	payload := []byte(strings.Repeat("x", 96<<10))
	envelope, err := SealRecovery(secret, "agent-a", "response", payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := OpenRecovery(secret, "agent-a", "response", envelope)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("large startup recovery payload failed to round trip: %v", err)
	}
	oversize := envelope
	oversize.Ciphertext = base64.StdEncoding.EncodeToString(make([]byte, 131073))
	_, err = OpenRecovery(secret, "agent-a", "response", oversize)
	if err == nil {
		t.Fatal("accepted a recovery ciphertext above the bounded payload size")
	}
}

func TestStartupKeysUsesAuthenticatedFreshRequestAndNonceBoundGrant(t *testing.T) {
	const secret = "fixture-only-startup-secret"
	var boundNonce string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != recoveryPath || r.Header.Get("x-incudal-agent-id") != "agent-fixture" {
			t.Errorf("unexpected recovery route or identity: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		hash := protocol.BodySHA256(body)
		payload := protocol.SigningPayload(r.Method, r.URL.Path, r.Header.Get("x-incudal-timestamp"), r.Header.Get("x-incudal-nonce"), hash)
		if r.Header.Get("x-incudal-body-sha256") != hash || r.Header.Get("x-incudal-signature") != protocol.Signature(secret, payload) {
			t.Error("startup request missing outer authenticated identity")
		}
		var envelope RecoveryEnvelope
		err = json.Unmarshal(body, &envelope)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := OpenRecovery(secret, "agent-fixture", "request", envelope)
		if err != nil {
			t.Fatal("request was not encrypted/authenticated", err)
		}
		var proof struct {
			Token     string `json:"token"`
			Timestamp int64  `json:"timestamp"`
			Nonce     string `json:"nonce"`
		}
		err = json.Unmarshal(plain, &proof)
		if err != nil {
			t.Fatal(err)
		}
		if proof.Token != secret || proof.Nonce == "" || time.Since(time.UnixMilli(proof.Timestamp)) > time.Minute {
			t.Error("inner startup proof has missing or stale credentials")
		}
		boundNonce = proof.Nonce
		response := sealRecoveryJSON(t, secret, "agent-fixture", "response", map[string]any{
			"nonce": proof.Nonce, "locked": false, "generation": 4,
			"keys": []map[string]string{{"dataset": "tank/private", "key": strings.Repeat("ab", 32)}},
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	client := New(config.Config{PanelURL: server.URL, AgentID: "agent-fixture", AgentSecret: secret, RequestTimeout: time.Second})
	grant, err := client.StartupKeys(context.Background())
	if err != nil || grant.Locked || grant.Generation != 4 || grant.Nonce != boundNonce || len(grant.Keys) != 1 || grant.Keys[0].Dataset != "tank/private" {
		t.Fatalf("invalid startup grant: %#v, %v", grant, err)
	}
	if grant.Keys[0].Key != strings.Repeat("ab", 32) {
		t.Fatal("startup key was corrupted in transit")
	}
}

func TestStartupKeysRejectsGrantForDifferentNonce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request RecoveryEnvelope
		_ = json.Unmarshal(body, &request)
		plain, err := OpenRecovery("fixture-secret", "agent-fixture", "request", request)
		if err != nil {
			t.Fatal(err)
		}
		var proof struct {
			Nonce string `json:"nonce"`
		}
		_ = json.Unmarshal(plain, &proof)
		response := sealRecoveryJSON(t, "fixture-secret", "agent-fixture", "response", map[string]any{"nonce": proof.Nonce + "-replay", "locked": false, "keys": []any{}})
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	client := New(config.Config{PanelURL: server.URL, AgentID: "agent-fixture", AgentSecret: "fixture-secret", RequestTimeout: time.Second})
	_, err := client.StartupKeys(context.Background())
	if err == nil {
		t.Fatal("accepted response not bound to its request nonce")
	}
}
