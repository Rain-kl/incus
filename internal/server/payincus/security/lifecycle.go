//go:build linux

package security

import (
	"context"
	"errors"
	"strings"
	"time"
)

// StartupExtension advertises normal online startup authorization independently of alarm recovery.
const StartupExtension = "payincus_security_startup_v1"

// StartupKey is ephemeral control-plane key material, never part of State.
type StartupKey struct {
	Dataset string `json:"dataset"`
	Key     string `json:"key"`
}

// Shutdown captures workload identities and unloads registered keys without raising an alarm.
func (m *Manager) Shutdown(ctx context.Context) error {
	actionErr := m.waitActions(ctx)
	if actionErr != nil {
		return actionErr
	}
	defer m.actions.Unlock()
	m.starts.Lock()
	defer m.starts.Unlock()
	state, err := m.Status()
	if err != nil {
		return err
	}
	if len(state.Targets) == 0 && !state.Enabled && !state.Locked && !state.RecoveryPending && !state.StartupRecovering && len(state.ResumeInstances) == 0 {
		return nil
	}
	m.mu.Lock()
	snapshot, stop := m.snapshot, m.stop
	m.mu.Unlock()
	items, snapshotErr := snapshot(ctx)
	var failures []error
	if snapshotErr != nil {
		failures = append(failures, snapshotErr)
	}
	err = m.transact(true, func(s *State) error {
		s.RecoveryPending = !s.Locked
		s.StartupRecovering = !s.Locked
		s.ResumeInstances = mergeResume(s.ResumeInstances, items)
		return nil
	})
	if err != nil {
		failures = append(failures, err)
	}
	stopErr := stop(ctx)
	if stopErr != nil {
		failures = append(failures, stopErr)
	}
	for _, target := range state.Targets {
		// An exported pool cannot retain a loaded encryption key; cold boots need not import it to prove absence.
		present, inspectErr := m.storage.poolPresent(ctx, strings.Split(target.Dataset, "/")[0])
		if inspectErr != nil {
			failures = append(failures, inspectErr)
			continue
		}
		if !present {
			continue
		}
		lockErr := m.storage.Lock(ctx, target)
		if lockErr != nil {
			failures = append(failures, lockErr)
		}
	}
	return errors.Join(failures...)
}

func mergeResume(previous, items []ResumeInstance) []ResumeInstance {
	result := append([]ResumeInstance{}, previous...)
	for _, item := range items {
		found := false
		for _, pending := range result {
			if pending.Project == item.Project && pending.Name == item.Name && pending.UUID == item.UUID {
				found = true
				break
			}
		}
		if !found {
			result = append(result, item)
		}
	}
	return result
}

// SetStartupRecovery installs native identity capture and restoration adapters.
func (m *Manager) SetStartupRecovery(snapshot func(context.Context) ([]ResumeInstance, error), restore func(context.Context, ResumeInstance) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startupSnapshot, m.restore = snapshot, restore
}

// Startup restores only a normal reboot; a persisted alarm or manual pool lock is never cleared here.
func (m *Manager) Startup(ctx context.Context, keys []StartupKey) error {
	actionErr := m.waitActions(ctx)
	if actionErr != nil {
		return actionErr
	}
	defer m.actions.Unlock()
	state, err := m.Status()
	if err != nil {
		return err
	}
	if state.Locked {
		return ErrLocked
	}
	if !state.StartupRecovering {
		return nil
	}
	m.mu.Lock()
	snapshot, restore := m.startupSnapshot, m.restore
	m.mu.Unlock()
	if snapshot == nil || restore == nil {
		return errors.New("native startup adapter unavailable")
	}
	if state.RecoveryPending {
		items, err := snapshot(ctx)
		if err != nil {
			return err
		}
		err = m.transact(true, func(s *State) error {
			if s.Locked {
				return ErrLocked
			}
			s.ResumeInstances = mergeResume(s.ResumeInstances, items)
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, target := range state.Targets {
		if state.StorageLocks[target.Dataset].Locked {
			continue
		}
		var key []byte
		for i := range keys {
			if keys[i].Dataset == target.Dataset {
				key = []byte(keys[i].Key)
				keys[i].Key = ""
				break
			}
		}
		if len(key) == 0 {
			return errors.New("control-plane storage key unavailable")
		}
		err = m.storage.Unlock(ctx, target, key)
		clear(key)
		if err != nil {
			return err
		}
	}
	err = m.transact(true, func(s *State) error {
		if s.Locked {
			return ErrLocked
		}
		s.RecoveryPending = false
		return nil
	})
	if err != nil {
		return err
	}
	state, err = m.Status()
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range state.ResumeInstances {
		err = m.Check()
		if err != nil {
			return err
		}
		err = restore(ctx, item)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		err = m.transact(true, func(s *State) error {
			if s.Locked {
				return ErrLocked
			}
			for i, pending := range s.ResumeInstances {
				if pending == item {
					s.ResumeInstances = append(s.ResumeInstances[:i], s.ResumeInstances[i+1:]...)
					break
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(failures) == 0 {
		err = m.transact(true, func(s *State) error {
			if s.Locked {
				return ErrLocked
			}
			if len(s.ResumeInstances) == 0 {
				s.StartupRecovering = false
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

// StartupPending gates upstream boot scheduling without treating a normal restart as an alarm.
func (m *Manager) StartupPending() bool {
	state, err := m.Status()
	return err != nil || state.RecoveryPending
}

// PrepareStartup closes crash-surviving workloads and keys once before requesting a fresh grant.
func (m *Manager) PrepareStartup(ctx context.Context) error {
	m.startupMu.Lock()
	defer m.startupMu.Unlock()
	if m.startupPrepared {
		return nil
	}
	state, err := m.Status()
	if err != nil {
		return err
	}
	if !state.Locked && state.RecoveryPending {
		err = m.Shutdown(ctx)
		if err != nil {
			return err
		}
	}
	m.startupPrepared = true
	return nil
}

func (m *Manager) waitActions(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.actions.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
