//go:build linux

package security

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestZFSNativeLifecycle uses only an explicitly marked disposable VM and an anonymous file vdev.
func TestZFSNativeLifecycle(t *testing.T) {
	if os.Getenv("RUN_NODE_SECURITY_ZFS_TEST") != "1" {
		t.Skip("requires an isolated ZFS test VM")
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil || !bytes.Contains(cmdline, []byte("payincus_zfs_test_vm=1")) {
		t.Fatal("requires the explicitly marked isolated test VM")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatal("native operation failed:", args[0], args[1], err, string(output))
		}
	}
	// A preallocated file vdev avoids a udev dependency in the minimal guest.
	vdev, err := os.CreateTemp(t.TempDir(), "vdev-")
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Fallocate(int(vdev.Fd()), 0, 0, 512<<20)
	closeErr := vdev.Close()
	if err != nil || closeErr != nil {
		t.Fatal("cannot allocate isolated vdev")
	}
	run("zpool", "create", "-o", "cachefile=none", "-O", "mountpoint=none", "-O", "canmount=off", "payincustest", vdev.Name())
	t.Cleanup(func() { _, _ = Run(context.Background(), "zpool", []string{"destroy", "payincustest"}, nil) })
	m := New(filepath.Join(t.TempDir(), "state"))
	m.storage.ImportDir = filepath.Dir(vdev.Name())
	key := []byte("abababababababababababababababababababababababababababababababab")
	defer clear(key)
	fp, _ := KeyFingerprint(string(key))
	target := Target{Dataset: "payincustest/private", Fingerprint: fp}
	err = m.storage.Create(ctx, target.Dataset, key)
	if err != nil {
		t.Fatal(err)
	}
	// This invokes load-key -n against an already loaded encryption root.
	err = m.storage.Unlock(ctx, target, key)
	if err != nil {
		t.Fatal("loaded-key verification failed", err)
	}
	err = m.Register(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := m.Status()
	target = state.Targets[0]
	mountpoint := filepath.Join(t.TempDir(), "data")
	run("zfs", "set", "mountpoint="+mountpoint, target.Dataset)
	mounted, err := m.storage.property(ctx, target.Dataset, "mounted")
	if err != nil {
		t.Fatal(err)
	}
	if mounted != "yes" {
		run("zfs", "mount", target.Dataset)
	}
	file := filepath.Join(mountpoint, "marker")
	err = os.WriteFile(file, []byte("isolated encrypted fixture"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	err = m.storage.Lock(ctx, target)
	_ = held.Close()
	if err == nil {
		t.Fatal("busy filesystem falsely reported locked")
	}
	m.SetStop(func(context.Context) error { return nil })
	err = m.Trigger("panel_request")
	if err != nil {
		t.Fatal(err)
	}
	m.Lockdown(ctx)
	state, err = m.Status()
	if err != nil || len(state.Results) != 2 || !state.Results[0].Success || !state.Results[1].Success {
		t.Fatal("containment verification failed", state.Results, err)
	}
	_, err = os.ReadFile(file)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("data still visible after unmount")
	}
	if m.Recover(state.Revision) == nil {
		t.Fatal("recovered without loaded key")
	}
	wrong := []byte("cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd")
	wrongTarget := target
	wrongTarget.Fingerprint, _ = KeyFingerprint(string(wrong))
	if m.storage.Unlock(ctx, wrongTarget, wrong) == nil {
		t.Fatal("ZFS accepted wrong key")
	}
	clear(wrong)
	err = m.storage.Unlock(ctx, target, key)
	if err != nil {
		t.Fatal(err)
	}
	if m.Check() == nil {
		t.Fatal("loading key silently recovered node")
	}
	err = m.Recover(state.Revision)
	if err != nil {
		t.Fatal(err)
	}
	run("zfs", "mount", target.Dataset)
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "isolated encrypted fixture" {
		t.Fatal("data did not survive lock/unlock")
	}
	err = m.storage.Lock(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	run("zpool", "export", "payincustest")
	err = m.storage.Unlock(ctx, target, key)
	if err != nil {
		t.Fatal("exported file-backed pool could not be imported and unlocked", err)
	}
	run("zfs", "mount", target.Dataset)
	data, err = os.ReadFile(file)
	if err != nil || string(data) != "isolated encrypted fixture" {
		t.Fatal("data did not survive pool export/import")
	}
}
