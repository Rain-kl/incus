//go:build linux

package security

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var poolNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,39}$`)

// CreatePool provisions only a new empty device or an exclusively created file, never force-overwriting.
// directory optionally overrides the default file-pool directory; it is only valid for file-backed pools.
func (s *Storage) CreatePool(ctx context.Context, name, device string, sizeGiB uint64, directory string, preallocate bool, fingerprint string) error {
	if !poolNamePattern.MatchString(name) || (device == "") == (sizeGiB == 0) || sizeGiB > 65536 || (device != "" && directory != "") {
		return errors.New("invalid pool name or backing device/size")
	}
	// Resume only a pool carrying the identity marker installed by this operation.
	marker, err := s.Run(ctx, "zfs", []string{"get", "-H", "-o", "value", "payincus:key-fingerprint", name}, nil)
	if err == nil {
		if strings.TrimSpace(string(marker)) == fingerprint {
			return nil
		}
		return errors.New("pool name already exists with another identity; refusing overwrite")
	}
	present, inspectErr := s.poolPresent(ctx, name)
	if inspectErr != nil {
		return fmt.Errorf("pool identity unavailable: %w; %v", err, inspectErr)
	}
	if present {
		return fmt.Errorf("pool identity unavailable: %w", err)
	}

	target := device
	if device == "" {
		dir := s.ImportDir
		if directory != "" {
			if !ValidPoolDirectory(directory) {
				return errors.New("invalid pool file directory; expected a clean absolute path outside /dev, /proc, /sys")
			}
			dir = directory
		}
		if dir == "" {
			return errors.New("pool file directory unavailable")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cannot prepare pool file directory: %w", err)
		}
		target = filepath.Join(dir, name+".img")
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("pool file already exists or cannot be created; refusing overwrite: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("cannot close new pool file: %w", err)
		}
		if preallocate {
			if _, err = s.Run(ctx, "fallocate", []string{"-l", strconv.FormatUint(sizeGiB<<30, 10), target}, nil); err != nil {
				return fmt.Errorf("pool file allocation failed; file retained for inspection: %w", err)
			}
		} else if err = os.Truncate(target, int64(sizeGiB<<30)); err != nil {
			// Sparse file: reserves only the maximum size, disk blocks are allocated on demand.
			return fmt.Errorf("pool file sizing failed; file retained for inspection: %w", err)
		}
	} else {
		if !strings.HasPrefix(device, "/dev/") || strings.Contains(device, "..") {
			return errors.New("expected a dedicated device under /dev")
		}
		info, err := os.Stat(device)
		if err != nil {
			return fmt.Errorf("cannot inspect block device: %w", err)
		}
		if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return errors.New("not an available block device")
		}
		output, err := s.Run(ctx, "lsblk", []string{"-n", "-o", "MOUNTPOINTS,FSTYPE", device}, nil)
		if err != nil {
			return fmt.Errorf("cannot inspect device filesystem or mounts: %w", err)
		}
		if strings.TrimSpace(string(output)) != "" {
			return errors.New("device contains a filesystem or mount; refusing overwrite")
		}
	}
	if _, err := s.Run(ctx, "zpool", []string{"create", "-o", "ashift=12", "-O", "payincus:key-fingerprint=" + fingerprint, "-O", "mountpoint=none", "-O", "canmount=off", name, target}, nil); err != nil {
		return fmt.Errorf("pool creation failed; no force overwrite was attempted: %w", err)
	}
	return nil
}

// ValidPoolDirectory accepts only clean absolute paths that are not kernel/device pseudo filesystems.
func ValidPoolDirectory(dir string) bool {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" || strings.ContainsAny(dir, "\x00\n\r") {
		return false
	}
	for _, banned := range []string{"/dev", "/proc", "/sys"} {
		if dir == banned || strings.HasPrefix(dir, banned+"/") {
			return false
		}
	}
	return true
}
