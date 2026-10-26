//go:build linux

package security

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func decode(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 32769))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(target)
	if err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one bounded JSON object")
	}
	return nil
}

// Handle is called only after upstream server can_edit authorization.
func (m *Manager) Handle(r *http.Request) (int, any) {
	action := strings.TrimPrefix(r.URL.Path, "/1.0/payincus/security")
	if action == "/report" && r.Method == http.MethodGet && r.TLS == nil {
		return 200, m.Report()
	}
	if action == "" && r.Method == http.MethodGet {
		s, err := m.Status()
		if err != nil {
			return 503, "security state unavailable"
		}
		return 200, s
	}
	if r.Method != http.MethodPost {
		return 405, "method not allowed"
	}
	// Recovery/policy authority uses the existing remote mTLS control-plane identity, never a spoofable header.
	remote := r.TLS != nil && len(r.TLS.PeerCertificates) > 0
	switch action {
	case "/policy":
		if !remote {
			return 403, "policy changes require an authenticated remote control connection"
		}
		var input struct {
			Enabled  *bool   `json:"enabled"`
			Revision *uint64 `json:"revision"`
		}
		if decode(r, &input) != nil || input.Enabled == nil || input.Revision == nil {
			return 400, "invalid policy"
		}
		err := m.Configure(*input.Enabled, *input.Revision)
		if err != nil {
			return 409, err.Error()
		}
	case "/recover":
		if !remote {
			return 403, "recovery requires an authenticated remote control connection"
		}
		var input struct {
			Revision *uint64 `json:"revision"`
		}
		if decode(r, &input) != nil || input.Revision == nil {
			return 400, "invalid recovery request"
		}
		if m.Recover(*input.Revision) != nil {
			return 409, "recovery refused: refresh state and verify registered storage keys"
		}
	case "/lock":
		if !remote {
			return 403, "manual lock requires the control panel"
		}
		if m.Trigger("panel_request") != nil {
			return 503, "cannot persist security lock"
		}
	case "/ack":
		if r.TLS != nil {
			return 403, "audit delivery acknowledgement is node-local"
		}
		var input struct {
			IDs []string `json:"ids"`
		}
		if decode(r, &input) != nil || m.Ack(input.IDs) != nil {
			return 400, "invalid acknowledgement"
		}
	case "/register", "/create", "/unlock":
		if !m.actions.TryLock() {
			return 409, "storage operation is in progress"
		}
		defer m.actions.Unlock()
		var input struct {
			Dataset     string `json:"dataset"`
			Key         string `json:"key"`
			Fingerprint string `json:"fingerprint"`
			Stored      bool   `json:"stored"`
		}
		if decode(r, &input) != nil {
			return 400, "invalid storage request"
		}
		key := []byte(input.Key)
		input.Key = ""
		defer clear(key)
		fingerprint, err := KeyFingerprint(strings.TrimSpace(string(key)))
		if err != nil {
			return 400, "invalid ZFS hex key"
		}
		target := Target{Dataset: input.Dataset, Fingerprint: fingerprint}
		if action == "/unlock" {
			state, err := m.Status()
			if err != nil {
				return 503, "security state unavailable"
			}
			found := false
			for _, registered := range state.Targets {
				if registered.Dataset == target.Dataset && registered.Fingerprint == target.Fingerprint {
					target = registered
					found = true
				}
			}
			if !found {
				return 409, "dataset/key identity not registered"
			}
			if m.storage.Unlock(r.Context(), target, key) != nil {
				return 409, "key loading failed; node remains locked"
			}
		} else {
			if !input.Stored || input.Fingerprint != fingerprint {
				return 409, "confirm control-panel key storage and matching fingerprint first"
			}
			if m.Check() != nil {
				return 423, "recover node before changing storage registration"
			}
			if action == "/create" {
				if m.storage.Create(r.Context(), target.Dataset, key) != nil {
					return 409, "dataset creation failed"
				}
			}
			// Prove the submitted key works, even when the filesystem was already unlocked.
			if m.storage.Unlock(r.Context(), target, key) != nil {
				return 409, "key validation failed"
			}
			if m.Register(r.Context(), target) != nil {
				return 409, "storage registration failed; retain key and inspect dataset"
			}
		}
	default:
		return 404, "unknown security endpoint"
	}
	state, err := m.Status()
	if err != nil {
		return 503, "security state unavailable"
	}
	return 200, state
}
