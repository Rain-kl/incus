package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var systemCommandDirs = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// PayIncus: non-login root shells may omit sbin, including after installing kmod/ZFS.
func systemCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !strings.ContainsRune(name, filepath.Separator) {
		resolved, err := exec.LookPath(name)
		if errors.Is(err, exec.ErrNotFound) {
			for _, dir := range systemCommandDirs {
				resolved, err = exec.LookPath(filepath.Join(dir, name))
				if err == nil {
					break
				}
			}
		}
		if err == nil {
			name = resolved
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// Child package scripts also need administrative tools; do not mutate the TUI environment.
	path := os.Getenv("PATH")
	for _, dir := range systemCommandDirs {
		if path == "" {
			path = dir
		} else if !strings.Contains(":"+path+":", ":"+dir+":") {
			path += ":" + dir
		}
	}
	cmd.Env = append(os.Environ(), "PATH="+path)
	return cmd
}
