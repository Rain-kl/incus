//go:build linux

package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnlyConfirmedGlobalLockMasksErrors(t *testing.T) {
	for _, mode := range []string{"disabled", "enabled", "manual", "corrupt", "unreadable", "locked"} {
		t.Run(mode, func(t *testing.T) {
			m := New(filepath.Join(t.TempDir(), "security"))
			if m.Configure(mode == "enabled", 0) != nil {
				t.Fatal("fixture failed")
			}
			switch mode {
			case "manual":
				if m.transact(true, func(s *State) error {
					s.StorageLocks = map[string]StorageLock{"pool/incus": {Locked: true}}
					return nil
				}) != nil {
					t.Fatal("fixture failed")
				}
			case "corrupt":
				if os.WriteFile(filepath.Join(m.dir, "state.json"), []byte("broken"), 0o600) != nil {
					t.Fatal("fixture failed")
				}
			case "unreadable":
				m.dir = filepath.Join(t.TempDir(), "file")
				if os.WriteFile(m.dir, []byte("not a directory"), 0o600) != nil {
					t.Fatal("fixture failed")
				}
			case "locked":
				if m.Trigger("fixture") != nil {
					t.Fatal("fixture failed")
				}
			}
			cause := m.CheckStorage("pool/incus")
			if mode == "disabled" || mode == "enabled" {
				if cause != nil || PublicErrorFor(cause) != nil {
					t.Fatal("unlocked state rejected", cause)
				}
				return
			}
			if cause == nil {
				t.Fatal("guard failed open")
			}
			public := PublicErrorFor(cause)
			if mode == "locked" {
				if public.Error() != PublicError().Error() {
					t.Fatal("global lock mask changed")
				}
			} else if public != cause || strings.Contains(public.Error(), "database disk image") {
				t.Fatal("non-lock cause was misreported", public)
			}
		})
	}
	if PublicErrorFor(fmt.Errorf("wrapped: %w", ErrLocked)).Error() != PublicError().Error() {
		t.Fatal("wrapped lock not recognized")
	}
}

func TestCLIAuditReportsProtocolAndWriteFailures(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INCUS_DIR", root)
	old := singleton
	singleton = New(Directory())
	t.Cleanup(func() { singleton = old })
	for _, tc := range []struct {
		status int
		body   string
	}{{200, ""}, {400, "invalid local CLI audit request"}, {403, "cannot authenticate local CLI audit request"}, {503, "audit queue full"}, {500, PublicError().Error()}} {
		listener, err := net.Listen("unix", filepath.Join(root, "payincus-audit.socket"))
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})}
		go func() { _ = server.Serve(listener) }()
		err = ObserveCLI([]string{"list"})
		_ = server.Close()
		if tc.status == 200 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.body) {
			t.Fatal("audit failure lost real reason", tc.status, err)
		}
		if tc.status != 500 && strings.Contains(err.Error(), "database disk image") {
			t.Fatal("protocol failure posed as corruption")
		}
	}
	// The offline fallback must reject an unreadable state without claiming the lock was persisted.
	if os.MkdirAll(singleton.dir, 0o700) != nil || os.WriteFile(filepath.Join(singleton.dir, "state.json"), []byte("broken"), 0o600) != nil {
		t.Fatal("fixture failed")
	}
	err := ObserveCLI([]string{"exec"})
	if err == nil || !strings.Contains(err.Error(), "security state is corrupt") || strings.Contains(err.Error(), "database disk image") {
		t.Fatal("offline failure was hidden or bypassed", err)
	}
}

func TestUnlockedManagementRetainsSafeZFSFailureReasons(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		m := New(filepath.Join(t.TempDir(), "state"))
		if m.Configure(enabled, 0) != nil {
			t.Fatal("fixture failed")
		}
		key := strings.Repeat("ab", 32)
		m.storage.Run = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
			if args[0] == "create" {
				return nil, errors.New("fixture command failed")
			}
			return nil, errors.New("fixture property unavailable")
		}
		r := request("POST", "/create", `{"dataset":"pool/private","key":"`+key+`","stored":true,"fingerprint":"`+keyFingerprintForTest(t, key)+`"}`, false)
		code, body := m.Handle(r)
		message := fmt.Sprint(body)
		if code != 409 || !strings.Contains(message, "ZFS dataset creation failed") || strings.Contains(message, key) || strings.Contains(message, "database disk image") {
			t.Fatal("create failure not diagnostic", code, message)
		}
	}
}

func keyFingerprintForTest(t *testing.T, key string) string {
	t.Helper()
	value, err := KeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestResizeStateFailureDoesNotPretendToBeLockOrIdentityChange(t *testing.T) {
	for _, mode := range []string{"corrupt", "manual", "property", "marker"} {
		t.Run(mode, func(t *testing.T) {
			m := New(filepath.Join(t.TempDir(), "state"))
			if m.transact(true, func(s *State) error {
				s.Targets = []Target{{Dataset: "tank/incus", GUID: "123"}}
				if mode == "manual" {
					s.StorageLocks = map[string]StorageLock{"tank/incus": {Locked: true}}
				}
				return nil
			}) != nil {
				t.Fatal("fixture failed")
			}
			if mode == "corrupt" && os.WriteFile(filepath.Join(m.dir, "state.json"), []byte("broken"), 0o600) != nil {
				t.Fatal("fixture failed")
			}
			commands := 0
			m.storage.Run = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				commands++
				if mode == "marker" && args[4] == "guid" {
					return []byte("123"), nil
				}
				return nil, errors.New("fixture property permission denied")
			}
			err := m.ResizePool(context.Background(), "tank/incus", 2)
			expected, calls := "security state is corrupt", 0
			if mode == "manual" {
				expected = "storage is manually locked"
			}
			if mode == "property" {
				expected, calls = "ZFS property unavailable: guid", 1
			}
			if mode == "marker" {
				expected, calls = "ZFS property unavailable: payincus:key-fingerprint", 2
			}
			if err == nil || !strings.Contains(err.Error(), expected) || strings.Contains(err.Error(), "database disk image") || strings.Contains(err.Error(), "identity changed") || commands != calls {
				t.Fatal("failure cause was masked or storage was modified", err, commands)
			}
		})
	}
}

// Reading a property is not evidence that its value violates the stored identity.
func TestStoragePropertyFailuresRemainDiagnostic(t *testing.T) {
	key := strings.Repeat("ab", 32)
	fp := keyFingerprintForTest(t, key)
	target := Target{Dataset: "tank/incus", GUID: "123", Fingerprint: fp}
	for _, action := range []string{"ready", "unlock", "adopt", "lock", "verify-unload"} {
		t.Run(action, func(t *testing.T) {
			cause := errors.New("fixture property permission denied")
			failProperty := "guid"
			if action == "adopt" {
				failProperty = "keyformat"
			}
			if action == "verify-unload" {
				failProperty = "keystatus"
			}
			statusReads, writes := 0, 0
			storage := &Storage{Run: func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				if args[0] == "get" {
					prop := args[4]
					if prop == "keystatus" {
						statusReads++
					}
					if prop == failProperty && (action != "verify-unload" || statusReads > 1) {
						return nil, cause
					}
					return []byte(map[string]string{"guid": "123", "keystatus": "available", "encryptionroot": "tank/incus", "mountpoint": "none", "keyformat": "hex", "encryption": "aes-256-gcm", "mounted": "no"}[prop]), nil
				}
				if args[0] == "list" {
					return []byte("tank/incus"), nil
				}
				writes++
				return nil, nil
			}}
			var err error
			switch action {
			case "ready":
				err = storage.Ready(context.Background(), target)
			case "unlock":
				err = storage.Unlock(context.Background(), target, []byte(key))
			case "adopt":
				err = storage.UnlockManaged(context.Background(), target, []byte(key))
			default:
				err = storage.Lock(context.Background(), target)
			}
			expectedWrites := 0
			if action == "verify-unload" {
				expectedWrites = 1
			}
			if !errors.Is(err, cause) || strings.Contains(err.Error(), "mismatch") || strings.Contains(err.Error(), "identity changed") || writes != expectedWrites {
				t.Fatal("property failure misreported or unsafe writes", err, writes)
			}
		})
	}
}

func TestStorageCommandsDoNotInventFailureCauses(t *testing.T) {
	for _, command := range []string{"unmount", "unload-key", "create"} {
		t.Run(command, func(t *testing.T) {
			cause := errors.New("executable file not found in PATH")
			storage := &Storage{Run: func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				if args[0] == command {
					return nil, cause
				}
				if args[0] == "list" {
					return []byte("tank/incus"), nil
				}
				if args[0] == "get" {
					return []byte(map[string]string{"encryptionroot": "tank/incus", "mounted": "yes", "keystatus": "available"}[args[4]]), nil
				}
				return nil, nil
			}}
			var err error
			if command == "create" {
				err = storage.Create(context.Background(), "tank/incus", []byte(strings.Repeat("ab", 32)))
			} else {
				err = storage.Lock(context.Background(), Target{Dataset: "tank/incus"})
			}
			if !errors.Is(err, cause) || strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "remains in use") {
				t.Fatal("command failure was misdiagnosed", err)
			}
		})
	}
}

func TestPoolDeviceInspectionRetainsFilesystemError(t *testing.T) {
	storage := &Storage{Run: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
		if name == "zpool" && args[0] == "list" {
			return nil, nil
		}
		return nil, errors.New("absent")
	}}
	err := storage.CreatePool(context.Background(), "pool", "/dev/payincus-nonexistent-fixture", 0, "", false, strings.Repeat("a", 64))
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "cannot inspect block device") {
		t.Fatal("stat failure misreported", err)
	}
}

func TestPropertyFailuresCannotTriggerPoolWrites(t *testing.T) {
	for _, action := range []string{"create", "unlock"} {
		for _, inventory := range []string{"present", "failed"} {
			t.Run(action+"/"+inventory, func(t *testing.T) {
				cause := errors.New("fixture property permission denied")
				commands := 0
				storage := &Storage{ImportDir: t.TempDir(), Run: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
					commands++
					if args[0] == "get" {
						return nil, cause
					}
					if name == "zpool" && args[0] == "list" {
						if inventory == "present" {
							return []byte("tank"), nil
						}
						return nil, errors.New("fixture inventory I/O failure")
					}
					t.Fatal("property failure reached storage write", name, args)
					return nil, nil
				}}
				var err error
				if action == "create" {
					err = storage.CreatePool(context.Background(), "tank", "", 1, "", false, strings.Repeat("a", 64))
				} else {
					key := strings.Repeat("ab", 32)
					err = storage.Unlock(context.Background(), Target{Dataset: "tank/incus", Fingerprint: keyFingerprintForTest(t, key)}, []byte(key))
				}
				entries, _ := os.ReadDir(storage.ImportDir)
				if !errors.Is(err, cause) || commands != 2 || len(entries) != 0 {
					t.Fatal("property failure hidden or backing file created", err, commands)
				}
			})
		}
	}
}

func TestUnlockImportsOnlyConfirmedAbsentPool(t *testing.T) {
	for _, failImport := range []bool{false, true} {
		t.Run(fmt.Sprint(failImport), func(t *testing.T) {
			key := strings.Repeat("ab", 32)
			imported, loaded, imports := false, false, 0
			propertyErr := errors.New("fixture dataset not imported")
			storage := &Storage{Run: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
				if name == "zpool" {
					if args[0] == "list" {
						return nil, nil
					}
					if args[0] == "import" {
						imports++
						if failImport {
							return nil, errors.New("fixture import permission denied")
						}
						imported = true
						return nil, nil
					}
				}
				if args[0] == "get" {
					if !imported {
						return nil, propertyErr
					}
					if args[4] == "keystatus" {
						if loaded {
							return []byte("available"), nil
						}
						return []byte("unavailable"), nil
					}
					return []byte(map[string]string{"encryptionroot": "tank/incus", "keyformat": "hex", "encryption": "aes-256-gcm", "mountpoint": "none", "keylocation": "prompt"}[args[4]]), nil
				}
				if args[0] == "load-key" {
					loaded = true
					return nil, nil
				}
				t.Fatal("unexpected command", name, args)
				return nil, nil
			}}
			err := storage.Unlock(context.Background(), Target{Dataset: "tank/incus", Fingerprint: keyFingerprintForTest(t, key)}, []byte(key))
			if failImport {
				if !errors.Is(err, propertyErr) || !strings.Contains(err.Error(), "import permission denied") || loaded {
					t.Fatal("import failure hidden", err)
				}
			} else if err != nil || !loaded {
				t.Fatal("confirmed offline pool did not recover", err)
			}
			if imports == 0 {
				t.Fatal("confirmed absent pool was not imported")
			}
		})
	}
}
