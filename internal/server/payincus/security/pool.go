//go:build linux

package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var poolNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,39}$`)

// CreatePool provisions only a new empty device or an exclusively created file, never force-overwriting.
func (s *Storage) CreatePool(ctx context.Context, name, device string, sizeGiB uint64, fingerprint string) error {
	if !poolNamePattern.MatchString(name) || (device == "") == (sizeGiB == 0) || sizeGiB > 65536 {
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
	target := device
	if device == "" {
		if s.ImportDir == "" {
			return errors.New("pool file directory unavailable")
		}
		if os.MkdirAll(s.ImportDir, 0o700) != nil {
			return errors.New("cannot prepare pool file directory")
		}
		target = filepath.Join(s.ImportDir, name+".img")
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return errors.New("pool file already exists or cannot be created; refusing overwrite")
		}
		if file.Close() != nil {
			return errors.New("cannot close new pool file")
		}
		if _, err = s.Run(ctx, "fallocate", []string{"-l", strconv.FormatUint(sizeGiB<<30, 10), target}, nil); err != nil {
			return errors.New("pool file allocation failed; file retained for inspection")
		}
	} else {
		if !strings.HasPrefix(device, "/dev/") || strings.Contains(device, "..") {
			return errors.New("expected a dedicated device under /dev")
		}
		info, err := os.Stat(device)
		if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return errors.New("not an available block device")
		}
		output, err := s.Run(ctx, "lsblk", []string{"-n", "-o", "MOUNTPOINTS,FSTYPE", device}, nil)
		if err != nil || strings.TrimSpace(string(output)) != "" {
			return errors.New("device contains a filesystem or mount; refusing overwrite")
		}
	}
	if _, err := s.Run(ctx, "zpool", []string{"create", "-o", "ashift=12", "-O", "payincus:key-fingerprint=" + fingerprint, "-O", "mountpoint=none", "-O", "canmount=off", name, target}, nil); err != nil {
		return errors.New("pool creation failed; no force overwrite was attempted")
	}
	return nil
}
