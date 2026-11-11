//go:build linux

package security

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func lifecycleManager(t *testing.T) (*Manager, string, *bool) {
	t.Helper()
	m := New(filepath.Join(t.TempDir(), "state"))
	key := strings.Repeat("ab", 32)
	fingerprint, err := KeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.transact(true, func(state *State) error {
		state.Enabled = true
		state.Targets = []Target{{Dataset: "pool/incus", GUID: "123", Fingerprint: fingerprint}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	available := true
	m.storage.Run = func(_ context.Context, command string, args []string, input []byte) ([]byte, error) {
		switch args[0] {
		case "get":
			properties := map[string]string{"guid": "123", "encryptionroot": "pool/incus", "mounted": "no", "keystatus": "unavailable", "keyformat": "hex", "keylocation": "prompt", "encryption": "aes-256-gcm"}
			if available {
				properties["keystatus"] = "available"
			}
			return []byte(properties[args[4]]), nil
		case "list":
			if command == "zpool" {
				return []byte("pool\n"), nil
			}
			return []byte("pool/incus\n"), nil
		case "unload-key":
			available = false
			return nil, nil
		case "load-key":
			if strings.TrimSpace(string(input)) != key {
				return nil, errors.New("wrong fixture key")
			}
			available = true
			return nil, nil
		default:
			return nil, errors.New("unexpected storage operation")
		}
	}
	return m, key, &available
}

func simulateNormalRestart(t *testing.T, m *Manager) {
	t.Helper()
	m.SetSnapshot(func(context.Context) ([]ResumeInstance, error) { return nil, nil })
	m.SetStop(func(context.Context) error { return nil })
	err := m.Shutdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = m.Boot()
	if err != nil {
		t.Fatal(err)
	}
}

func TestNormalShutdownUnloadsKeysWithoutLockAndFencesStarts(t *testing.T) {
	m, _, available := lifecycleManager(t)
	item := ResumeInstance{Project: "default", Name: "running", UUID: "identity"}
	var order []string
	m.SetSnapshot(func(context.Context) ([]ResumeInstance, error) {
		order = append(order, "snapshot")
		return []ResumeInstance{item}, nil
	})
	m.SetStop(func(context.Context) error {
		state, err := m.Status()
		if err != nil || !state.RecoveryPending || state.Locked || state.ResumeInstances[0] != item {
			t.Fatalf("shutdown did not durably capture recovery before stopping: %#v, %v", state, err)
		}
		order = append(order, "stop")
		return nil
	})
	err := m.Shutdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Status()
	if err != nil || state.Locked || !state.RecoveryPending || *available {
		t.Fatalf("normal shutdown must fence recovery and unload storage without locking: %#v, %v", state, err)
	}
	if strings.Join(order, ",") != "snapshot,stop" {
		t.Fatalf("unexpected shutdown order %v", order)
	}
	_, err = m.beginStart()
	if err == nil {
		t.Fatal("instance start passed the normal shutdown recovery fence")
	}
}

func TestShutdownContinuesAfterSnapshotFailureAndSkipsAbsentPool(t *testing.T) {
	m, _, available := lifecycleManager(t)
	var stopCalled bool
	m.SetSnapshot(func(context.Context) ([]ResumeInstance, error) { return nil, errors.New("snapshot failed") })
	m.SetStop(func(context.Context) error { stopCalled = true; return nil })
	m.storage.Run = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			return nil, nil // Cold boot: successful inventory confirms the pool is not imported.
		}
		t.Fatalf("absent pool must not invoke ZFS property/key commands: %v", args)
		return nil, nil
	}
	err := m.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "snapshot failed") {
		t.Fatalf("shutdown should report snapshot failure after cleanup: %v", err)
	}
	if !stopCalled || !*available {
		t.Fatalf("shutdown skipped stop or probed a pool confirmed absent: stop=%t available=%t", stopCalled, *available)
	}
}

func TestStartupRestoresOnlyAfterKeyLoadAndSuccessfulRecovery(t *testing.T) {
	m, key, available := lifecycleManager(t)
	item := ResumeInstance{Project: "default", Name: "resume-me", UUID: "identity", Frozen: true}
	simulateNormalRestart(t, m)
	state, err := m.Status()
	if err != nil || state.Locked || !state.RecoveryPending || !state.StartupRecovering {
		t.Fatalf("daemon boot converted normal restart to an alarm: %#v, %v", state, err)
	}
	if *available {
		t.Fatal("fixture expected the key to be unloaded across restart")
	}
	var order []string
	m.SetStartupRecovery(func(context.Context) ([]ResumeInstance, error) {
		order = append(order, "snapshot")
		return []ResumeInstance{item}, nil
	}, func(_ context.Context, got ResumeInstance) error {
		if !*available {
			t.Fatal("workload restore ran before ZFS key load")
		}
		if got != item {
			t.Fatalf("restored wrong workload identity: %#v", got)
		}
		order = append(order, "restore")
		return nil
	})
	err = m.Startup(context.Background(), []StartupKey{{Dataset: "pool/incus", Key: key}})
	if err != nil {
		t.Fatal(err)
	}
	state, err = m.Status()
	if err != nil || state.Locked || state.RecoveryPending || state.StartupRecovering || len(state.ResumeInstances) != 0 || !*available {
		t.Fatalf("normal startup did not complete recovery: %#v, %v", state, err)
	}
	if strings.Join(order, ",") != "snapshot,restore" {
		t.Fatalf("unexpected startup order %v", order)
	}
}

func TestStartupRecoveryRetryKeepsExecutorFenceUntilAllWorkloadsRestore(t *testing.T) {
	m, key, _ := lifecycleManager(t)
	simulateNormalRestart(t, m)
	item := ResumeInstance{Project: "default", Name: "delayed", UUID: "delayed-id"}
	var restoreCalls int
	m.SetStartupRecovery(func(context.Context) ([]ResumeInstance, error) { return []ResumeInstance{item}, nil }, func(context.Context, ResumeInstance) error {
		restoreCalls++
		if restoreCalls == 1 {
			return errors.New("fixture restore failure")
		}
		return nil
	})
	err := m.Startup(context.Background(), []StartupKey{{Dataset: "pool/incus", Key: key}})
	if err == nil {
		t.Fatal("partial workload restore unexpectedly succeeded")
	}
	state, err := m.Status()
	if err != nil || state.RecoveryPending || !state.StartupRecovering || len(state.ResumeInstances) != 1 || state.ResumeInstances[0] != item {
		t.Fatalf("partial restore dropped its execution fence or identity: %#v, %v", state, err)
	}
	err = m.Startup(context.Background(), []StartupKey{{Dataset: "pool/incus", Key: key}})
	if err != nil {
		t.Fatal("retrying the remaining workload unexpectedly failed", err)
	}
	state, err = m.Status()
	if err != nil || state.StartupRecovering || len(state.ResumeInstances) != 0 || restoreCalls != 2 {
		t.Fatalf("completed retry did not clear the execution fence: %#v restoreCalls=%d err=%v", state, restoreCalls, err)
	}
}

func TestStartupDoesNotRunManualRecoveryListWithoutStartupAuthorization(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	item := ResumeInstance{Project: "default", Name: "manual", UUID: "manual-id"}
	err := m.transact(true, func(state *State) error {
		state.Enabled = true
		state.Targets = []Target{{Dataset: "pool/incus", GUID: "123"}}
		state.ResumeCaptured = true
		state.ResumeInstances = []ResumeInstance{item}
		state.RecoveryPending = false
		state.StartupRecovering = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var snapshotCalls, restoreCalls int
	m.SetStartupRecovery(func(context.Context) ([]ResumeInstance, error) {
		snapshotCalls++
		return nil, nil
	}, func(context.Context, ResumeInstance) error {
		restoreCalls++
		return nil
	})
	err = m.Startup(context.Background(), []StartupKey{{Dataset: "pool/incus", Key: "should-not-be-used"}})
	if err != nil {
		t.Fatal("manual recovery state should not be consumed by normal startup", err)
	}
	state, err := m.Status()
	if err != nil || len(state.ResumeInstances) != 1 || state.ResumeInstances[0] != item || snapshotCalls != 0 || restoreCalls != 0 {
		t.Fatalf("normal startup consumed an unauthorized manual recovery list: %#v snapshot=%d restore=%d err=%v", state, snapshotCalls, restoreCalls, err)
	}
}

func TestRunPreparesPendingRecoveryWithoutAgentRequest(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	key := strings.Repeat("ab", 32)
	fingerprint, err := KeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	err = m.transact(true, func(state *State) error {
		state.Enabled = true
		state.Targets = []Target{{Dataset: "pool/incus", GUID: "123", Fingerprint: fingerprint}}
		state.RecoveryPending = true
		state.StartupRecovering = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	available := atomic.Bool{}
	available.Store(true)
	m.storage.Run = func(_ context.Context, command string, args []string, _ []byte) ([]byte, error) {
		switch args[0] {
		case "list":
			if command == "zpool" {
				return []byte("pool\n"), nil
			}
			return []byte("pool/incus\n"), nil
		case "get":
			properties := map[string]string{"guid": "123", "encryptionroot": "pool/incus", "mounted": "no", "keyformat": "hex", "keylocation": "prompt", "encryption": "aes-256-gcm", "keystatus": "unavailable"}
			if available.Load() {
				properties["keystatus"] = "available"
			}
			return []byte(properties[args[4]]), nil
		case "unload-key":
			available.Store(false)
			return nil, nil
		default:
			return nil, errors.New("unexpected storage operation")
		}
	}
	stopped := make(chan struct{}, 1)
	m.SetSnapshot(func(context.Context) ([]ResumeInstance, error) { return nil, nil })
	m.SetStop(func(context.Context) error { stopped <- struct{}{}; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	select {
	case <-stopped:
	case <-time.After(2500 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("background lifecycle did not stop workloads while the agent was offline")
	}
	deadline := time.After(2 * time.Second)
	for available.Load() {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("background lifecycle did not unload the key")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background lifecycle did not stop after cancellation")
	}
	state, err := m.Status()
	if err != nil || state.Locked || !state.RecoveryPending || !state.StartupRecovering {
		t.Fatalf("offline normal-restart preparation alarmed the node or cleared its fence: %#v %v", state, err)
	}
}

func TestStartupMissingKeyKeepsRecoveryFenceAndNeverStarts(t *testing.T) {
	m, _, available := lifecycleManager(t)
	simulateNormalRestart(t, m)
	m.SetStartupRecovery(func(context.Context) ([]ResumeInstance, error) { return nil, nil }, func(context.Context, ResumeInstance) error { return nil })
	err := m.Startup(context.Background(), nil)
	if err == nil {
		t.Fatal("startup completed without the control-plane storage key")
	}
	state, err := m.Status()
	if err != nil || state.Locked || !state.RecoveryPending || *available {
		t.Fatalf("missing key must leave an unlocked but fenced node: %#v, %v", state, err)
	}
	_, err = m.beginStart()
	if err == nil {
		t.Fatal("instance started after incomplete key recovery")
	}
}

func TestBootPreservesExistingSecurityLock(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	resume := []ResumeInstance{{Project: "default", Name: "fixture-workload", UUID: "fixture-workload-id"}}
	err := m.transact(true, func(state *State) error {
		state.Enabled = true
		state.Locked = true
		state.Reason = "fixture containment failure"
		state.ResumeCaptured = true
		state.ResumeInstances = resume
		state.ControlGeneration = 7
		state.Results = []Result{{Target: "containers", Success: false, Detail: "fixture stop failure"}}
		return nil
	})
	if err != nil {
		t.Fatal("could not create locked lifecycle fixture", err)
	}
	err = m.Boot()
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Status()
	if err != nil || !state.Locked || state.Reason != "fixture containment failure" || !state.ResumeCaptured || len(state.ResumeInstances) != 1 || state.ResumeInstances[0] != resume[0] || state.ControlGeneration != 7 {
		t.Fatalf("daemon boot cleared a confirmed lock or recovery identity: %#v, %v", state, err)
	}
	if len(state.Results) != 0 {
		t.Fatalf("daemon boot did not clear stale failed containment results for retry: %#v", state.Results)
	}
}

func TestStaleControllerLockCannotRelockApprovedRecovery(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "state"))
	err := m.transact(true, func(state *State) error {
		state.Enabled = true
		state.Locked = true
		state.ResumeCaptured = true
		state.ControlGeneration = 7
		state.Results = []Result{{Target: "containers", Success: true}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	err = m.RecoverApproved(state.Revision, 9)
	if err != nil {
		t.Fatal("administrator recovery failed", err)
	}
	state, err = m.Status()
	if err != nil || state.Locked || state.ControlGeneration != 9 {
		t.Fatalf("approved recovery did not commit controller generation: %#v %v", state, err)
	}
	err = m.TriggerControl(8)
	if err != nil {
		t.Fatal(err)
	}
	state, err = m.Status()
	if err != nil || state.Locked || state.ControlGeneration != 9 {
		t.Fatalf("stale lock receipt relocked a recovered node: %#v %v", state, err)
	}
	err = m.TriggerControl(10)
	if err != nil {
		t.Fatal(err)
	}
	state, err = m.Status()
	if err != nil || !state.Locked || state.ControlGeneration != 10 {
		t.Fatalf("newer lock decision was not applied: %#v %v", state, err)
	}
}
