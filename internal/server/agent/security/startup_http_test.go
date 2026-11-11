//go:build linux

package security

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupHTTPContract(t *testing.T) {
	m, key, _ := lifecycleManager(t)
	simulateNormalRestart(t, m)
	item := ResumeInstance{Project: "default", Name: "http-resume", UUID: "http-resume-id"}
	var restored bool
	m.SetStartupRecovery(func(context.Context) ([]ResumeInstance, error) { return []ResumeInstance{item}, nil }, func(_ context.Context, got ResumeInstance) error {
		if got != item {
			return errors.New("wrong startup identity")
		}
		restored = true
		return nil
	})
	code, value := m.Handle(request("GET", "/startup", "", false))
	if code != 200 {
		t.Fatalf("startup state GET failed: %d %#v", code, value)
	}
	var state State
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	err = json.Unmarshal(encoded, &state)
	if err != nil || !state.RecoveryPending || state.Locked {
		t.Fatalf("GET did not prepare an unlocked recovery: %#v %v", state, err)
	}
	code, _ = m.Handle(request("GET", "/startup", "", true))
	if code == 200 {
		t.Fatal("remote TLS caller reached the local startup endpoint")
	}
	code, _ = m.Handle(request("POST", "/startup", `{"keys":[],"locked":false,"extra":true}`, false))
	if code != 400 {
		t.Fatalf("unknown startup field was accepted: %d", code)
	}
	tooMany := make([]StartupKey, 257)
	body, err := json.Marshal(map[string]any{"keys": tooMany, "locked": false})
	if err != nil {
		t.Fatal(err)
	}
	code, _ = m.Handle(request("POST", "/startup", string(body), false))
	if code != 400 {
		t.Fatalf("more than 256 startup keys were accepted: %d", code)
	}
	code, _ = m.Handle(request("PUT", "/startup", `{}`, false))
	if code != 405 {
		t.Fatalf("unsupported startup method was accepted: %d", code)
	}
	code, _ = m.Handle(request("POST", "/startup", `{"keys":[{"dataset":"pool/incus","key":"`+key+`"}],"locked":false}`, true))
	if code == 200 {
		t.Fatal("remote TLS caller supplied startup key material")
	}
	code, value = m.Handle(request("POST", "/startup", `{"keys":[{"dataset":"pool/incus","key":"`+key+`"}],"locked":false}`, false))
	if code != 200 || !restored {
		t.Fatalf("local startup grant did not load the key and restore state: %d %#v", code, value)
	}
	state, err = m.Status()
	if err != nil || state.Locked || state.RecoveryPending {
		t.Fatalf("startup HTTP grant did not finish recovery: %#v %v", state, err)
	}
}

func TestStartupHTTPLockedProofAndPreparationFailure(t *testing.T) {
	m, _, _ := lifecycleManager(t)
	err := m.Trigger("fixture")
	if err != nil {
		t.Fatal(err)
	}
	code, _ := m.Handle(request("POST", "/startup", `{"keys":[{"dataset":"pool/incus","key":"`+strings.Repeat("ab", 32)+`"}],"locked":true}`, false))
	if code != 200 {
		t.Fatalf("authenticated local lock decision failed: %d", code)
	}
	state, err := m.Status()
	if err != nil || !state.Locked {
		t.Fatalf("locked proof cleared a confirmed lock: %#v %v", state, err)
	}
	if state.RecoveryPending {
		t.Fatal("locked startup proof incorrectly cleared the confirmed alarm")
	}

	failed, _, _ := lifecycleManager(t)
	simulateNormalRestart(t, failed)
	failed.SetSnapshot(func(context.Context) ([]ResumeInstance, error) { return nil, errors.New("fixture snapshot failure") })
	code, _ = failed.Handle(request("GET", "/startup", "", false))
	if code != 503 {
		t.Fatalf("incomplete startup preparation was not retryable: %d", code)
	}

	broken := New(filepath.Join(t.TempDir(), "broken"))
	err = broken.transact(true, func(s *State) error { s.Enabled = true; s.RecoveryPending = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	err = broken.Boot()
	if err != nil {
		t.Fatal("could not initialize missing-adapter startup state", err)
	}
	code, _ = broken.Handle(request("POST", "/startup", `{"keys":[],"locked":false}`, false))
	if code != 409 {
		t.Fatalf("missing startup adapter was accepted: %d", code)
	}
}

func TestStartupHTTPAcceptsRecoveryPayloadAboveLegacyLimit(t *testing.T) {
	m, _, _ := lifecycleManager(t)
	keys := make([]StartupKey, 256)
	for i := range keys {
		keys[i] = StartupKey{Dataset: "pool/" + strings.Repeat("d", 100), Key: strings.Repeat("ab", 32)}
	}
	body, err := json.Marshal(map[string]any{"keys": keys, "locked": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 32768 || len(body) >= 128<<10 {
		t.Fatalf("fixture does not exercise the startup-only body limit: %d bytes", len(body))
	}
	code, _ := m.Handle(request("POST", "/startup", string(body), false))
	if code != 200 {
		t.Fatalf("valid 256-key startup body above the legacy 32 KiB limit was rejected: %d", code)
	}
}
