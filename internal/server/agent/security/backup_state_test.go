//go:build linux

package security

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupStateAuthenticatedAndFailClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	m := New(dir)
	if data, err := m.LoadBackupState(); err != nil || len(data) != 0 {
		t.Fatal(data, err)
	}
	plain := []byte(`{"destination":"/mnt/private-backup","status":"running"}`)
	expected := append([]byte(nil), plain...)
	if err := m.SaveBackupState(plain); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "instance-backups"))
	if err != nil || bytes.Contains(stored, expected) {
		t.Fatal("plaintext journal", err)
	}
	loaded, err := m.LoadBackupState()
	if err != nil || !bytes.Equal(loaded, expected) {
		t.Fatal(err)
	}
	stored[len(stored)-1] ^= 1
	if err = os.WriteFile(filepath.Join(dir, "instance-backups"), stored, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.LoadBackupState(); err == nil {
		t.Fatal("tampered journal accepted")
	}
	if err = os.Remove(filepath.Join(dir, "seed")); err != nil {
		t.Fatal(err)
	}
	if err = m.SaveBackupState(expected); err == nil {
		t.Fatal("lost state key regenerated")
	}
}
