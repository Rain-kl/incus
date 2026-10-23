package egress

import (
	"os"
	"os/exec"
	"path/filepath"
)

// The copied helper must also work with systemd's default PATH before incusd starts.
func workerExecutable() (string, error) {
	executable, _ := os.Executable()
	return locateWorker(executable)
}

func locateWorker(executable string) (string, error) {
	if executable != "" {
		name := filepath.Base(executable)
		if name == "network-worker" || name == "payincus-network-worker" {
			return executable, nil
		}

		sibling := filepath.Join(filepath.Dir(executable), "payincus-network-worker")
		info, statErr := os.Stat(sibling)
		if statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return sibling, nil
		}
	}

	return exec.LookPath("payincus-network-worker")
}
