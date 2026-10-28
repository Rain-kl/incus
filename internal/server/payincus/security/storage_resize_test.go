//go:build linux

package security

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestManagedSparsePoolResizeContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.img")
	if os.WriteFile(path, []byte("marker"), 0o600) != nil {
		t.Fatal("fixture")
	}
	m := New(filepath.Join(dir, "state"))
	fp := strings.Repeat("a", 64)
	err := m.transact(true, func(s *State) error {
		s.Targets = []Target{{Dataset: "tank/incus", GUID: "123", Fingerprint: fp}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fail, calls, topology := true, 0, "tank ONLINE\n"+path+" ONLINE 0 0 0\n"
	m.storage.Run = func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
		if name == "zfs" && args[0] == "get" {
			if args[4] == "guid" {
				return []byte("123"), nil
			}
			if args[4] == "payincus:key-fingerprint" {
				return []byte(fp), nil
			}
		}
		if name == "zpool" && args[0] == "status" {
			return []byte(topology), nil
		}
		if name == "zpool" && args[0] == "online" {
			calls++
			if strings.Join(args, " ") != "online -e tank "+path {
				t.Fatal("wrong vdev expansion", args)
			}
			if fail {
				return nil, errors.New("fixture expansion failure")
			}
			return nil, nil
		}
		return nil, errors.New("unexpected command")
	}
	call := func(body string, remote bool) int {
		r := httptest.NewRequest("POST", "/1.0/payincus/storage/resize", strings.NewReader(body))
		if remote {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
		}
		code, _ := m.HandleStorage(r)
		return code
	}
	body := `{"dataset":"tank/incus","sizeGiB":2}`
	if call(body, false) != 403 || calls != 0 {
		t.Fatal("local caller resized storage")
	}
	if call(body, true) != 409 || calls != 1 {
		t.Fatal("partial failure lost")
	}
	fail = false
	if call(body, true) != 200 || calls != 2 {
		t.Fatal("same-size retry did not expand ZFS")
	}
	info, _ := os.Stat(path)
	if info.Size() != 2<<30 || info.Sys().(*syscall.Stat_t).Blocks > 1024 {
		t.Fatal("sparse growth allocated the full file")
	}
	data := make([]byte, 6)
	f, _ := os.Open(path)
	_, _ = f.Read(data)
	_ = f.Close()
	if string(data) != "marker" {
		t.Fatal("existing data overwritten")
	}
	if call(`{"dataset":"tank/incus","sizeGiB":1}`, true) != 409 || calls != 2 {
		t.Fatal("shrinking was attempted")
	}
	if call(`{"dataset":"tank/incus","sizeGiB":0}`, true) != 409 {
		t.Fatal("zero size accepted")
	}
	if call(`{"dataset":"tank/other","sizeGiB":2}`, true) != 409 {
		t.Fatal("unregistered dataset resized")
	}
	topology += "/dev/sda ONLINE 0 0 0\n"
	if call(body, true) != 409 {
		t.Fatal("multi-vdev topology was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m.ResizePool(ctx, "tank/incus", 3) == nil {
		t.Fatal("cancelled resize succeeded")
	}
}
