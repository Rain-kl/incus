package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZFSKernelPreflightAndPackageSelection(t *testing.T) {
	for _, mode := range []string{"loaded", "module-file", "existing-headers", "missing-headers"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			kernel := "7.0.14-orbstack-fixture"
			modules := filepath.Join(root, "modules")
			sysModules := filepath.Join(root, "sys-module")
			release := filepath.Join(root, "os-release")
			source := filepath.Join(root, "debian.sources")
			if os.WriteFile(release, []byte("ID=debian\nVERSION_ID=13\n"), 0o600) != nil || os.WriteFile(source, []byte("Components: main contrib\n"), 0o600) != nil {
				t.Fatal("fixture creation failed")
			}
			var fixturePath string
			switch mode {
			case "loaded":
				fixturePath = filepath.Join(sysModules, "zfs", "version")
			case "module-file":
				fixturePath = filepath.Join(modules, kernel, "updates", "dkms", "zfs.ko.zst")
			case "existing-headers":
				fixturePath = filepath.Join(modules, kernel, "build", "Makefile")
			}
			if fixturePath != "" {
				if os.MkdirAll(filepath.Dir(fixturePath), 0o700) != nil || os.WriteFile(fixturePath, nil, 0o600) != nil {
					t.Fatal("kernel fixture creation failed")
				}
			}
			missing := errors.New("apt-cache: exit status 100: E: No packages found")
			f := &fakeRunner{reply: func(c call) ([]byte, error) {
				if c.name == "uname" {
					return []byte(kernel), nil
				}
				if c.name == "apt-cache" {
					return nil, missing
				}
				return nil, nil
			}}
			b := &backend{runner: f, releasePath: release, sourcePath: source, modulesPath: modules, sysModulesPath: sysModules}
			err := b.prepareZFS(context.Background())
			if mode == "missing-headers" {
				if !errors.Is(err, missing) || !strings.Contains(err.Error(), kernel) || !strings.Contains(err.Error(), "OrbStack") {
					t.Fatalf("missing kernel prerequisites were hidden: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			installed := false
			for _, c := range f.calls {
				args := strings.Join(c.args, " ")
				if c.name == "apt-get" && strings.HasPrefix(args, "install ") {
					installed = true
					if strings.Contains(args, "linux-headers-") {
						t.Fatal("unnecessary or unavailable headers were installed")
					}
					if strings.Contains(args, "zfs-dkms") != (mode == "existing-headers") {
						t.Fatal("DKMS selection ignored the available module")
					}
				}
			}
			if installed != (mode != "missing-headers") {
				t.Fatal("unsupported kernel reached package installation")
			}
		})
	}
}
