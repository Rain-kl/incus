//go:build linux

package security

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgetDestroyedStorageRetainsUnavailableOrChangedRoots(t *testing.T) {
	for _, mode := range []string{"gone", "exists", "exported", "empty", "changed-guid", "locked"} {
		t.Run(mode, func(t *testing.T) {
			m := New(filepath.Join(t.TempDir(), "state"))
			err := m.transact(true, func(s *State) error {
				s.Targets = []Target{{Dataset: "pool/incus", GUID: "123", Fingerprint: strings.Repeat("a", 64)}, {Dataset: "pool/other", GUID: "456"}}
				s.Locked = mode == "locked"
				s.StorageLocks = map[string]StorageLock{"pool/incus": {Locked: true}, "pool/other": {Locked: true}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			m.storage.Run = func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
				if name != "zfs" || strings.Join(args, " ") != "list -H -t filesystem,volume -o name -r pool" {
					t.Fatal("incorrect deletion verification query")
				}
				if mode == "exported" {
					return nil, errors.New("pool unavailable")
				}
				if mode == "empty" {
					return nil, nil
				}
				if mode == "exists" {
					return []byte("pool\npool/incus\npool/other\n"), nil
				}
				return []byte("pool\npool/other\n"), nil
			}
			guid := "123"
			if mode == "changed-guid" {
				guid = "different"
			}
			req := httptest.NewRequest("POST", "/1.0/payincus/storage/forget", strings.NewReader(`{"dataset":"pool/incus","guid":"`+guid+`"}`))
			code, _ := m.HandleStorage(req)
			state, err := m.Status()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "gone" || mode == "locked" {
				if code != 200 || len(state.Targets) != 1 || state.Targets[0].Dataset != "pool/other" {
					t.Fatal("removed wrong registration", code, state)
				}
				if _, exists := state.StorageLocks["pool/incus"]; exists || !state.StorageLocks["pool/other"].Locked {
					t.Fatal("deleted storage lock cleanup affected wrong scope")
				}
				if state.Locked != (mode == "locked") {
					t.Fatal("cleanup changed node lock")
				}
				if m.ForgetDestroyed(context.Background(), "pool/incus", guid) != nil {
					t.Fatal("cleanup retry is not idempotent")
				}
			} else if code != 409 || len(state.Targets) != 2 {
				t.Fatal("unsafe cleanup accepted", code, state)
			}
		})
	}
	if !AllowedWhileLocked("POST", "/1.0/payincus/storage/forget") {
		t.Fatal("destroyed storage cleanup cannot recover stale locked registrations")
	}
}

func TestNativeRootDeletionProof(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	if m.transact(true, func(s *State) error { s.Targets = []Target{{Dataset: "external", GUID: "123"}}; return nil }) != nil {
		t.Fatal("fixture failed")
	}
	deleted := false
	m.storage.Run = func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
		if !deleted && name == "zfs" && args[0] == "get" {
			return []byte("123"), nil
		}
		return nil, errors.New("pool unavailable")
	}
	if m.ForgetDestroyed(context.Background(), "external", "123") == nil {
		t.Fatal("unavailable pool was treated as destroyed")
	}
	record, err := m.TrackDeletion(context.Background(), "external")
	if err != nil {
		t.Fatal(err)
	}
	if m.ForgetDestroyed(context.Background(), "external", "123") == nil {
		t.Fatal("prepare alone proved destruction")
	}
	deleted = true
	if record() != nil {
		t.Fatal("cannot record native success")
	}
	if m.ForgetDestroyed(context.Background(), "external", "wrong") == nil {
		t.Fatal("another identity accepted")
	}
	for range 2 {
		if m.ForgetDestroyed(context.Background(), "external", "123") != nil {
			t.Fatal("native root deletion cleanup was not idempotent")
		}
	}
	state, _ := m.Status()
	if len(state.Targets) != 0 {
		t.Fatal("destroyed registration retained")
	}
}

func TestManagedPoolReclaimProtectsExternalAndSiblingData(t *testing.T) {
	for _, mode := range []string{"own", "external", "sibling", "wrong-file"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "owned.img")
			if os.WriteFile(file, []byte("fixture"), 0o600) != nil {
				t.Fatal("fixture failed")
			}
			fp := strings.Repeat("a", 64)
			destroys := 0
			s := &Storage{ImportDir: dir, Run: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
				switch {
				case name == "zfs" && args[0] == "get":
					if mode == "external" {
						return []byte("-"), nil
					}
					return []byte(fp), nil
				case name == "zfs" && args[0] == "list":
					if mode == "sibling" {
						return []byte("owned\nowned/other\n"), nil
					}
					return []byte("owned\n"), nil
				case name == "zpool" && args[0] == "status":
					if mode == "wrong-file" {
						return []byte("/elsewhere/owned.img ONLINE"), nil
					}
					return []byte(file + " ONLINE"), nil
				case name == "zpool" && args[0] == "destroy":
					destroys++
					return nil, nil
				}
				return nil, errors.New("unexpected operation")
			}}
			if s.reclaimManagedPool(context.Background(), "owned/incus", fp) != nil {
				t.Fatal("reclaim failed")
			}
			_, err := os.Stat(file)
			if mode == "own" {
				if destroys != 1 || !os.IsNotExist(err) {
					t.Fatal("owned file/pool retained")
				}
			} else if err != nil {
				t.Fatal("unrelated file removed")
			}
			if (mode == "external" || mode == "sibling") && destroys != 0 {
				t.Fatal("unowned/nonempty pool destroyed")
			}
		})
	}
}
