//go:build linux

package security

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestZFSSparsePoolResizeLive(t *testing.T) {
	if os.Getenv("RUN_NODE_SECURITY_ZFS_TEST") != "1" {
		t.Skip("requires an isolated ZFS test VM")
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil || !bytes.Contains(cmdline, []byte("payincus_zfs_test_vm=1")) {
		t.Fatal("requires the explicitly marked isolated test VM")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	m := New(filepath.Join(dir, "state"))
	m.storage.ImportDir = filepath.Join(dir, "custom-location")
	key := strings.Repeat("ab", 32)
	r := httptest.NewRequest("POST", "/1.0/payincus/storage/pool", strings.NewReader(`{"name":"payincusresize","sizeGiB":1,"key":"`+key+`"}`))
	code, result := m.HandleStorage(r)
	if code != 200 {
		t.Fatal("sparse pool creation failed", code, result)
	}
	t.Cleanup(func() { _, _ = Run(context.Background(), "zpool", []string{"destroy", "payincusresize"}, nil) })
	size := func() uint64 {
		t.Helper()
		output, err := Run(ctx, "zpool", []string{"list", "-Hp", "-o", "size", "payincusresize"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := size()
	mount := filepath.Join(dir, "data")
	_, err = Run(ctx, "zfs", []string{"set", "mountpoint=" + mount, "payincusresize/incus"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(mount, "marker")
	if os.WriteFile(file, []byte("survives sparse pool expansion"), 0o600) != nil {
		t.Fatal("cannot write fixture")
	}
	if err := m.ResizePool(ctx, "payincusresize/incus", 2); err != nil {
		t.Fatal(err)
	}
	if size() <= before {
		t.Fatal("ZFS did not discover the expanded file vdev")
	}
	info, err := os.Stat(filepath.Join(m.storage.ImportDir, "payincusresize.img"))
	if err != nil || info.Size() != 2<<30 || info.Sys().(*syscall.Stat_t).Blocks*512 >= 512<<20 {
		t.Fatal("sparse file was preallocated", err)
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "survives sparse pool expansion" {
		t.Fatal("data lost after expansion", err)
	}
	if m.ResizePool(ctx, "payincusresize/incus", 1) == nil {
		t.Fatal("shrinking accepted")
	}
	if m.ResizePool(ctx, "payincusresize/incus", 2) != nil {
		t.Fatal("same-size retry failed")
	}
}
