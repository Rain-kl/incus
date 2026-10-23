package egress

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkerDiscoveryBeforeDaemonStartup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	copied := filepath.Join(dir, "network-worker")
	found, err := locateWorker(copied)
	if err != nil || found != copied {
		t.Fatal(found, err)
	}

	_, err = locateWorker(filepath.Join(dir, "incusd"))
	if err == nil {
		t.Fatal("attempted to run incusd as the helper")
	}

	helper := filepath.Join(dir, "payincus-network-worker")
	err = os.WriteFile(helper, []byte("helper"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	found, err = locateWorker(filepath.Join(dir, "incusd"))
	if err != nil || found != helper {
		t.Fatal(found, err)
	}
}
