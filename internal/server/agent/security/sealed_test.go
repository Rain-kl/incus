//go:build linux

package security

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSealedStateAndEventsSurviveManagerReplacement(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	if err := m.Configure(true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Audit("list", false, 1000, 10); err != nil {
		t.Fatal(err)
	}
	clone := New(m.dir)
	state, err := clone.Status()
	if err != nil || !state.Enabled {
		t.Fatal(state, err)
	}
	events, err := clone.Events()
	if err != nil || len(events) != 1 || events[0].Command != "list" {
		t.Fatal(events, err)
	}
	report := clone.Report()
	if _, ok := report["queue"]; ok {
		t.Fatal("disk queue name leaked into transport contract")
	}
	if delivered, ok := report["events"].([]Event); !ok || len(delivered) != 1 {
		t.Fatal("event delivery contract changed", report)
	}

	paths := []string{filepath.Join(m.dir, "index"), filepath.Join(m.dir, "queue", events[0].ID+".bin")}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, plain := range []string{`"enabled"`, `"locked"`, `"command"`, `"list"`, `"resumeInstances"`} {
			if bytes.Contains(data, []byte(plain)) {
				t.Fatal("plaintext record persisted", path)
			}
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("record permissions", err)
		}
	}
	if err := clone.Ack([]string{events[0].ID}); err != nil {
		t.Fatal(err)
	}
	if events, err = m.Events(); err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
}

func TestSealedStateRejectsTamperingMissingKeysAndDeletedIndex(t *testing.T) {
	for _, damage := range []string{"record", "missing-key", "wrong-key", "missing-index", "key-permissions", "symlink-key", "directory-permissions"} {
		t.Run(damage, func(t *testing.T) {
			m := New(filepath.Join(t.TempDir(), "state"))
			if err := m.Configure(true, 0); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(m.dir, "index")
			keyPath := filepath.Join(m.dir, "seed")
			switch damage {
			case "record":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-1] ^= 1
				if err = os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-key":
				if err := os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			case "wrong-key":
				if err := os.WriteFile(keyPath, bytes.Repeat([]byte{42}, 32), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-index":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "directory-permissions":
				if err := os.Chmod(m.dir, 0o777); err != nil {
					t.Fatal(err)
				}
			case "key-permissions":
				if err := os.Chmod(keyPath, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink-key":
				other := filepath.Join(t.TempDir(), "seed")
				if err := os.Rename(keyPath, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, keyPath); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := New(m.dir).Status(); !errors.Is(err, errStateUnavailable) {
				t.Fatal("damaged state accepted", err)
			}
			if err := m.Configure(false, 0); err == nil {
				t.Fatal("damaged state reset through configuration")
			}
			if damage == "missing-key" {
				if _, err := os.Lstat(keyPath); !os.IsNotExist(err) {
					t.Fatal("lost key regenerated")
				}
			}
		})
	}
}

func TestSealedEventsAuthenticateTheirNames(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	if err := m.Configure(true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Audit("list", false, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Audit("info", false, 0, 2); err != nil {
		t.Fatal(err)
	}
	events, err := m.Events()
	if err != nil || len(events) != 2 {
		t.Fatal(err, events)
	}
	a := filepath.Join(m.dir, "queue", events[0].ID+".bin")
	b := filepath.Join(m.dir, "queue", events[1].ID+".bin")
	data, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(b, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Events(); !errors.Is(err, errStateUnavailable) {
		t.Fatal("substituted event accepted", err)
	}
}
