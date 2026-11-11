//go:build linux

package security

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// StorageLock retains the exact running identities until each successful restore is acknowledged.
type StorageLock struct {
	Locked   bool             `json:"locked"`
	Complete bool             `json:"complete"`
	Resume   []ResumeInstance `json:"resumeInstances"`
}

// StorageRecovery projects pending identities with the revision required for acknowledgement.
type StorageRecovery struct {
	Locked   bool             `json:"locked"`
	Revision uint64           `json:"revision"`
	Resume   []ResumeInstance `json:"resumeInstances"`
}

// SetPoolWorkloads installs native instance adapters scoped to a registered encryption root.
func (m *Manager) SetPoolWorkloads(snapshot func(context.Context, string) ([]ResumeInstance, error), stop func(context.Context, string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolSnapshot = snapshot
	m.poolStop = stop
}

// CheckStorage prevents storage retries from reopening a manually locked encryption root.
func (m *Manager) CheckStorage(dataset string) error {
	if _, err := os.Stat(m.dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := m.Check(); err != nil {
		return err
	}
	state, err := m.Status()
	if err != nil {
		return err
	}
	for root, lock := range state.StorageLocks {
		if lock.Locked && (dataset == root || strings.HasPrefix(dataset, root+"/")) {
			return errors.New("storage is manually locked")
		}
	}
	return nil
}

// CheckStorage applies the process-wide node and manual storage guards.
func CheckStorage(dataset string) error { return Default().CheckStorage(dataset) }

func targetFor(state State, dataset string) (Target, error) {
	for _, target := range state.Targets {
		if target.Dataset == dataset {
			return target, nil
		}
	}
	return Target{}, errors.New("encryption root is not registered")
}

// LockStorage checkpoints running workloads before stopping them and unloading the key.
func (m *Manager) LockStorage(ctx context.Context, dataset string) error {
	if !m.actions.TryLock() {
		return errors.New("storage operation is in progress")
	}
	defer m.actions.Unlock()
	m.starts.Lock()
	defer m.starts.Unlock()
	guardErr := m.Check()
	if guardErr != nil {
		return guardErr
	}
	state, err := m.Status()
	if err != nil {
		return err
	}
	target, err := targetFor(state, dataset)
	if err != nil {
		return err
	}
	m.mu.Lock()
	snapshot, stop := m.poolSnapshot, m.poolStop
	m.mu.Unlock()
	if snapshot == nil || stop == nil {
		return errors.New("native storage workload adapter unavailable")
	}
	lock := state.StorageLocks[dataset]
	if !lock.Locked {
		if len(lock.Resume) > 0 {
			return errors.New("finish pending storage recovery before locking again")
		}
		items, err := snapshot(ctx, dataset)
		if err != nil {
			return err
		}
		if items == nil {
			items = []ResumeInstance{}
		}
		seen := map[string]bool{}
		for _, item := range items {
			identity := item.Project + "/" + item.Name
			if item.Project == "" || item.Name == "" || item.UUID == "" || seen[identity] {
				return errors.New("invalid native workload identity; refusing stop")
			}
			seen[identity] = true
		}
		err = m.transact(true, func(s *State) error {
			if s.Revision != state.Revision || s.Locked {
				return errors.New("state changed")
			}
			if s.StorageLocks == nil {
				s.StorageLocks = map[string]StorageLock{}
			}
			s.StorageLocks[dataset] = StorageLock{Locked: true, Resume: items}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err = stop(ctx, dataset); err != nil {
		return fmt.Errorf("related instances could not all be stopped; storage lock remains pending: %w", err)
	}
	if err = m.storage.Lock(ctx, target); err != nil {
		return fmt.Errorf("storage key could not be unloaded; lock remains pending: %w", err)
	}
	return m.transact(true, func(s *State) error {
		lock := s.StorageLocks[dataset]
		lock.Complete = true
		s.StorageLocks[dataset] = lock
		return nil
	})
}

// UnlockStorage validates a supplied key and clears only the manual storage lock.
func (m *Manager) UnlockStorage(ctx context.Context, dataset string, key []byte) error {
	if !m.actions.TryLock() {
		return errors.New("storage operation is in progress")
	}
	defer m.actions.Unlock()
	state, err := m.Status()
	if err != nil {
		return err
	}
	target, err := targetFor(state, dataset)
	if err != nil {
		return err
	}
	if err = m.storage.Unlock(ctx, target, key); err != nil {
		return fmt.Errorf("storage key validation/loading failed: %w", err)
	}
	return m.transact(true, func(s *State) error {
		if lock, ok := s.StorageLocks[dataset]; ok {
			lock.Locked = false
			lock.Complete = false
			s.StorageLocks[dataset] = lock
		}
		return nil
	})
}

// StorageRecovery reads the pending per-storage recovery plan without modifying it.
func (m *Manager) StorageRecovery(dataset string) (StorageRecovery, error) {
	state, err := m.Status()
	if err != nil {
		return StorageRecovery{}, err
	}
	lock := state.StorageLocks[dataset]
	items := lock.Resume
	if items == nil {
		items = []ResumeInstance{}
	}
	return StorageRecovery{Locked: state.Locked || lock.Locked, Revision: state.Revision, Resume: items}, nil
}

// AcknowledgeStorage removes one verified workload identity from the current revision.
func (m *Manager) AcknowledgeStorage(dataset string, revision uint64, item ResumeInstance) error {
	return m.transact(true, func(s *State) error {
		lock, ok := s.StorageLocks[dataset]
		if !ok || s.Locked || lock.Locked || s.Revision != revision {
			return errors.New("storage recovery state changed")
		}
		for i, pending := range lock.Resume {
			if pending == item {
				lock.Resume = append(lock.Resume[:i], lock.Resume[i+1:]...)
				s.StorageLocks[dataset] = lock
				return nil
			}
		}
		return errors.New("instance is not pending storage recovery")
	})
}
