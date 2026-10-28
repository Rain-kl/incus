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
	return m.handle(r, false)
}

// StorageExtension separates encrypted storage management from node lockdown policy.
const StorageExtension = "payincus_storage_encryption_v1"

// StoragePoolDirExtension signals support for a custom file-pool directory on pool creation.
const StoragePoolDirExtension = "payincus_storage_pool_dir_v1"

// StoragePoolAllocExtension signals sparse-by-default file pools with an optional preallocate flag.
const StoragePoolAllocExtension = "payincus_storage_pool_alloc_v1"

// StoragePoolResizeExtension signals support for growing managed encrypted file pools.
const StoragePoolResizeExtension = "payincus_storage_pool_resize_v1"

// HandleStorage supports local creation without a control-panel key custody prerequisite.
func (m *Manager) HandleStorage(r *http.Request) (int, any) {
	return m.handle(r, true)
}

func (m *Manager) handle(r *http.Request, storage bool) (int, any) {
	prefix := "/1.0/payincus/security"
	if storage {
		prefix = "/1.0/payincus/storage"
	}
	action := strings.TrimPrefix(r.URL.Path, prefix)
	if storage && action != "" && action != "/create" && action != "/register" && action != "/unlock" && action != "/pool" && action != "/forget" && action != "/lock-pool" && action != "/unlock-pool" && action != "/resume" && action != "/auto-unlock" && action != "/resize" {
		return 404, "unknown storage endpoint"
	}
	if action == "/report" && r.Method == http.MethodGet && r.TLS == nil {
		return 200, m.Report()
	}
	if storage && action == "" && r.Method == http.MethodGet {
		items, err := m.storage.Inventory(r.Context())
		if err != nil {
			return 503, err.Error()
		}
		state, err := m.Status()
		if err != nil {
			return 503, "storage lock state unavailable: " + err.Error()
		}
		return 200, map[string]any{"datasets": items, "locks": state.StorageLocks, "revision": state.Revision, "locked": state.Locked}
	}
	if action == "" && r.Method == http.MethodGet {
		s, err := m.Status()
		if err != nil {
			return 503, "security state unavailable: " + err.Error()
		}
		return 200, s
	}
	if r.Method != http.MethodPost {
		return 405, "method not allowed"
	}
	// Recovery/policy authority uses the existing remote mTLS control-plane identity, never a spoofable header.
	remote := r.TLS != nil && len(r.TLS.PeerCertificates) > 0
	switch action {
	case "/resize":
		if !storage || !remote {
			return 403, "storage resize requires an authenticated remote control connection"
		}
		var input struct {
			Dataset string `json:"dataset"`
			SizeGiB uint64 `json:"sizeGiB"`
		}
		if decode(r, &input) != nil {
			return 400, "invalid storage resize request"
		}
		if err := m.ResizePool(r.Context(), input.Dataset, input.SizeGiB); err != nil {
			return 409, err.Error()
		}
		return 200, map[string]any{"dataset": input.Dataset, "sizeGiB": input.SizeGiB}
	case "/lock-pool", "/unlock-pool":
		if !storage {
			return 404, "unknown security endpoint"
		}
		if !remote {
			return 403, "storage lock changes require an authenticated remote control connection"
		}
		var input struct {
			Dataset string `json:"dataset"`
			Key     string `json:"key"`
		}
		if decode(r, &input) != nil || !ValidEncryptionRoot(input.Dataset) {
			return 400, "invalid storage lock request"
		}
		if action == "/lock-pool" {
			actionErr := m.LockStorage(r.Context(), input.Dataset)
			if actionErr != nil {
				return 409, actionErr.Error()
			}
		} else {
			key := []byte(input.Key)
			input.Key = ""
			defer clear(key)
			actionErr := m.UnlockStorage(r.Context(), input.Dataset, key)
			if actionErr != nil {
				return 409, actionErr.Error()
			}
		}
		value, err := m.StorageRecovery(input.Dataset)
		if err != nil {
			return 503, "storage recovery state unavailable: " + err.Error()
		}
		return 200, value
	case "/forget":
		if !storage {
			return 404, "unknown security endpoint"
		}
		if !m.actions.TryLock() {
			return 409, "storage operation is in progress"
		}
		defer m.actions.Unlock()
		var input struct {
			Dataset string `json:"dataset"`
			GUID    string `json:"guid"`
		}
		if decode(r, &input) != nil {
			return 400, "invalid storage cleanup request"
		}
		actionErr := m.ForgetDestroyed(r.Context(), input.Dataset, input.GUID)
		if actionErr != nil {
			return 409, actionErr.Error()
		}
	case "/pool":
		if !storage {
			return 404, "unknown security endpoint"
		}
		if !m.actions.TryLock() {
			return 409, "storage operation is in progress"
		}
		defer m.actions.Unlock()
		guardErr := m.Check()
		if guardErr != nil {
			if errors.Is(guardErr, ErrLocked) {
				return 423, guardErr.Error()
			}
			return 503, guardErr.Error()
		}
		var input struct {
			Name        string `json:"name"`
			Device      string `json:"device"`
			SizeGiB     uint64 `json:"sizeGiB"`
			Directory   string `json:"directory"`
			Preallocate bool   `json:"preallocate"`
			Key         string `json:"key"`
		}
		if decode(r, &input) != nil {
			return 400, "invalid pool request"
		}
		fingerprint, err := KeyFingerprint(input.Key)
		if err != nil {
			return 400, "invalid ZFS hex key"
		}
		key := []byte(input.Key)
		input.Key = ""
		defer clear(key)
		state, err := m.Status()
		if err != nil {
			return 503, "storage registration state unavailable: " + err.Error()
		}
		target := Target{Dataset: input.Name + "/incus", Fingerprint: fingerprint}
		if state.StorageLocks[target.Dataset].Locked {
			return 423, "storage is manually locked; explicit storage unlock required"
		}
		for _, registered := range state.Targets {
			if registered.Dataset == target.Dataset {
				if registered.Fingerprint != fingerprint {
					return 409, "existing pool identity does not match"
				}
				unlockErr := m.storage.Unlock(r.Context(), registered, key)
				if unlockErr != nil {
					return 409, unlockErr.Error()
				}
				return 200, state
			}
		}
		poolErr := m.storage.CreatePool(r.Context(), input.Name, input.Device, input.SizeGiB, input.Directory, input.Preallocate, fingerprint)
		if poolErr != nil {
			return 409, poolErr.Error() + "; retain key before retrying"
		}
		// A response may have been lost after dataset creation; verify the same key before resuming registration.
		createErr := m.storage.Create(r.Context(), target.Dataset, key)
		unlockErr := m.storage.Unlock(r.Context(), target, key)
		if unlockErr != nil {
			if createErr != nil {
				return 409, "encrypted dataset initialization failed: " + createErr.Error() + "; " + unlockErr.Error() + "; retain key"
			}
			return 409, "encrypted dataset identity verification failed: " + unlockErr.Error() + "; retain key"
		}
		actionErr := m.Register(r.Context(), target)
		if actionErr != nil {
			return 409, actionErr.Error()
		}

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
	case "/resume":
		if storage {
			if !remote {
				return 403, "storage recovery acknowledgement requires the control panel"
			}
			var input struct {
				Dataset  string         `json:"dataset"`
				Revision *uint64        `json:"revision"`
				Instance ResumeInstance `json:"instance"`
			}
			if decode(r, &input) != nil || input.Revision == nil || m.AcknowledgeStorage(input.Dataset, *input.Revision, input.Instance) != nil {
				return 409, "storage recovery state changed"
			}
			value, err := m.StorageRecovery(input.Dataset)
			if err != nil {
				return 503, "storage recovery state unavailable: " + err.Error()
			}
			return 200, value
		}
		if !remote {
			return 403, "recovery requires an authenticated remote control connection"
		}
		var input struct {
			Revision *uint64        `json:"revision"`
			Instance ResumeInstance `json:"instance"`
		}
		if decode(r, &input) != nil || input.Revision == nil || m.AcknowledgeResume(*input.Revision, input.Instance) != nil {
			return 409, "refresh recovery state"
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
		recoverErr := m.Recover(*input.Revision)
		if recoverErr != nil {
			return 409, recoverErr.Error()
		}
	case "/lock":
		if !remote {
			return 403, "manual lock requires the control panel"
		}
		actionErr := m.Trigger("panel_request")
		if actionErr != nil {
			return 503, actionErr.Error()
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
	case "/register", "/create", "/unlock", "/auto-unlock":
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
		if action == "/unlock" || action == "/auto-unlock" {
			state, err := m.Status()
			if err != nil {
				return 503, "security state unavailable: " + err.Error()
			}
			if action == "/auto-unlock" && (!storage || !remote || state.Locked) {
				return 423, "automatic key loading is blocked by node security state"
			}
			if state.StorageLocks[input.Dataset].Locked {
				return 409, "storage is manually locked; explicit storage unlock required"
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
			unlockErr := m.storage.Unlock(r.Context(), target, key)
			if unlockErr != nil {
				return 409, unlockErr.Error()
			}
		} else {
			if !storage && (!input.Stored || input.Fingerprint != fingerprint) {
				return 409, "confirm control-panel key storage and matching fingerprint first"
			}
			if storage && input.Fingerprint != "" && input.Fingerprint != fingerprint {
				return 409, "key fingerprint mismatch"
			}
			guardErr := m.Check()
			if guardErr != nil {
				if errors.Is(guardErr, ErrLocked) {
					return 423, guardErr.Error()
				}
				return 503, guardErr.Error()
			}
			state, err := m.Status()
			if err != nil {
				return 503, "storage state unavailable: " + err.Error()
			}
			if state.StorageLocks[target.Dataset].Locked {
				return 409, "storage is manually locked; explicit storage unlock required"
			}
			if action == "/create" {
				createErr := m.storage.Create(r.Context(), target.Dataset, key)
				if createErr != nil {
					return 409, createErr.Error()
				}
			}
			// Prove the key before adopting external storage; never retain a native automatic keyfile source.
			var validationErr error
			if storage && action == "/register" {
				validationErr = m.storage.UnlockManaged(r.Context(), target, key)
			} else {
				validationErr = m.storage.Unlock(r.Context(), target, key)
			}
			if validationErr != nil {
				return 409, validationErr.Error()
			}
			actionErr := m.Register(r.Context(), target)
			if actionErr != nil {
				return 409, actionErr.Error()
			}
		}
	default:
		return 404, "unknown security endpoint"
	}
	state, err := m.Status()
	if err != nil {
		return 503, "security state unavailable: " + err.Error()
	}
	return 200, state
}
