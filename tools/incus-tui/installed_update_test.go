package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestInstalledUpdatePendingUsesRunningProcess(t *testing.T) {
	for _, target := range []string{"/usr/libexec/incus/incusd", "/usr/libexec/incus/incusd (deleted)"} {
		f := &fakeRunner{reply: func(c call) ([]byte, error) {
			if c.name == "systemctl" {
				return []byte("196\n"), nil
			}
			if c.name != "readlink" || !reflect.DeepEqual(c.args, []string{"/proc/196/exe"}) {
				t.Fatalf("unexpected process inspection: %+v", c)
			}
			return []byte(target + "\n"), nil
		}}
		pending, err := (&backend{runner: f}).installedUpdatePending(context.Background())
		if err != nil || pending != strings.HasSuffix(target, " (deleted)") {
			t.Fatal(pending, err)
		}
	}
	for _, pid := range []string{"0", "-1", "196/../../etc", "invalid"} {
		f := &fakeRunner{reply: func(call) ([]byte, error) { return []byte(pid), nil }}
		pending, err := (&backend{runner: f}).installedUpdatePending(context.Background())
		if pending || (err != nil) != (pid != "0") || len(f.calls) != 1 {
			t.Fatal("invalid or stopped PID reached /proc", pending, err, f.calls)
		}
	}
	cause := errors.New("readlink: permission denied")
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		if c.name == "systemctl" {
			return []byte("196"), nil
		}
		return nil, cause
	}}
	_, err := (&backend{runner: f}).installedUpdatePending(context.Background())
	if !errors.Is(err, cause) {
		t.Fatal("process inspection error hidden", err)
	}
}

func TestApplyInstalledUpdateChecksBeforeRestartAndDoesNotInstall(t *testing.T) {
	for _, states := range [][2]bool{{true, true}, {true, false}, {false, false}} {
		f := updateRunner(states[0], states[1], "", nil)
		err := updaterBackend(t, f).applyInstalledUpdate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		checked := false
		var restarted []string
		for _, c := range f.calls {
			if c.name == "/usr/libexec/incus/prepare-idmap" {
				checked = true
				if !reflect.DeepEqual(c.args, []string{"--check"}) {
					t.Fatal("apply mutated root mappings", c)
				}
			} else if c.name == "systemctl" && c.args[0] != "show" {
				if !checked || c.args[0] != "restart" {
					t.Fatal("restarted before validation or changed enablement", c)
				}
				restarted = append(restarted, c.args[1])
			} else if c.name != "systemctl" && c.name != "/usr/bin/incus" {
				t.Fatal("apply attempted installation or another mutation", c)
			}
		}
		want := []string(nil)
		if states[0] {
			want = append(want, "incus")
		}
		if states[1] {
			want = append(want, "incudal-agent")
		}
		if !checked || !reflect.DeepEqual(restarted, want) {
			t.Fatal(checked, restarted, want)
		}
	}
}

func TestStatusWarnsPendingInstalledUpdate(t *testing.T) {
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		if c.name == "systemctl" && strings.Contains(strings.Join(c.args, " "), "MainPID") {
			return []byte("196"), nil
		}
		if c.name == "readlink" {
			return []byte("/usr/libexec/incus/incusd (deleted)"), nil
		}
		if c.name == "/usr/bin/incus" && strings.Contains(strings.Join(c.args, " "), "/1.0/payincus") {
			return []byte(`{"api_extensions":["payincus_node_v1"]}`), nil
		}
		return []byte("[]"), nil
	}}
	status := (&backend{runner: f}).status(context.Background())
	if !strings.Contains(status, "节点 API: 可连接") || !strings.Contains(status, "待应用更新") || !strings.Contains(status, "应用已安装更新（无需下载）") {
		t.Fatal("a reachable old daemon was reported without restart guidance", status)
	}
}

func TestApplyInstalledUpdatePropagatesFailureAndSharesUpdateLock(t *testing.T) {
	for _, stage := range []string{"/usr/libexec/incus/prepare-idmap --check", "systemctl restart incus"} {
		cause := errors.New("fixture failure at " + stage)
		f := updateRunner(true, true, stage, cause)
		err := updaterBackend(t, f).applyInstalledUpdate(context.Background())
		if !errors.Is(err, cause) {
			t.Fatal("apply hid failure", err)
		}
		for _, c := range f.calls {
			if c.name == "systemctl" && reflect.DeepEqual(c.args, []string{"restart", "incudal-agent"}) {
				t.Fatal("continued after failed prerequisite")
			}
		}
	}
	b := updaterBackend(t, updateRunner(true, true, "", nil))
	lock, err := b.lockNodeUpdate()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if b.applyInstalledUpdate(context.Background()) == nil {
		t.Fatal("apply ran concurrently with package update")
	}
}

func TestApplyInstalledUpdateRequiresConfirmation(t *testing.T) {
	f := updateRunner(true, true, "", nil)
	m := model{backend: updaterBackend(t, f), menuGroup: "update", cursor: 2}
	updated, cmd := m.selectMenu()
	m = asModel(t, updated)
	if cmd != nil || len(f.calls) != 0 || m.page != "confirm" || m.updateChannel != "installed" {
		t.Fatal("apply executed before confirmation")
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = asModel(t, updated)
	if cmd == nil || !m.updating {
		t.Fatal("confirmed apply did not start")
	}
	result := cmd().(resultMsg)
	if result.err != nil || !result.restartTUI {
		t.Fatal("apply did not reopen installed TUI", result)
	}
}
