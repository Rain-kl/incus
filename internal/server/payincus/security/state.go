//go:build linux

// Package security implements the optional node security mode independently of storage encryption.
package security

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Extension identifies the optional node guard contract.
const Extension = "payincus_security_v1"

// ResumeExtension advertises durable workload recovery through native instance APIs.
const ResumeExtension = "payincus_security_resume_v1"

// ErrLocked indicates an operation requires explicit control-plane recovery.
var ErrLocked = errors.New("node is security-locked; recovery requires the control panel")

// PublicError returns the generic public error used to mask security lockdown.
func PublicError() error {
	return errors.New("Failed to begin transaction: sql: database disk image is malformed (11)")
}

// PublicErrorFor masks only an established global lock; unrelated failures retain their cause.
func PublicErrorFor(err error) error {
	if errors.Is(err, ErrLocked) {
		return PublicError()
	}
	return err
}

// Target identifies a dedicated ZFS encryption root without storing its key.
type Target struct {
	Dataset     string `json:"dataset"`
	GUID        string `json:"guid"`
	Fingerprint string `json:"fingerprint"`
}

// Result records independently verified containment outcomes.
type Result struct {
	Target  string `json:"target"`
	Success bool   `json:"success"`
	Detail  string `json:"detail"`
}

// ResumeInstance identifies a pre-lock workload without retaining credentials.
type ResumeInstance struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	Frozen  bool   `json:"frozen"`
}

// State persists policy and lock state separately from storage outcomes.
type State struct {
	StartupRecovering bool                   `json:"startupRecovering"`
	ControlGeneration uint64                 `json:"controlGeneration"`
	RecoveryPending   bool                   `json:"recoveryPending"`
	ResumeCaptured    bool                   `json:"resumeCaptured"`
	ResumeInstances   []ResumeInstance       `json:"resumeInstances"`
	Enabled           bool                   `json:"enabled"`
	Locked            bool                   `json:"locked"`
	Revision          uint64                 `json:"revision"`
	Reason            string                 `json:"reason"`
	UpdatedAt         time.Time              `json:"updatedAt"`
	Targets           []Target               `json:"targets"`
	Results           []Result               `json:"results"`
	StorageLocks      map[string]StorageLock `json:"storageLocks,omitempty"`
	DestroyedStorage  map[string]string      `json:"destroyedStorage,omitempty"`
}

// Event contains sanitized CLI metadata without arguments or secrets.
type Event struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	UID      int       `json:"uid"`
	PID      int       `json:"pid"`
	Command  string    `json:"command"`
	Critical bool      `json:"critical"`
	Blocked  bool      `json:"blocked"`
}

// Manager coordinates cross-process state, delivery queues and containment.
type Manager struct {
	dir             string
	mu              sync.Mutex
	actions         sync.Mutex
	starts          sync.RWMutex
	storage         *Storage
	stop            func(context.Context) error
	snapshot        func(context.Context) ([]ResumeInstance, error)
	poolSnapshot    func(context.Context, string) ([]ResumeInstance, error)
	poolStop        func(context.Context, string) error
	startupSnapshot func(context.Context) ([]ResumeInstance, error)
	restore         func(context.Context, ResumeInstance) error
	startupMu       sync.Mutex
	startupPrepared bool
}

// Directory returns the non-secret node state location.
func Directory() string {
	root := os.Getenv("INCUS_DIR")
	if root == "" {
		root = "/var/lib/incus"
	}
	return filepath.Join(root, "payincus-security")
}

// New constructs a manager with injectable containment adapters.
func New(dir string) *Manager {
	return &Manager{dir: dir, storage: &Storage{Run: Run, ImportDir: filepath.Join(filepath.Dir(dir), "disks")}, snapshot: func(context.Context) ([]ResumeInstance, error) { return nil, nil }, stop: func(context.Context) error { return errors.New("container stop adapter unavailable") }}
}

var singleton = New(Directory())

// Default returns the process-wide node manager.
func Default() *Manager { return singleton }

// SetStop installs the instance containment adapter.
func (m *Manager) SetStop(stop func(context.Context) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop = stop
}

// SetSnapshot installs the read-only workload identity adapter.
func (m *Manager) SetSnapshot(snapshot func(context.Context) ([]ResumeInstance, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = snapshot
}

// AcknowledgeResume removes only the completed workload from the current recovery plan.
func (m *Manager) AcknowledgeResume(revision uint64, item ResumeInstance) error {
	return m.transact(true, func(s *State) error {
		if s.Locked || s.Revision != revision {
			return errors.New("refresh recovery state")
		}
		for i, pending := range s.ResumeInstances {
			if pending == item {
				s.ResumeInstances = append(s.ResumeInstances[:i], s.ResumeInstances[i+1:]...)
				return nil
			}
		}
		return errors.New("workload is not pending recovery")
	})
}

func (m *Manager) transact(write bool, fn func(*State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := os.MkdirAll(filepath.Dir(m.dir), 0o711)
	if err != nil {
		return err
	}
	err = os.MkdirAll(m.dir, 0o700)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(m.dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }()
	data, err := os.ReadFile(filepath.Join(m.dir, "state.json"))
	var state State
	if err == nil {
		err = json.Unmarshal(data, &state)
		if err != nil {
			return errors.New("security state is corrupt; refusing operation")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	before, _ := json.Marshal(state)
	err = fn(&state)
	if err != nil {
		return err
	}
	if !write {
		return nil
	}
	after, _ := json.Marshal(state)
	if bytes.Equal(before, after) && len(data) > 0 {
		return nil
	}
	state.Revision++
	state.UpdatedAt = time.Now().UTC()
	data, err = json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(m.dir, "state.json"), data)
}

func atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	err = os.Rename(file.Name(), path)
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Status reads the current durable revision.
func (m *Manager) Status() (State, error) {
	var s State
	err := m.transact(false, func(state *State) error { s = *state; return nil })
	return s, err
}

// Check rejects locked or unreadable node state.
func (m *Manager) Check() error {
	// Ordinary upstream installations have no custom security state.
	_, err := os.Stat(m.dir)
	if os.IsNotExist(err) {
		return nil
	}
	state, err := m.Status()
	if err != nil {
		return err
	}
	if state.Locked {
		return ErrLocked
	}
	return nil
}

// Check rejects locked or unreadable node state.
func Check() error { return Default().Check() }

// BeginStart fences in-flight starts so containment cannot finish before a container finishes starting.
func BeginStart() (func(), error) { return Default().beginStart() }

func (m *Manager) beginStart() (func(), error) {
	m.starts.RLock()
	err := m.Check()
	if err != nil {
		m.starts.RUnlock()
		return nil, err
	}
	_, statErr := os.Stat(m.dir)
	if os.IsNotExist(statErr) {
		return m.starts.RUnlock, nil
	}
	state, err := m.Status()
	if err != nil || state.RecoveryPending {
		m.starts.RUnlock()
		return nil, errors.New("instance startup temporarily unavailable")
	}
	return m.starts.RUnlock, nil
}

// Boot preserves alarms and fences normal restarts until the control plane authorizes recovery.
func (m *Manager) Boot() error {
	return m.transact(true, func(s *State) error {
		if s.Locked {
			// Retry verified containment after a crash, retaining the original recovery identities.
			s.Results = nil
			s.StartupRecovering = false
		} else {
			s.RecoveryPending = s.Enabled || len(s.Targets) > 0 || len(s.ResumeInstances) > 0
			s.StartupRecovering = s.RecoveryPending
		}
		return nil
	})
}

// Configure updates policy without clearing an existing lock.
func (m *Manager) Configure(enabled bool, revision uint64) error {
	return m.transact(true, func(s *State) error {
		if s.Revision != revision {
			return errors.New("security revision changed; refresh status")
		}
		s.Enabled = enabled
		return nil
	})
}

// Recover requires the current revision and usable registered storage.
func (m *Manager) Recover(revision uint64) error { return m.RecoverApproved(revision, 0) }

// RecoverApproved fences delayed controller decisions already covered by this explicit approval.
func (m *Manager) RecoverApproved(revision, generation uint64) error {
	if !m.actions.TryLock() {
		return errors.New("containment is still in progress")
	}
	defer m.actions.Unlock()
	m.starts.Lock()
	defer m.starts.Unlock()
	state, err := m.Status()
	if err != nil {
		return err
	}
	if state.Revision != revision {
		return errors.New("security revision changed; refresh status")
	}
	if state.Locked && !state.ResumeCaptured {
		return errors.New("recovery checkpoint unavailable")
	}
	if state.Locked && (len(state.Results) == 0 || state.Results[0].Target != "containers" || !state.Results[0].Success) {
		return errors.New("container containment has not completed")
	}
	for _, target := range state.Targets {
		if state.StorageLocks[target.Dataset].Locked {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = m.storage.Ready(ctx, target)
		cancel()
		if err != nil {
			return err
		}
	}
	return m.transact(true, func(s *State) error {
		if s.Revision != revision {
			return errors.New("security revision changed; refresh status")
		}
		if generation > s.ControlGeneration {
			s.ControlGeneration = generation
		}
		s.Locked = false
		s.RecoveryPending = false
		s.StartupRecovering = false
		s.Reason = ""
		s.Results = nil
		return nil
	})
}

// Register records a validated ZFS identity without persisting its key.
func (m *Manager) Register(ctx context.Context, target Target) error {
	guid, err := m.storage.property(ctx, target.Dataset, "guid")
	if err != nil {
		return err
	}
	target.GUID = guid
	err = m.storage.Ready(ctx, target)
	if err != nil {
		return err
	}
	return m.transact(true, func(s *State) error {
		for _, existing := range s.Targets {
			if existing.Dataset == target.Dataset {
				return errors.New("dataset already registered; key replacement requires a separate workflow")
			}
		}
		delete(s.DestroyedStorage, target.Dataset)
		s.Targets = append(s.Targets, target)
		return nil
	})
}

// Trigger persists lockdown before containment begins.
func (m *Manager) Trigger(reason string) error {
	return m.transact(true, func(s *State) error {
		if !s.Locked {
			s.ResumeCaptured = false
		}
		s.Locked = true
		s.StartupRecovering = false
		s.Reason = reason
		s.Results = nil
		return nil
	})
}

// Audit records a sanitized invocation and locks before rejecting a critical request.
func (m *Manager) Audit(command string, critical bool, uid, pid int) (bool, error) {
	blocked := false
	err := m.transact(true, func(s *State) error {
		blocked = critical && (s.Enabled || s.Locked)
		if critical && s.Enabled && !s.Locked {
			s.ResumeCaptured = false
			s.Locked = true
			s.StartupRecovering = false
			s.Reason = "cli:" + command
			s.Results = nil
		}
		return nil
	})
	if err != nil {
		return true, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	err = m.queue(Event{UID: uid, PID: pid, Command: command, Critical: critical, Blocked: blocked})
	return blocked, err
}

func (m *Manager) queue(event Event) error {
	var id [16]byte
	_, err := rand.Read(id[:])
	if err != nil {
		return err
	}
	event.ID = hex.EncodeToString(id[:])
	event.Time = time.Now().UTC()
	path := filepath.Join(m.dir, "events")
	err = os.MkdirAll(path, 0o700)
	if err != nil {
		return err
	}
	files, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(files) >= 2048 {
		return errors.New("CLI audit queue full; command refused until events are delivered")
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(path, event.ID+".json"), data)
}

// Events returns a bounded batch pending durable remote acknowledgement.
func (m *Manager) Events() ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := filepath.Join(m.dir, "events")
	files, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	events := []Event{}
	for _, file := range files {
		if len(events) >= 128 {
			break
		}
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, file.Name()))
		if err != nil {
			return nil, err
		}
		var event Event
		err = json.Unmarshal(data, &event)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// Ack removes only acknowledged event identifiers.
func (m *Manager) Ack(ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ids) > 128 {
		return errors.New("too many event IDs")
	}
	for _, id := range ids {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			return errors.New("invalid event ID")
		}
		err = os.Remove(filepath.Join(m.dir, "events", id+".json"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Lockdown stops workloads and verifies each applicable storage lock.
func (m *Manager) Lockdown(ctx context.Context) {
	if !m.actions.TryLock() {
		return
	}
	defer m.actions.Unlock()
	m.starts.Lock()
	defer m.starts.Unlock()
	state, err := m.Status()
	if err != nil || !state.Locked || len(state.Results) > 0 {
		return
	}
	var checkpointErr error
	if !state.ResumeCaptured {
		m.mu.Lock()
		snapshot := m.snapshot
		m.mu.Unlock()
		items, snapshotErr := snapshot(ctx)
		checkpointErr = snapshotErr
		if checkpointErr == nil {
			err = m.transact(true, func(s *State) error {
				if !s.Locked || s.Revision != state.Revision {
					return errors.New("state changed")
				}
				if s.ResumeInstances == nil {
					s.ResumeInstances = []ResumeInstance{}
				}
				// Preserve failed recovery entries across a subsequent lock; never recapture after stopping.
				for _, item := range items {
					found := false
					for _, previous := range s.ResumeInstances {
						if previous.Project == item.Project && previous.Name == item.Name && previous.UUID == item.UUID {
							found = true
						}
					}
					if !found {
						s.ResumeInstances = append(s.ResumeInstances, item)
					}
				}
				s.ResumeCaptured = true
				return nil
			})
			checkpointErr = err
			if checkpointErr == nil {
				updated, statusErr := m.Status()
				checkpointErr = statusErr
				if statusErr == nil {
					state = updated
				}
			}
		}
	}
	results := []Result{}
	m.mu.Lock()
	stop := m.stop
	m.mu.Unlock()
	err = stop(ctx)
	results = append(results, Result{Target: "containers", Success: err == nil, Detail: safeResult(err)})
	if checkpointErr != nil {
		results = append(results, Result{Target: "recovery_plan", Success: false, Detail: safeResult(checkpointErr)})
	}
	if err == nil {
		for _, target := range state.Targets {
			err = m.storage.Lock(ctx, target)
			results = append(results, Result{Target: target.Dataset, Success: err == nil, Detail: safeResult(err)})
		}
	}
	if len(state.Targets) == 0 {
		results = append(results, Result{Target: "storage", Success: true, Detail: "not_configured"})
	}
	_ = m.transact(true, func(s *State) error {
		if s.Locked && s.Revision == state.Revision {
			s.Results = results
		}
		return nil
	})
}

func safeResult(err error) string {
	if err != nil {
		return "failed; inspect node storage and process occupancy"
	}
	return "complete"
}

// Run performs asynchronous containment while the control channel remains alive.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			actionCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			// Complete crash cleanup even when the connector is offline or has not been configured.
			prepareErr := m.PrepareStartup(actionCtx)
			if prepareErr == nil {
				m.Lockdown(actionCtx)
			}
			cancel()
		}
	}
}

// Report returns bounded management telemetry.
func (m *Manager) Report() map[string]any {
	state, err := m.Status()
	if err != nil {
		return map[string]any{"state": State{Locked: true, Reason: "state_unavailable"}, "events": []Event{}}
	}
	events, err := m.Events()
	if err != nil {
		return map[string]any{"state": State{Locked: true, Reason: "audit_unavailable"}, "events": []Event{}}
	}
	return map[string]any{"state": state, "events": events}
}

// TriggerControl is idempotent and ignores controller decisions superseded by an explicit recovery.
func (m *Manager) TriggerControl(generation uint64) error {
	if generation == 0 {
		return errors.New("invalid controller decision")
	}
	return m.transact(true, func(s *State) error {
		if generation <= s.ControlGeneration {
			return nil
		}
		s.ControlGeneration = generation
		if !s.Locked {
			s.ResumeCaptured = false
		}
		s.Locked = true
		s.StartupRecovering = false
		s.Reason = "panel_request"
		s.Results = nil
		return nil
	})
}
