package diagnostics

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskLogNameCannotEscapeNodeLogDirectory(t *testing.T) {
	for _, name := range []string{"../other", "x/y", "-exec", "", "vm;id", strings.Repeat("a", 101)} {
		if InstanceName.MatchString(name) {
			t.Fatalf("accepted unsafe instance name %q", name)
		}
	}

	if !InstanceName.MatchString("cn002-20261017200640-bd4s") {
		t.Fatal("rejected valid instance name")
	}
}

func TestQemuLogFollowAndCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qemu.log")
	{
		err := os.WriteFile(path, []byte("booting\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 8)
	done := make(chan struct{})
	go func() { defer close(done); followQemuLogFile(ctx, path, func(line string) { lines <- line }) }()
	receive := func(want string) {
		t.Helper()
		select {
		case line := <-lines:
			if !strings.Contains(line, want) {
				t.Fatalf("got %q, want %q", line, want)
			}

		case <-time.After(5 * time.Second):
			t.Fatalf("missing %q", want)
		}
	}

	receive("booting")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = file.WriteString("KVM failed\n")
	_ = file.Close()
	receive("KVM failed")
	// Log truncation must reset the read cursor.
	{
		err := os.WriteFile(path, []byte("new\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	receive("new")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector leaked after cancellation")
	}
}

func TestJournalInstanceBoundary(t *testing.T) {
	if containsTaskInstance("failed vm-10", "vm-1") {
		t.Fatal("mixed instance logs")
	}

	if !containsTaskInstance("failed instance=vm-1: KVM missing", "vm-1") {
		t.Fatal("missing matching log")
	}
}

func TestLXCLogDrainsLastFailureWhenTaskEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lxc.log")
	err := os.WriteFile(path, []byte("starting\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		followInstanceLogFile(ctx, path, "lxc", func(line string) { lines <- line })
	}()
	select {
	case line := <-lines:
		if line != "[lxc] starting" {
			t.Fatal(line)
		}
	case <-time.After(time.Second):
		t.Fatal("LXC log did not start")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("Failed to access lxc/rootfs\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("LXC log collector did not stop")
	}
	select {
	case line := <-lines:
		if line != "[lxc] Failed to access lxc/rootfs" {
			t.Fatal(line)
		}
	default:
		t.Fatal("task cancellation lost the actual LXC failure")
	}
}
