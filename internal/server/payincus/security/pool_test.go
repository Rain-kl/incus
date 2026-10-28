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

func TestPoolInitializationResumesWithSameKey(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	m.storage.ImportDir = t.TempDir()
	key := strings.Repeat("ab", 32)
	fp, _ := KeyFingerprint(key)
	marker := ""
	created, failDataset, poolCreates := false, true, 0
	m.storage.Run = func(_ context.Context, name string, args []string, input []byte) ([]byte, error) {
		switch name {
		case "fallocate":
			return nil, nil
		case "zpool":

			if args[0] == "create" {
				if strings.Contains(" "+strings.Join(args, " ")+" ", " -f ") || !strings.Contains(strings.Join(args, " "), "payincus:key-fingerprint="+fp) {
					t.Fatal("pool create lost identity or added force overwrite")
				}
				poolCreates++
				marker = fp
				return nil, nil
			}
		case "zfs":
			if args[0] == "get" && args[4] == "payincus:key-fingerprint" {
				if marker == "" {
					return nil, errors.New("pool absent")
				}
				return []byte(marker), nil
			}
			switch args[0] {
			case "create":
				if string(input) != key+"\n" {
					t.Fatal("wrong key input")
				}
				if failDataset {
					return nil, errors.New("fixture disk failure")
				}
				created = true
				return nil, nil
			case "load-key":
				if string(input) != key+"\n" {
					t.Fatal("wrong verification key")
				}
				return nil, nil
			case "get":
				if !created {
					return nil, errors.New("dataset absent")
				}
				props := map[string]string{"guid": "123", "encryptionroot": "pool/incus", "keystatus": "available", "keyformat": "hex", "encryption": "aes-256-gcm", "keylocation": "prompt", "mountpoint": "none"}
				return []byte(props[args[4]]), nil
			}
		}
		return nil, errors.New("unexpected command")
	}
	body := `{"name":"pool","sizeGiB":1,"key":"` + key + `"}`
	call := func(body string) int {
		code, _ := m.HandleStorage(httptest.NewRequest("POST", "/1.0/payincus/storage/pool", strings.NewReader(body)))
		return code
	}
	if code := call(body); code != 409 {
		t.Fatal("partial initialization falsely succeeded", code)
	}
	failDataset = false
	if code := call(body); code != 200 {
		t.Fatal("retry did not resume", code)
	}
	if code := call(body); code != 200 {
		t.Fatal("completed request was not idempotent", code)
	}
	if poolCreates != 1 {
		t.Fatal("retry recreated backing pool")
	}
	if call(strings.Replace(body, key, strings.Repeat("cd", 32), 1)) != 409 {
		t.Fatal("different key replaced identity")
	}
	if AllowedWhileLocked("POST", "/1.0/payincus/storage/pool") || !SensitivePath("/1.0/payincus/storage/pool") {
		t.Fatal("pool endpoint bypasses lock or logs key")
	}
}

func TestPoolNeverOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.img")
	if os.WriteFile(path, []byte("existing"), 0o600) != nil {
		t.Fatal("fixture write failed")
	}
	s := &Storage{ImportDir: dir, Run: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
		if name != "zfs" || args[0] != "get" {
			t.Fatal("existing file reached a write command")
		}
		return nil, errors.New("pool absent")
	}}
	if s.CreatePool(context.Background(), "pool", "", 1, "", false, strings.Repeat("a", 64)) == nil {
		t.Fatal("existing file was accepted")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "existing" {
		t.Fatal("existing file modified")
	}
}
