//go:build linux

package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ForgetDestroyed removes only an absent encryption root with the expected recorded identity.
func (m *Manager) ForgetDestroyed(ctx context.Context, dataset, guid string) error {
	if !ValidEncryptionRoot(dataset) {
		return errors.New("invalid encryption root")
	}
	state, err := m.Status()
	if err != nil {
		return err
	}
	if guid != "" && state.DestroyedStorage[dataset] == guid {
		return m.transact(true, func(s *State) error {
			for i, target := range s.Targets {
				if target.Dataset != dataset {
					continue
				}
				if target.GUID != guid {
					return errors.New("registered dataset identity changed")
				}
				s.Targets = append(s.Targets[:i], s.Targets[i+1:]...)
				break
			}
			delete(s.StorageLocks, dataset)
			return nil
		})
	}
	parent := strings.Split(dataset, "/")[0]
	output, err := m.storage.Run(ctx, "zfs", []string{"list", "-H", "-t", "filesystem,volume", "-o", "name", "-r", parent}, nil)
	if err != nil {
		return errors.New("cannot verify storage deletion")
	}
	names := strings.Fields(string(output))
	parentFound := false
	for _, name := range names {
		if name == parent {
			parentFound = true
		}
		if name == dataset || strings.HasPrefix(name, dataset+"/") {
			return errors.New("dataset still exists; refusing registration removal")
		}
	}
	if !parentFound {
		return errors.New("parent pool unavailable; retain registration and key")
	}
	return m.transact(true, func(s *State) error {
		for i, target := range s.Targets {
			if target.Dataset != dataset {
				continue
			}
			if guid == "" || target.GUID != guid {
				return errors.New("registered dataset identity changed")
			}
			s.Targets = append(s.Targets[:i], s.Targets[i+1:]...)
			delete(s.StorageLocks, dataset)
			return nil
		}
		delete(s.StorageLocks, dataset)
		return nil
	})
}

// TrackDeletion records proof only after the native ZFS destroy command succeeds.
func (m *Manager) TrackDeletion(ctx context.Context, source string) (func() error, error) {
	noop := func() error { return nil }
	if _, err := os.Stat(m.dir); os.IsNotExist(err) {
		return noop, nil
	}
	state, err := m.Status()
	if err != nil {
		return nil, err
	}
	targets := []Target{}
	for _, target := range state.Targets {
		if target.Dataset != source && !strings.HasPrefix(target.Dataset, source+"/") {
			continue
		}
		guid, err := m.storage.property(ctx, target.Dataset, "guid")
		if err != nil || guid != target.GUID {
			return nil, errors.New("cannot verify encryption identity before deletion")
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return noop, nil
	}
	return func() error {
		err := m.transact(true, func(s *State) error {
			if s.DestroyedStorage == nil {
				s.DestroyedStorage = map[string]string{}
			}
			for _, target := range targets {
				for _, current := range s.Targets {
					if current.Dataset == target.Dataset && current.GUID == target.GUID {
						s.DestroyedStorage[target.Dataset] = target.GUID
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, target := range targets {
			if target.Dataset == source {
				return m.storage.reclaimManagedPool(ctx, source, target.Fingerprint)
			}
		}
		return nil
	}, nil
}

// TrackDeletion connects native successful destruction to later control-plane key cleanup.
func TrackDeletion(ctx context.Context, source string) (func() error, error) {
	return Default().TrackDeletion(ctx, source)
}

// reclaimManagedPool releases only our own empty backing pool after its encryption root was destroyed.
func (s *Storage) reclaimManagedPool(ctx context.Context, dataset, fingerprint string) error {
	parts := strings.Split(dataset, "/")
	if len(parts) != 2 || parts[1] != "incus" || len(fingerprint) != 64 {
		return nil
	}
	parent := parts[0]
	marker, err := s.property(ctx, parent, "payincus:key-fingerprint")
	if err != nil || marker != fingerprint {
		return nil
	}
	output, err := s.Run(ctx, "zfs", []string{"list", "-H", "-t", "filesystem,volume", "-o", "name", "-r", parent}, nil)
	if err != nil {
		return errors.New("cannot verify empty managed pool")
	}
	names := strings.Fields(string(output))
	if len(names) != 1 || names[0] != parent {
		return nil
	}
	var backing string
	var before os.FileInfo
	if s.ImportDir != "" {
		candidate := filepath.Join(s.ImportDir, parent+".img")
		info, err := os.Lstat(candidate)
		if err == nil && info.Mode().IsRegular() {
			status, err := s.Run(ctx, "zpool", []string{"status", "-P", parent}, nil)
			if err != nil {
				return errors.New("cannot verify managed file vdev")
			}
			for _, field := range strings.Fields(string(status)) {
				if field == candidate {
					backing, before = candidate, info
				}
			}
		}
	}
	if _, err := s.Run(ctx, "zpool", []string{"destroy", parent}, nil); err != nil {
		return errors.New("empty managed pool cleanup failed")
	}
	if backing != "" {
		after, err := os.Lstat(backing)
		if err != nil || !os.SameFile(before, after) {
			return errors.New("managed backing file identity changed")
		}
		if os.Remove(backing) != nil {
			return errors.New("managed backing file cleanup failed")
		}
	}
	return nil
}
