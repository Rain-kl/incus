package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageCommandCannotOpenInteractiveFrontends(t *testing.T) {
	root := t.TempDir()
	apt := filepath.Join(root, "apt-get")
	// The fixture observes the actual child process, without installing packages on the test host.
	script := `#!/bin/sh
set -eu
test "$DEBIAN_FRONTEND" = noninteractive
test "$DEBIAN_PRIORITY" = critical
test "$APT_LISTCHANGES_FRONTEND" = text
test "$NEEDRESTART_MODE" = l
test "$TERM" = dumb
test "$1 $2 $3 $4" = '-o Dpkg::Use-Pty=0 -o Dpkg::Progress-Fancy=0'
shift 4
test "$1 $2 $3" = 'install -y zfs-dkms'
printf '正在设定 zfs-dkms\n'
printf '\033[31mDKMS build failed: fixture kernel incompatibility\033[0m\n' >&2
exit 100
`
	err := os.WriteFile(apt, []byte(script), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, key := range []string{"DEBIAN_FRONTEND", "DEBIAN_PRIORITY", "APT_LISTCHANGES_FRONTEND", "NEEDRESTART_MODE", "TERM"} {
		t.Setenv(key, "interactive-fixture")
	}
	var logs strings.Builder
	ctx := context.WithValue(context.Background(), operationLogKey{}, &operationLog{writer: &logs})
	for _, name := range []string{"apt-get", apt} {
		output, runErr := (commandRunner{}).Run(ctx, name, []string{"install", "-y", "zfs-dkms"}, "")
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 100 || !strings.Contains(runErr.Error(), "fixture kernel incompatibility") || !strings.Contains(string(output), "正在设定 zfs-dkms") {
			t.Fatalf("APT subprocess lost its environment or raw failure: %s: %v: %s", name, runErr, output)
		}
	}
	if !strings.Contains(logs.String(), "DKMS build failed: fixture kernel incompatibility") || strings.Contains(logs.String(), "[31m") {
		t.Fatal("APT diagnostics were hidden or terminal controls leaked")
	}
	output, err := (commandRunner{}).Run(ctx, "/bin/sh", []string{"-c", `printf '%s' "$DEBIAN_FRONTEND"`}, "")
	if err != nil || string(output) != "interactive-fixture" || os.Getenv("DEBIAN_FRONTEND") != "interactive-fixture" {
		t.Fatalf("APT settings leaked into other commands or the parent environment: %s: %v", output, err)
	}
}

func TestOperationLogsStripCompleteTerminalSequences(t *testing.T) {
	var logs strings.Builder
	ctx := context.WithValue(context.Background(), operationLogKey{}, &operationLog{writer: &logs})
	w := commandLogWriter{ctx: ctx}
	for _, chunk := range []string{"\x1b[?104", "9h\x1b[22;0;0t\x1b[1;48r\x1b(B\x1b[37m\x1b[40m\x1b[H\x1b[2J正在设定 zfs-dkms\x1b[K\n", "\x1b]11;rgb:fefe/ffff/ffff\x1b\\E: package failure [code 100]\x07\n"} {
		_, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	for _, text := range []string{"正在设定 zfs-dkms", "E: package failure [code 100]"} {
		if !strings.Contains(logs.String(), text) {
			t.Fatalf("diagnostic text lost: %s", logs.String())
		}
	}
	for _, control := range []string{"\x1b", "[?1049h", "[22;0;0t", "[1;48r", "(B", "[37m", "[40m", "[2J", "[K", "rgb:fefe"} {
		if strings.Contains(logs.String(), control) {
			t.Fatalf("terminal control left in log: %q", control)
		}
	}
}

func TestPackageCommandDebconfNotice(t *testing.T) {
	if os.Getenv("RUN_TUI_DEBCONF_TEST") != "1" {
		t.Skip("requires an explicitly selected disposable Debian/Ubuntu container")
	}
	root := t.TempDir()
	template := "Template: incus-tui-fixture/notice\nType: note\nDescription: OpenZFS setup fixture\n This critical package notice must not wait for a terminal confirmation.\n"
	err := os.WriteFile(filepath.Join(root, "templates"), []byte(template), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	config := `#!/bin/sh
set -eu
. /usr/share/debconf/confmodule
db_x_loadtemplatefile "$INCUS_TUI_DEBCONF_FIXTURE/templates" incus-tui-fixture
db_input critical incus-tui-fixture/notice || test "$?" = 30
db_go
printf 'debconf configuration completed\n'
`
	err = os.WriteFile(filepath.Join(root, "config"), []byte(config), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(root, "apt-get"), []byte("#!/bin/sh\nexec debconf \"$INCUS_TUI_DEBCONF_FIXTURE/config\"\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("INCUS_TUI_DEBCONF_FIXTURE", root)
	t.Setenv("DEBIAN_FRONTEND", "dialog")
	t.Setenv("TERM", "xterm-256color")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := (commandRunner{}).Run(ctx, filepath.Join(root, "apt-get"), []string{"install", "-y", "zfs-dkms"}, "")
	if err != nil || !strings.Contains(string(output), "debconf configuration completed") || strings.Contains(string(output), "\x1b[") {
		t.Fatalf("real debconf opened a dialog or failed to finish: %v: %s", err, output)
	}
}

func TestCommandStreamsCompleteOutputBeforeExit(t *testing.T) {
	done := filepath.Join(t.TempDir(), "done")
	var logs strings.Builder
	streamed := false
	l := &operationLog{writer: &logs, emit: func(line string) {
		if strings.HasSuffix(line, "first stdout") {
			_, err := os.Stat(done)
			streamed = os.IsNotExist(err)
		}
	}}
	ctx := context.WithValue(context.Background(), operationLogKey{}, l)
	output, err := (commandRunner{}).Run(ctx, "/bin/sh", []string{"-c", `printf 'first stdout\n'; sleep 0.1; printf 'last stderr\n' >&2; touch "$1"; exit 7`, "test", done}, "")
	if err == nil || !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(err.Error(), "last stderr") || !streamed {
		t.Fatalf("command diagnostics were buffered or lost: streamed=%v err=%v", streamed, err)
	}
	if string(output) != "first stdout\nlast stderr\n" || !strings.Contains(logs.String(), "last stderr") {
		t.Fatal("stdout/stderr output missing")
	}
	var text strings.Builder
	l.writer = &text
	l.emit = nil
	longLine := strings.Repeat("x", 128*1024)
	w := commandLogWriter{ctx: ctx}
	_, err = w.Write([]byte(longLine))
	if err != nil {
		t.Fatal(err)
	}
	w.flush()
	if !strings.Contains(text.String(), longLine) {
		t.Fatal("operation output was truncated")
	}
}

func TestOperationLogsRedactOnlySecrets(t *testing.T) {
	var logs strings.Builder
	l := &operationLog{writer: &logs}
	ctx := context.WithValue(context.Background(), operationLogKey{}, l)
	key := strings.Repeat("a", 64)
	payload := []byte(`{"dataset":"tank/private","key":"` + key + `","error":"mount: permission denied"}`)
	logOperation(ctx, "%s", safePayload(ctx, payload))
	logOperation(ctx, "echoed key=%s token=ait_fixture secret=ias_fixture", key)
	if strings.Contains(logs.String(), key) || strings.Contains(logs.String(), "ait_fixture") || strings.Contains(logs.String(), "ias_fixture") {
		t.Fatal("secret leaked in diagnostic log")
	}
	if !strings.Contains(logs.String(), "mount: permission denied") || !strings.Contains(logs.String(), "tank/private") {
		t.Fatal("useful failure context was removed")
	}
}

func TestPanelAcceptsHTTPOnUserChosenHosts(t *testing.T) {
	for _, address := range []string{"http://100.75.147.95:3000", "http://panel.example.com", "http://[2001:db8::1]:3000", "https://panel.example.com"} {
		actual, err := panelURL(address)
		if err != nil || actual != address {
			t.Fatalf("user-selected HTTP(S) panel rejected: %s: %v", address, err)
		}
	}
}

func TestCarriageReturnProgressUpdatesLiveTailWithoutFloodingDisk(t *testing.T) {
	var disk strings.Builder
	l := &operationLog{writer: &disk}
	ctx := context.WithValue(context.Background(), operationLogKey{}, l)
	w := commandLogWriter{ctx: ctx}
	_, err := w.Write([]byte("Installing integration-7.0.2-d61a07997f98 for ubuntu-24.04/amd64\nDownloading payincus-incus.deb (82.4 MiB)\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{
		"\r###=-                                           ",
		"\r################                          ",
		" 40.0%",
		"\r########################                   60.0%",
	} {
		_, err = w.Write([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := l.tail()
	if len(entries) != 3 || !strings.HasSuffix(entries[2], "########################                   60.0%") {
		t.Fatalf("expected live progress bar in third UI line, got: %#v", entries)
	}
	if strings.Contains(disk.String(), "40.0%") || strings.Contains(disk.String(), "60.0%") {
		t.Fatalf("intermediate carriage-return frames flooded disk log: %s", disk.String())
	}
	_, err = w.Write([]byte("\r######################################### 100.0%\nVerified SHA256 checksum for payincus-incus.deb\n"))
	if err != nil {
		t.Fatal(err)
	}
	w.flush()
	entries, _ = l.tail()
	if len(entries) != 4 || !strings.HasSuffix(entries[2], "######################################### 100.0%") || !strings.HasSuffix(entries[3], "Verified SHA256 checksum for payincus-incus.deb") {
		t.Fatalf("completed progress bar was not finalized in place: %#v", entries)
	}
	if !strings.Contains(disk.String(), "######################################### 100.0%") || strings.Contains(disk.String(), "60.0%") {
		t.Fatalf("final progress bar missing or intermediate frame leaked on disk: %s", disk.String())
	}
	if cleanText("frame-1\rframe-2\rframe-3\nnext\r\n") != "frame-3\nnext\n" {
		t.Fatalf("cleanText did not collapse carriage-return frames: %q", cleanText("frame-1\rframe-2\rframe-3\nnext\r\n"))
	}
}
