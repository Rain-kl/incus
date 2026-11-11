package localapi

import (
	"os"
	"path/filepath"
)

// SocketCandidates returns the Unix socket for the configured Incus state directory.
func SocketCandidates() []string {
	dir := os.Getenv("INCUS_DIR")
	if dir == "" {
		dir = "/var/lib/incus"
	}

	return []string{filepath.Join(dir, "unix.socket")}
}
