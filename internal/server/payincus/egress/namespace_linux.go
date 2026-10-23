package egress

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
)

const namespacePath = "/run/netns/" + namespace

// PrivateTmp/ProtectHome give the Agent a slave mount namespace. Creating an
// ip-netns bind mount there leaves only its ordinary placeholder file visible to
// PID 1 and to incudal-xray.service. Always manage named namespaces in PID 1's
// mount namespace, without removing the Agent's filesystem sandbox.
func networkCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name == "ip" {
		for _, arg := range args {
			// Namespace names also occur in `ip link ... netns NAME` and
			// `ip -n NAME ...`, not only in `ip netns exec`.
			if arg == "netns" || arg == "-n" || arg == "-netns" || arg == "--netns" {
				hostArgs := append([]string{"--mount=/proc/1/ns/mnt", "--", name}, args...)
				return exec.CommandContext(ctx, "nsenter", hostArgs...)
			}
		}
	}

	return exec.CommandContext(ctx, name, args...)
}

func isNetworkNamespace(file *os.File) bool {
	// NS_GET_NSTYPE only succeeds for nsfs handles. Never publish an ordinary
	// placeholder or a namespace of another type under the managed netns name.
	kind, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), 0xb703, 0)
	return errno == 0 && kind == syscall.CLONE_NEWNET
}

func ensureHostNamespace(ctx context.Context) error {
	if networkCommand(ctx, "ip", "netns", "exec", namespace, "true").Run() == nil {
		return nil
	}
	// Upgrade recovery: retain the existing namespace, veth peers and routes from
	// the old Agent's private mount view. Copying the file would lose the namespace.
	file, err := os.Open(namespacePath)
	if err == nil {
		defer file.Close()
		if isNetworkNamespace(file) {
			// ExtraFiles preserves the actual nsfs reference across setns. mount's usual
			// realpath canonicalization would re-resolve its old pathname in the host's
			// mount view and bind the placeholder instead; it MUST be disabled.
			cmd := exec.CommandContext(ctx, "nsenter", "--mount=/proc/1/ns/mnt", "--", "mount", "--no-canonicalize", "--bind", "/proc/self/fd/3", namespacePath)
			cmd.ExtraFiles = []*os.File{file}
			if cmd.Run() != nil {
				return fmt.Errorf("publish managed network namespace failed")
			}

			if networkCommand(ctx, "ip", "netns", "exec", namespace, "true").Run() != nil {
				return fmt.Errorf("published managed network namespace is unavailable")
			}

			log.Printf("[egress] published existing network namespace to host mount namespace")
			return nil
		}
	}
	// An upgraded/restarted Agent may no longer hold the old private nsfs mount.
	// ip netns add uses O_EXCL and cannot recreate a stale ordinary placeholder.
	// Inspect through PID 1's root so a private mount cannot hide its true type.
	hostPath := "/proc/1/root" + namespacePath
	{
		host, openErr := os.Open(hostPath)
		if openErr == nil {
			defer host.Close()
			var fs syscall.Statfs_t
			info, statErr := host.Stat()
			if syscall.Fstatfs(int(host.Fd()), &fs) != nil || statErr != nil || fs.Type == 0x6e736673 || !info.Mode().IsRegular() || info.Size() != 0 {
				return fmt.Errorf("managed namespace path is not an empty placeholder")
			}
			{
				err := os.Remove(hostPath)
				if err != nil {
					return fmt.Errorf("remove stale managed namespace placeholder failed")
				}
			}
		} else if !os.IsNotExist(openErr) {
			return fmt.Errorf("inspect managed namespace path failed")
		}
	}

	return run(ctx, "ip", "netns", "add", namespace)
}
