//go:build linux

package security

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// ErrNonFilePool distinguishes ordinary device/topology pools from a failed file inspection.
var ErrNonFilePool = errors.New("pool is not a plain single file vdev")

// LoopBacking resolves an actual single file vdev, including custom file-pool directories.
func (s *Storage) LoopBacking(ctx context.Context, pool string) (string, error) {
	if !ValidEncryptionRoot(pool) || strings.Contains(pool, "/") {
		return "", errors.New("invalid ZFS pool")
	}
	output, err := s.Run(ctx, "zpool", []string{"status", "-P", pool}, nil)
	if err != nil {
		return "", fmt.Errorf("cannot inspect ZFS pool topology: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "logs" || fields[0] == "cache" || fields[0] == "spares" || fields[0] == "special" || strings.HasPrefix(fields[0], "mirror-") || strings.HasPrefix(fields[0], "raidz")) {
			return "", ErrNonFilePool
		}
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "/") {
			if fields[1] != "ONLINE" {
				return "", errors.New("ZFS vdev is not online")
			}
			paths = append(paths, fields[0])
		}
	}
	if len(paths) != 1 {
		return "", ErrNonFilePool
	}
	if strings.HasPrefix(paths[0], "/dev/") {
		return "", ErrNonFilePool
	}
	info, err := os.Lstat(paths[0])
	if err != nil {
		return "", fmt.Errorf("ZFS backing file unavailable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("ZFS backing file unavailable")
	}
	if !info.Mode().IsRegular() {
		return "", ErrNonFilePool
	}
	return paths[0], nil
}

// ResizePool grows registered managed file vdevs without allocating the sparse extent or accepting shrink.
func (m *Manager) ResizePool(ctx context.Context, dataset string, sizeGiB uint64) error {
	if !ValidDataset(dataset) || sizeGiB == 0 || sizeGiB > 65536 || !strings.HasSuffix(dataset, "/incus") || strings.Count(dataset, "/") != 1 {
		return errors.New("expected a managed dataset and a size of 1-65536 GiB")
	}
	if !m.actions.TryLock() {
		return errors.New("storage operation is in progress")
	}
	defer m.actions.Unlock()
	guardErr := m.Check()
	if guardErr != nil {
		return guardErr
	}
	state, err := m.Status()
	if err != nil {
		return err
	}
	if state.StorageLocks[dataset].Locked {
		return errors.New("storage is manually locked")
	}
	var target *Target
	for _, item := range state.Targets {
		if item.Dataset == dataset {
			copy := item
			target = &copy
		}
	}
	if target == nil {
		return errors.New("storage registration missing")
	}
	guid, err := m.storage.property(ctx, dataset, "guid")
	if err != nil {
		return err
	}
	if guid != target.GUID {
		return errors.New("storage identity changed")
	}
	return m.storage.resizeManagedPool(ctx, dataset, target.Fingerprint, sizeGiB)
}

func (s *Storage) resizeManagedPool(ctx context.Context, dataset, fingerprint string, sizeGiB uint64) error {
	parent := strings.Split(dataset, "/")[0]
	marker, err := s.property(ctx, parent, "payincus:key-fingerprint")
	if err != nil {
		return err
	}
	if marker != fingerprint {
		return errors.New("pool is not managed by this node")
	}
	path, err := s.LoopBacking(ctx, parent)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("cannot open backing file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backing file identity unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("backing file identity unavailable")
	}
	size := int64(sizeGiB << 30)
	if size < info.Size() {
		return errors.New("ZFS pool shrinking is not supported")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Equal size retries must still run online -e after a previous partial failure.
	if err = f.Truncate(size); err != nil {
		return fmt.Errorf("backing file growth failed: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("backing file sync failed: %w", err)
	}
	_, err = s.Run(ctx, "zpool", []string{"online", "-e", parent, path}, nil)
	if err != nil {
		return fmt.Errorf("backing file grew but ZFS expansion failed; retry the same target size: %w", err)
	}
	return nil
}
