//go:build linux

package security

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageLockCapturesBeforeStopAndRequiresExplicitUnlock(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	key := strings.Repeat("ab", 32)
	fp, _ := KeyFingerprint(key)
	if m.transact(true, func(s *State) error {
		s.Targets = []Target{{Dataset: "pool/incus", GUID: "123", Fingerprint: fp}}
		return nil
	}) != nil {
		t.Fatal("fixture")
	}
	available := true
	stops, snapshots := 0, 0
	stopFails := true
	pending := []ResumeInstance{{Project: "default", Name: "running", UUID: "one"}, {Project: "default", Name: "frozen", UUID: "two", Frozen: true}}
	m.SetPoolWorkloads(func(_ context.Context, dataset string) ([]ResumeInstance, error) {
		if dataset != "pool/incus" {
			t.Fatal("wrong scope")
		}
		snapshots++
		return pending, nil
	}, func(_ context.Context, dataset string) error {
		state, err := m.Status()
		if err != nil || !state.StorageLocks[dataset].Locked || len(state.StorageLocks[dataset].Resume) != 2 {
			t.Fatal("workloads stopped before durable checkpoint")
		}
		stops++
		if stopFails {
			return errors.New("fixture stop failed")
		}
		return nil
	})
	m.storage.Run = func(_ context.Context, _ string, args []string, input []byte) ([]byte, error) {
		switch args[0] {
		case "get":
			props := map[string]string{"guid": "123", "encryptionroot": "pool/incus", "keyformat": "hex", "encryption": "aes-256-gcm", "keylocation": "prompt", "mounted": "no", "keystatus": "available"}
			if !available {
				props["keystatus"] = "unavailable"
			}
			return []byte(props[args[4]]), nil
		case "list":
			return []byte("pool/incus\n"), nil
		case "unload-key":
			available = false
			return nil, nil
		case "load-key":
			if string(input) != key+"\n" {
				return nil, errors.New("wrong key")
			}
			available = true
			return nil, nil
		}
		return nil, errors.New("unexpected storage command")
	}
	if m.LockStorage(context.Background(), "pool/incus") == nil || !available {
		t.Fatal("failed stop was followed by key removal")
	}
	state, _ := m.Status()
	if !state.StorageLocks["pool/incus"].Locked || state.StorageLocks["pool/incus"].Complete {
		t.Fatal("failed lock reported complete")
	}
	if m.CheckStorage("pool/incus/containers/c") == nil || m.CheckStorage("pool/other") != nil {
		t.Fatal("manual lock scope incorrect")
	}
	stopFails = false
	if m.LockStorage(context.Background(), "pool/incus") != nil || available || snapshots != 1 || stops != 2 {
		t.Fatal("retry recaptured stopped workloads or did not unload key")
	}
	r := httptest.NewRequest("POST", "/1.0/payincus/storage/unlock", strings.NewReader(`{"dataset":"pool/incus","key":"`+key+`"}`))
	code, _ := m.HandleStorage(r)
	if code != 409 || available {
		t.Fatal("automatic unlock reopened manually locked storage")
	}
	r = httptest.NewRequest("POST", "/1.0/payincus/storage/unlock-pool", strings.NewReader(`{"dataset":"pool/incus","key":"`+key+`"}`))
	code, _ = m.HandleStorage(r)
	if code != 403 {
		t.Fatal("local caller cleared manual lock")
	}
	r = httptest.NewRequest("POST", "/1.0/payincus/storage/unlock-pool", strings.NewReader(`{"dataset":"pool/incus","key":"`+key+`"}`))
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	code, _ = m.HandleStorage(r)
	if code != 200 || !available || m.CheckStorage("pool/incus") != nil {
		t.Fatal("explicit unlock failed", code)
	}
	recovery, err := m.StorageRecovery("pool/incus")
	if err != nil || recovery.Locked || len(recovery.Resume) != 2 {
		t.Fatal("running identities lost on unlock")
	}
	if m.LockStorage(context.Background(), "pool/incus") == nil {
		t.Fatal("relock discarded pending recovery")
	}
	if m.AcknowledgeStorage("pool/incus", recovery.Revision, ResumeInstance{Project: "default", Name: "running", UUID: "changed"}) == nil {
		t.Fatal("changed identity accepted")
	}
	for _, item := range pending {
		recovery, _ = m.StorageRecovery("pool/incus")
		if m.AcknowledgeStorage("pool/incus", recovery.Revision, item) != nil {
			t.Fatal("recovery acknowledgement failed")
		}
	}
	recovery, _ = m.StorageRecovery("pool/incus")
	if len(recovery.Resume) != 0 {
		t.Fatal("completed identities retained")
	}
}
