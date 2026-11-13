package backups

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	incus "github.com/lxc/incus/v7/client"
)

// TestLiveRunningInstanceBackupRestore runs only against an explicitly prepared isolated node.
func TestLiveRunningInstanceBackupRestore(t *testing.T) {
	if os.Getenv("INCUS_BACKUP_LIVE_TEST") != "1" {
		t.Skip("isolated live node not requested")
	}
	client, err := incus.ConnectIncusUnix("", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect()
	before, _, err := client.GetInstanceState("backup-live")
	if err != nil || before.Status != "Running" {
		t.Fatal(before, err)
	}
	m := newManager(context.Background(), &memoryStore{}, nativeSource{})
	if response := post(m, strings.Repeat("c", 32), t.TempDir()); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var task Task
	for range 1200 {
		response := httptest.NewRecorder()
		m.ServeHTTP(response, httptest.NewRequest("GET", "/1.0/agent/instance-backups", nil))
		m.mu.Lock()
		task = m.tasks[0]
		done := task.Status != "running"
		m.mu.Unlock()
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if task.Status != "success" {
		t.Fatalf("backup: %+v", task)
	}
	after, _, err := client.GetInstanceState("backup-live")
	if err != nil || after.Status != "Running" || after.Pid != before.Pid {
		t.Fatalf("original instance changed: before=%+v after=%+v err=%v", before, after, err)
	}
	if len(task.Items) != 1 || task.Items[0].Status != "success" {
		t.Fatal(task.Items)
	}
	file, err := os.Open(filepath.Join(task.Directory, task.Items[0].File))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	operation, err := client.CreateInstanceFromBackup(incus.InstanceBackupArgs{BackupFile: file, PoolName: "default", Name: "backup-restored"})
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.Wait(); err != nil {
		t.Fatal(err)
	}
	proof, _, err := client.GetInstanceFile("backup-restored", "/backup-proof")
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	data, err := io.ReadAll(proof)
	if err != nil || string(data) != "online-backup-proof\n" {
		t.Fatalf("restore content: %q %v", data, err)
	}
	t.Logf("running instance kept pid=%d; archive=%d bytes; restored file content matched", after.Pid, task.Items[0].Bytes)
}
