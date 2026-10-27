package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
