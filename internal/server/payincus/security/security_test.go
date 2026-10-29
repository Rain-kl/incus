//go:build linux

package security

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPersistentSecurityModeWithoutZFS(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "guard"))
	state, err := m.Status()
	if err != nil || state.Locked || state.Enabled {
		t.Fatal(state, err)
	}
	{
		err = m.Configure(true, state.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
	blocked, err := m.Audit("list", false, 1000, 123)
	if err != nil || blocked {
		t.Fatal(err)
	}
	before, _ := m.Status()
	blocked, err = m.Audit("exec", true, 1000, 124)
	if err != nil || !blocked {
		t.Fatal(err)
	}
	locked, _ := m.Status()
	if !locked.Locked || locked.Revision <= before.Revision {
		t.Fatal(locked)
	}
	if m.Recover(locked.Revision) == nil {
		t.Fatal("recovery raced unfinished containment")
	}
	clone := New(m.dir)
	if !errors.Is(clone.Check(), ErrLocked) {
		t.Fatal("lock did not survive process restart")
	}
	stopCalled := false
	m.SetStop(func(context.Context) error { stopCalled = true; return nil })
	m.Lockdown(context.Background())
	result, _ := m.Status()
	if !stopCalled || len(result.Results) != 2 || result.Results[1].Detail != "not_configured" {
		t.Fatal(result)
	}
	{
		err = m.Configure(false, result.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
	if m.Check() == nil {
		t.Fatal("disabling policy cleared lock")
	}
	state, _ = m.Status()
	if m.Recover(state.Revision-1) == nil {
		t.Fatal("stale recovery accepted")
	}
	{
		err = m.Recover(state.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
	if m.Check() != nil {
		t.Fatal("explicit recovery did not work")
	}
	events, err := m.Events()
	if err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
	bytes, _ := json.Marshal(events)
	if strings.Contains(string(bytes), "args") || strings.Contains(string(bytes), "payload") {
		t.Fatal("raw command retained")
	}
	if m.Ack([]string{"../../state"}) == nil {
		t.Fatal("unsafe acknowledgement accepted")
	}
	{
		err = m.Ack([]string{events[0].ID})
		if err != nil {
			t.Fatal(err)
		}
	}
	events, _ = m.Events()
	if len(events) != 1 {
		t.Fatal(events)
	}
}

func TestRestartCorruptionAndFailedStop(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "guard"))
	_ = m.Boot()
	state, _ := m.Status()
	_ = m.Configure(true, state.Revision)
	{
		err := m.Boot()
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := m.Status()
	if err != nil || state.Locked || !state.RecoveryPending {
		t.Fatal("normal daemon restart must retain a recovery fence without alarming", state, err)
	}
	if _, err := m.beginStart(); err == nil {
		t.Fatal("instance start bypassed pending startup recovery")
	}
	if err := m.Trigger("fixture"); err != nil {
		t.Fatal("explicit security event did not lock the node", err)
	}
	m.SetStop(func(context.Context) error { return errors.New("busy") })
	m.Lockdown(context.Background())
	state, _ = m.Status()
	if !state.Locked || len(state.Results) == 0 || state.Results[0].Success {
		t.Fatal(state)
	}
	{
		err := os.WriteFile(filepath.Join(m.dir, "state.json"), []byte("broken"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
	if m.Check() == nil {
		t.Fatal("corrupt state failed open")
	}
	report := m.Report()
	if !report["state"].(State).Locked {
		t.Fatal(report)
	}
}

func TestConcurrentEventsDoNotLoseLock(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "guard"))
	_ = m.Configure(true, 0)
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(pid int) {
			defer group.Done()
			_, err := m.Audit("exec", true, 0, pid)
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	group.Wait()
	if m.Check() == nil {
		t.Fatal("lock lost")
	}
	events, err := m.Events()
	if err != nil || len(events) != 20 {
		t.Fatal(len(events), err)
	}
}

func TestCommandPolicyAndNoArgumentLeaks(t *testing.T) {
	for _, tt := range []struct {
		args     []string
		critical bool
	}{
		{[]string{"--version"}, false},
		{[]string{"--help"}, false},
		{[]string{"help", "exec"}, false},
		{[]string{"--project", "p", "list"}, false},
		{[]string{"--project=p", "config", "show", "c"}, false},
		{[]string{"storage", "volume", "list", "pool"}, false},
		{[]string{"file", "pull", "c/root/private", "."}, true},
		{[]string{"exec", "c", "--", "sh", "-c", "secret=never-log"}, true},
		{[]string{"console", "c"}, true},
		{[]string{"copy", "c", "clone"}, true},
		{[]string{"publish", "c"}, true},
		{[]string{"config", "set", "c", "raw.lxc", "secret"}, true},
		{[]string{"query", "/1.0/instances/c/files?path=/secret"}, true},
		{[]string{"custom-alias"}, true},
	} {
		command, critical := Command(tt.args)
		if critical != tt.critical {
			t.Fatal(tt.args, command, critical)
		}
		if strings.Contains(command, "secret") || strings.Contains(command, "/") {
			t.Fatal("payload leaked into audit category")
		}
	}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD"} {
		for _, path := range []string{"/1.0/instances", "/1.0/instances/c/files", "/1.0/instances/c/console", "/1.0/storage-pools/p/volumes", "/1.0/operations/x/websocket"} {
			if AllowedWhileLocked(method, path) {
				t.Fatal("allowed sensitive request", method, path)
			}
		}
	}
	if !AllowedWhileLocked("GET", "/internal/containers/c/onstop") || !AllowedWhileLocked("GET", "/internal/containers/c/onstopns") || AllowedWhileLocked("GET", "/internal/containers/c/onstart") {
		t.Fatal("containment hook boundary incorrect")
	}
	if !AllowedWhileLocked("GET", "/1.0/payincus/report") || !AllowedWhileLocked("POST", "/1.0/payincus/security/recover") {
		t.Fatal("management path unavailable")
	}
}

func request(method, path, body string, remote bool) *http.Request {
	r := httptest.NewRequest(method, "http://node/1.0/payincus/security"+path, strings.NewReader(body))
	if remote {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	}
	return r
}

func TestHTTPAuthorityReplayAndFormats(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "guard"))
	code, _ := m.Handle(request("POST", "/policy", `{"enabled":true,"revision":0}`, false))
	if code != 403 {
		t.Fatal("Unix caller enabled mode")
	}
	code, _ = m.Handle(request("POST", "/policy", `{"enabled":true,"revision":0}`, true))
	if code != 200 {
		t.Fatal(code)
	}
	code, _ = m.Handle(request("POST", "/policy", `{"enabled":false,"revision":0}`, true))
	if code != 409 {
		t.Fatal("replayed policy accepted")
	}
	blocked, err := m.Audit("exec", true, 0, 2)
	if err != nil || !blocked {
		t.Fatal("CLI trigger unavailable", err)
	}
	code, _ = m.Handle(request("POST", "/recover", `{"revision":2}`, false))
	if code != 403 {
		t.Fatal("Unix caller recovered node")
	}
	for _, body := range []string{`{}`, `{"enabled":true,"revision":2} {}`, `{"revision":2,"secret":"payload"}`} {
		code, _ = m.Handle(request("POST", "/recover", body, true))
		if code == 200 {
			t.Fatal("malformed recovery accepted", body)
		}
	}
	code, value := m.Handle(request("GET", "/report", "", false))
	if code != 200 || value == nil {
		t.Fatal("agent cannot report while locked")
	}
	code, _ = m.Handle(request("POST", "/ack", `{"ids":["../../state"]}`, false))
	if code != 400 {
		t.Fatal(code)
	}
	code, _ = m.Handle(request("POST", "/lock", "{}", true))
	if code != 200 {
		t.Fatal(code)
	}
}

func TestStorageKeyLifecycleAndBusyFailure(t *testing.T) {
	key := strings.Repeat("ab", 32)
	fp, err := KeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Dataset: "tank/private", Fingerprint: fp, GUID: "123"}
	available := true
	mounted := true
	busy := false
	calls := []string{}
	storage := &Storage{Run: func(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		text := name + " " + strings.Join(args, " ")
		calls = append(calls, text)
		if strings.Contains(text, key) {
			t.Fatal("key in command arguments")
		}
		if name == "zfs" && len(args) > 3 && args[0] == "get" {
			switch args[4] {
			case "guid":
				return []byte("123\n"), nil
			case "encryptionroot":
				return []byte(target.Dataset), nil
			case "encryption":
				return []byte("aes-256-gcm"), nil
			case "keyformat":
				return []byte("hex"), nil
			case "keylocation":
				return []byte("prompt"), nil
			case "keystatus":
				if available {
					return []byte("available"), nil
				}
				return []byte("unavailable"), nil
			case "mounted":
				if mounted {
					return []byte("yes"), nil
				}
				return []byte("no"), nil
			}
		}
		if args[0] == "list" {
			return []byte("tank/private\ntank/private/containers/c\n"), nil
		}
		if args[0] == "unmount" {
			if busy {
				return nil, errors.New("busy")
			}
			mounted = false
		}
		if args[0] == "unload-key" {
			available = false
		}
		if args[0] == "load-key" {
			if string(input) != key+"\n" {
				t.Fatal("key not delivered over stdin")
			}
			available = true
		}
		return nil, nil
	}}
	{
		err := storage.Ready(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		err := storage.Lock(context.Background(), target)
		if err != nil || available || mounted {
			t.Fatal(err)
		}
	}
	{
		err := storage.Unlock(context.Background(), target, []byte(key))
		if err != nil || !available {
			t.Fatal(err)
		}
	}
	{
		err := storage.Unlock(context.Background(), target, []byte(strings.Repeat("cd", 32)))
		if err == nil {
			t.Fatal("wrong key accepted")
		}
	}
	mounted = true
	busy = true
	if storage.Lock(context.Background(), target) == nil || !available {
		t.Fatal("busy volume reported locked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if storage.Ready(ctx, target) == nil {
		t.Fatal("ignored cancellation")
	}
	if ValidDataset("tank") || ValidDataset("tank/../root") || ValidDataset("-x/root") {
		t.Fatal("unsafe dataset accepted")
	}
	if len(calls) == 0 {
		t.Fatal("no storage adapter calls")
	}
}

func TestContainmentWaitsForInFlightStart(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "guard"))
	release, err := m.beginStart()
	if err != nil {
		t.Fatal(err)
	}
	if m.Trigger("panel_request") != nil {
		t.Fatal("cannot trigger")
	}
	{
		_, err = m.beginStart()
		if err == nil {
			t.Fatal("start admitted after lock")
		}
	}
	called := make(chan struct{})
	m.SetStop(func(context.Context) error { close(called); return nil })
	done := make(chan struct{})
	go func() { m.Lockdown(context.Background()); close(done) }()
	select {
	case <-called:
		t.Fatal("containment ran before in-flight start drained")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("containment did not resume")
	}
}

func TestStorageHTTPEnrollmentUnlockAndRecovery(t *testing.T) {
	for _, action := range []string{"create", "register"} {
		t.Run(action, func(t *testing.T) {
			m := New(filepath.Join(t.TempDir(), "guard"))
			key := strings.Repeat("ab", 32)
			fp, _ := KeyFingerprint(key)
			available := true
			created := false
			m.storage = &Storage{Run: func(_ context.Context, _ string, args []string, input []byte) ([]byte, error) {
				switch args[0] {
				case "create":
					created = true
				case "get":
					switch args[4] {
					case "guid":
						return []byte("123"), nil
					case "encryptionroot":
						return []byte("tank/private"), nil
					case "encryption":
						return []byte("aes-256-gcm"), nil
					case "keyformat":
						return []byte("hex"), nil
					case "keylocation":
						return []byte("prompt"), nil
					case "mounted":
						return []byte("no"), nil
					case "keystatus":
						if available {
							return []byte("available"), nil
						}
						return []byte("unavailable"), nil
					}
				case "load-key":
					if string(input) != key+"\n" {
						return nil, errors.New("wrong key")
					}
					available = true
				case "list":
					return []byte("tank/private\n"), nil
				case "unload-key":
					available = false
				}
				return nil, nil
			}}
			body := `{"dataset":"tank/private","key":"` + key + `","fingerprint":"` + fp + `","stored":true}`
			code, _ := m.Handle(request("POST", "/"+action, body, false))
			if code != 200 || created != (action == "create") {
				t.Fatal("enrollment failed", code)
			}
			state, _ := m.Status()
			if len(state.Targets) != 1 || state.Targets[0].GUID != "123" {
				t.Fatal("dataset identity missing")
			}
			code, _ = m.Handle(request("POST", "/register", body, false))
			if code != 409 {
				t.Fatal("registration overwritten")
			}
			m.SetStop(func(context.Context) error { return nil })
			_ = m.Trigger("panel_request")
			m.Lockdown(context.Background())
			if available {
				t.Fatal("registered key not unloaded")
			}
			bad := `{"dataset":"tank/private","key":"` + strings.Repeat("cd", 32) + `"}`
			code, _ = m.Handle(request("POST", "/unlock", bad, false))
			if code != 409 {
				t.Fatal("accepted wrong key")
			}
			code, _ = m.Handle(request("POST", "/unlock", body, false))
			if code != 200 || !available || m.Check() == nil {
				t.Fatal("storage unlock cleared API lock", code)
			}
			state, _ = m.Status()
			recoverBody, _ := json.Marshal(map[string]uint64{"revision": state.Revision})
			code, _ = m.Handle(request("POST", "/recover", string(recoverBody), true))
			if code != 200 || m.Check() != nil {
				t.Fatal("remote recovery failed", code)
			}
			m.actions.Lock()
			code, _ = m.Handle(request("POST", "/unlock", body, false))
			m.actions.Unlock()
			if code != 409 {
				t.Fatal("storage actions overlapped containment")
			}
			_, err := m.Audit("list", false, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			events, _ := m.Events()
			ackBody, _ := json.Marshal(map[string]any{"ids": []string{events[0].ID}})
			code, _ = m.Handle(request("POST", "/ack", string(ackBody), false))
			if code != 200 {
				t.Fatal("ack failed", code)
			}
		})
	}
}

func TestKeyHandoffNeverUsesRequestBodyDebugCapture(t *testing.T) {
	for _, path := range []string{"/1.0/payincus/security", "/1.0/payincus/security/create", "/1.0/payincus/security/unlock", "/1.0/payincus/security/malformed"} {
		if !SensitivePath(path) {
			t.Fatal("key handoff could be logged", path)
		}
	}
	if SensitivePath("/1.0/instances") {
		t.Fatal("unrelated upstream logging changed")
	}
}

func TestRestartWithoutSafetyPreservesStoragePolicy(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "guard"))
	if m.transact(true, func(s *State) error {
		s.Targets = []Target{{Dataset: "pool/incus"}}
		s.StorageLocks = map[string]StorageLock{"pool/incus": {Locked: true, Complete: true}}
		return nil
	}) != nil || m.Boot() != nil {
		t.Fatal("fixture/restart failed")
	}
	state, err := m.Status()
	if err != nil || state.Locked || !state.StorageLocks["pool/incus"].Locked {
		t.Fatal("storage registration armed global safety or lost manual lock", state, err)
	}
	if m.Trigger("panel_request") != nil || m.Boot() != nil {
		t.Fatal("lock/restart failed")
	}
	state, _ = m.Status()
	if !state.Locked {
		t.Fatal("restart cleared an existing global lock")
	}
}
