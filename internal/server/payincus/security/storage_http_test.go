//go:build linux

package security

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentStorageCreationAndLockBoundary(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	key := strings.Repeat("ab", 32)
	created := false
	m.storage.Run = func(_ context.Context, name string, args []string, input []byte) ([]byte, error) {
		if name != "zfs" {
			return nil, errors.New("unexpected command")
		}
		switch args[0] {
		case "create":
			if created || args[len(args)-1] != "pool/incus" || string(input) != key+"\n" {
				t.Fatal("wrong creation arguments, key transport or duplicate creation")
			}
			created = true
			return nil, nil
		case "load-key":
			if string(input) != key+"\n" {
				t.Fatal("wrong key verification input")
			}
			return nil, nil
		case "get":
			properties := map[string]string{"guid": "123", "encryptionroot": "pool/incus", "keyformat": "hex", "keystatus": "available", "encryption": "aes-256-gcm", "keylocation": "prompt", "mountpoint": "none"}
			value, ok := properties[args[4]]
			if !ok {
				return nil, errors.New("unexpected property")
			}
			return []byte(value), nil
		}
		return nil, errors.New("unexpected command")
	}
	invoke := func(action, body string) (int, any) {
		r := httptest.NewRequest("POST", "/1.0/payincus/storage/"+action, strings.NewReader(body))
		return m.HandleStorage(r)
	}
	body := `{"dataset":"pool/incus","key":"` + key + `"}`
	code, value := invoke("create", body)
	if code != 200 || !created {
		t.Fatal("standalone creation required control-panel participation", code, value)
	}
	data, err := json.Marshal(value)
	if err != nil || strings.Contains(string(data), key) {
		t.Fatal("storage response leaked key material")
	}
	state, err := m.Status()
	if err != nil || len(state.Targets) != 1 || state.Targets[0].Dataset != "pool/incus" || state.Targets[0].Fingerprint == "" {
		t.Fatal("storage was not registered for containment", state, err)
	}
	code, _ = invoke("policy", `{"enabled":false,"revision":0}`)
	if code != 404 {
		t.Fatal("storage namespace exposed node policy controls")
	}
	if m.Trigger("fixture") != nil {
		t.Fatal("cannot lock fixture")
	}
	code, _ = invoke("create", body)
	if code != 423 {
		t.Fatal("locked node accepted new storage")
	}
	code, _ = invoke("unlock", body)
	state, err = m.Status()
	if code != 200 || err != nil || !state.Locked {
		t.Fatal("storage unlock cleared node security lock", code, state, err)
	}
	if !AllowedWhileLocked("POST", "/1.0/payincus/storage/unlock") || AllowedWhileLocked("POST", "/1.0/payincus/storage/create") || AllowedWhileLocked("POST", "/1.0/payincus/storage/register") {
		t.Fatal("storage routes widened lockdown authority")
	}
	for _, path := range []string{"/1.0/payincus/storage", "/1.0/payincus/storage/create", "/1.0/payincus/storage/register", "/1.0/payincus/storage/unlock"} {
		if !SensitivePath(path) {
			t.Fatal("storage key request could enter debug logs")
		}
	}
}

func TestStorageDiscoversExternalEncryptedDatasets(t *testing.T) {
	for _, tc := range []struct {
		output string
		fail   bool
		count  int
	}{
		{"pool\toff\t-\t-\t-\npool/private\taes-256-gcm\tpool/private\tunavailable\thex\npool/private/containers\taes-256-gcm\tpool/private\tunavailable\thex\n", false, 3},
		{"", false, 0},
		{"bad\trow\n", true, 0},
		{"pool/private\taes-256-gcm\tpool/private\tunexpected\thex\n", true, 0},
	} {
		m := New(filepath.Join(t.TempDir(), "state"))
		m.storage.Run = func(_ context.Context, name string, args []string, input []byte) ([]byte, error) {
			if name != "zfs" || strings.Join(args, " ") != "list -H -t filesystem,volume -o name,encryption,encryptionroot,keystatus,keyformat" || len(input) != 0 {
				t.Fatal("inventory command changed or received key material")
			}
			return []byte(tc.output), nil
		}
		code, value := m.HandleStorage(httptest.NewRequest("GET", "/1.0/payincus/storage", nil))
		if tc.fail {
			if code != 503 {
				t.Fatal("invalid inventory was reported as successful", code)
			}
			continue
		}
		encoded, err := json.Marshal(value)
		var response struct {
			Datasets []EncryptedDataset `json:"datasets"`
		}
		if code != 200 || err != nil || json.Unmarshal(encoded, &response) != nil || len(response.Datasets) != tc.count {
			t.Fatal("external storage discovery failed", code, value)
		}
		if tc.count > 0 && response.Datasets[1].KeyStatus != "unavailable" {
			t.Fatal("locked encrypted dataset was presented as unlocked")
		}
	}
}

func TestLockedStorageIdentityReadOnly(t *testing.T) {
	for _, path := range []string{"/1.0/storage-pools", "/1.0/storage-pools/disk"} {
		if !AllowedWhileLocked("GET", path) {
			t.Fatal("storage identity hidden while locked", path)
		}
		for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
			if AllowedWhileLocked(method, path) {
				t.Fatal("storage write allowed while locked", method, path)
			}
		}
	}
	for _, path := range []string{"/1.0/storage-pools/disk/volumes", "/1.0/storage-pools/disk/resources", "/1.0/storage-pools/../instances"} {
		if AllowedWhileLocked("GET", path) {
			t.Fatal("non-identity read allowed while locked", path)
		}
	}
}

func TestExternalRootAdoptionProtectsSystemFilesystems(t *testing.T) {
	for _, mountpoint := range []string{"none", "/"} {
		t.Run(mountpoint, func(t *testing.T) {
			key := strings.Repeat("ab", 32)
			fp, _ := KeyFingerprint(key)
			props := map[string]string{"mountpoint": mountpoint, "encryptionroot": "external", "keyformat": "hex", "encryption": "aes-256-gcm", "keystatus": "available", "keylocation": "file:///old.key"}
			writes := 0
			s := &Storage{Run: func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				if args[0] == "get" {
					return []byte(props[args[4]]), nil
				}
				writes++
				if args[0] == "set" {
					props["keylocation"] = "prompt"
				}
				return nil, nil
			}}
			err := s.UnlockManaged(context.Background(), Target{Dataset: "external", Fingerprint: fp}, []byte(key))
			if mountpoint == "/" {
				if err == nil || writes != 0 {
					t.Fatal("system key source mutated")
				}
			} else if err != nil || props["keylocation"] != "prompt" {
				t.Fatal("external root not adopted", err)
			}
		})
	}
}
