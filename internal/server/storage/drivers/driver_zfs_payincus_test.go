package drivers

import (
	"os"
	"path/filepath"
	"testing"
)

// PayIncus: native pool PATCH must reject shrink/invalid sizes before truncating a live file vdev.
func TestPayIncusNativeZFSLoopRejectsShrink(t *testing.T) {
	t.Setenv("INCUS_DIR", t.TempDir())
	path := loopFilePath("resize-test")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, []byte("preserve"), 0o600) != nil || os.Truncate(path, 2<<30) != nil {
		t.Fatal("cannot prepare sparse loop fixture")
	}
	driver := &zfs{common: common{name: "resize-test", config: map[string]string{"source": path}}}
	for _, size := range []string{"1GiB", "0", "-1GiB", "not-a-size"} {
		if driver.Update(map[string]string{"size": size}) == nil {
			t.Fatal("unsafe native pool update accepted", size)
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() != 2<<30 {
			t.Fatal("rejected native update changed the backing file", size, err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := make([]byte, 8)
	_, err = f.Read(data)
	if err != nil || string(data) != "preserve" {
		t.Fatal("rejected native update changed the original data", err)
	}
}
