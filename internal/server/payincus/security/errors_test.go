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
