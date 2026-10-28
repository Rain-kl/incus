package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemCommandWithEmptyLoginPath(t *testing.T) {
	t.Setenv("PATH", "")
	output, err := (commandRunner{}).Run(context.Background(), "sh", []string{"-c", `command -v sh; printf '%s' "$PATH"`}, "")
	if err != nil || !strings.Contains(string(output), "/usr/sbin") || !strings.Contains(string(output), "/sbin") || os.Getenv("PATH") != "" {
		t.Fatalf("system command lookup or child PATH failed: %s: %v", output, err)
	}
}

func TestSystemCommandFindsNewlyInstalledModprobe(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", "/usr/bin:/bin")
	originalDirs := systemCommandDirs
	systemCommandDirs = append(append([]string(nil), originalDirs...), root)
	t.Cleanup(func() { systemCommandDirs = originalDirs })
	before := systemCommand(context.Background(), "incus-tui-fixture-modprobe", "zfs")
	if before.Err == nil {
		t.Fatal("missing command unexpectedly resolved")
	}
	// Model kmod becoming available after APT finishes in the same TUI session.
	err := os.WriteFile(filepath.Join(root, "incus-tui-fixture-modprobe"), []byte("#!/bin/sh\ntest \"$1\" = zfs\nprintf 'loaded'\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	output, err := (commandRunner{}).Run(context.Background(), "incus-tui-fixture-modprobe", []string{"zfs"}, "")
	if err != nil || string(output) != "loaded" {
		t.Fatalf("newly installed command was not resolved: %s: %v", output, err)
	}
}
