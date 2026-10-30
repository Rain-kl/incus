package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func updaterBackend(t *testing.T, f *fakeRunner) *backend {
	t.Helper()
	return &backend{runner: f, updateLockPath: filepath.Join(t.TempDir(), "update.lock")}
}

func updateRunner(incusActive, connectorActive bool, fail string, cause error) *fakeRunner {
	return &fakeRunner{reply: func(c call) ([]byte, error) {
		args := strings.Join(c.args, " ")
		if c.name+" "+args == fail {
			return nil, cause
		}
		if c.name == "systemctl" && strings.Contains(args, "--property=ExecStart") {
			return []byte("/usr/libexec/incus/payincus-agent"), nil
		}
		if c.name == "systemctl" && len(c.args) > 1 && c.args[0] == "show" {
			active := incusActive
			if c.args[1] == "incudal-agent" {
				active = connectorActive
			}
			state := "inactive"
			if active {
				state = "active"
			}
			return []byte("ActiveState=" + state + "\nUnitFileState=disabled\n"), nil
		}
		if c.name == "/usr/bin/incus-tui" {
			return []byte("incus-tui fixture-new-version\n"), nil
		}
		return nil, nil
	}}
}

func TestNodeUpdateUsesPackagedInstallerAndRestartsOnlyActiveServices(t *testing.T) {
	for _, channel := range []string{"canary", "latest"} {
		for _, states := range [][2]bool{{true, true}, {true, false}, {false, false}} {
			t.Run(fmt.Sprintf("%s-%v", channel, states), func(t *testing.T) {
				f := updateRunner(states[0], states[1], "", nil)
				b := updaterBackend(t, f)
				err := b.updateNode(context.Background(), channel)
				if err != nil {
					t.Fatal(err)
				}
				installer := -1
				restarts := []string{}
				for index, call := range f.calls {
					if call.name == "/bin/bash" {
						installer = index
						if !reflect.DeepEqual(call.args, []string{nodeInstallerPath, channel, "--update"}) {
							t.Fatal("wrong installer or channel", call)
						}
					}
					if call.name == "systemctl" && call.args[0] != "show" {
						if installer < 0 || call.args[0] != "restart" {
							t.Fatal("service modified before installation, or enablement changed", call)
						}
						restarts = append(restarts, call.args[1])
					}
				}
				want := []string{}
				if states[0] {
					want = append(want, "incus")
				}
				if states[1] {
					want = append(want, "incudal-agent")
				}
				if installer < 0 || !reflect.DeepEqual(restarts, want) {
					t.Fatal("wrong service restart set", restarts, want)
				}
			})
		}
	}
}

func TestNodeUpdatePreservesFailureCauseAndStopsAtFailedStage(t *testing.T) {
	for _, fail := range []string{
		"systemctl show incus --property=ActiveState,UnitFileState --no-pager",
		"/bin/bash " + nodeInstallerPath + " canary --update",
		"/usr/bin/incus-tui --version", "systemctl restart incus", "systemctl restart incudal-agent",
	} {
		cause := errors.New("fixture apt/dpkg or systemd error: exit status 100")
		f := updateRunner(true, true, fail, cause)
		err := updaterBackend(t, f).updateNode(context.Background(), "canary")
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), cause.Error()) {
			t.Fatal("update error was hidden", err)
		}
		last := f.calls[len(f.calls)-1]
		if last.name+" "+strings.Join(last.args, " ") != fail {
			t.Fatal("update continued after failure", f.calls)
		}
	}
	f := updateRunner(false, true, "", nil)
	err := updaterBackend(t, f).updateNode(context.Background(), "canary")
	if err == nil || len(f.calls) != 2 {
		t.Fatal("inconsistent services caused implicit Incus startup", err)
	}
	f = updateRunner(false, false, "", nil)
	err = updaterBackend(t, f).updateNode(context.Background(), "other; sh -c bad")
	if err == nil || len(f.calls) != 0 {
		t.Fatal("invalid channel reached commands")
	}
}

func TestNodeUpdateReadinessFailureDoesNotRestartConnector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := updateRunner(true, true, "", nil)
	reply := f.reply
	f.reply = func(c call) ([]byte, error) {
		if c.name == "/usr/bin/incus" {
			cancel()
			return nil, errors.New("new API not ready")
		}
		return reply(c)
	}
	err := updaterBackend(t, f).updateNode(ctx, "latest")
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "new API not ready") {
		t.Fatal("readiness failure cause lost", err)
	}
	for _, c := range f.calls {
		if c.name == "systemctl" && strings.Join(c.args, " ") == "restart incudal-agent" {
			t.Fatal("restarted connector while Incus was not ready")
		}
	}
}

func TestUpdateMenuConfirmationAndPackageTransactionCannotBeCancelled(t *testing.T) {
	// Locate the release channel entry by action: menu order is presentation detail.
	cursorOf := func(action string) int {
		for i, item := range (model{menuGroup: "update"}).currentMenu() {
			if item.action == action {
				return i
			}
		}
		return -1
	}
	canary := cursorOf("update-canary")
	if canary < 0 {
		t.Fatal("update menu is missing the canary channel")
	}
	if cursorOf("update-latest") < 0 {
		t.Fatal("update menu is missing the stable channel")
	}
	m := model{backend: updaterBackend(t, updateRunner(false, false, "", nil)), menuGroup: "update", cursor: canary}
	updated, cmd := m.selectMenu()
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" || m.updateChannel != "canary" {
		t.Fatal("update started before confirmation")
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = asModel(t, updated)
	if cmd == nil || !m.busy || !m.updating || m.cancel != nil {
		t.Fatal("package transaction is cancellable or was not scheduled")
	}
	updated, interrupt := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = asModel(t, updated)
	if interrupt != nil || !m.busy || !m.updating {
		t.Fatal("Ctrl+C interrupted apt/dpkg")
	}
	msg, ok := cmd().(resultMsg)
	if !ok || msg.err != nil || !msg.restartTUI {
		t.Fatal("successful update did not request new TUI", msg)
	}
	updated, done := m.Update(msg)
	m = asModel(t, updated)
	if !m.relaunch || done == nil {
		t.Fatal("old TUI kept running after successful update")
	}
	_, quit := done().(tea.QuitMsg)
	if !quit {
		t.Fatal("old TUI did not exit")
	}
}

func TestFailedNodeUpdateDoesNotRelaunchOldInterface(t *testing.T) {
	cause := errors.New("SHA256 mismatch")
	b := updaterBackend(t, updateRunner(true, true, "/bin/bash "+nodeInstallerPath+" latest --update", cause))
	msg, ok := nodeUpdateCommand(b, "latest")().(resultMsg)
	if !ok || !errors.Is(msg.err, cause) || msg.restartTUI {
		t.Fatal("failed update requested relaunch", msg)
	}
	m := model{backend: b, busy: true, updating: true}
	updated, _ := m.Update(msg)
	m = asModel(t, updated)
	if m.relaunch || m.busy || m.updating || !strings.Contains(m.message, "SHA256 mismatch") {
		t.Fatal("failed update state or diagnostics lost")
	}
}

func TestNodeUpdateRejectsConcurrentUpdaterAndReleasesLockAfterFailure(t *testing.T) {
	f := updateRunner(false, false, "", nil)
	b := updaterBackend(t, f)
	file, err := os.OpenFile(b.updateLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	err = b.updateNode(context.Background(), "canary")
	if err == nil || len(f.calls) != 0 {
		t.Fatal("concurrent update reached external commands", err)
	}
	err = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("fixture installer failed")
	f.reply = updateRunner(false, false, "/bin/bash "+nodeInstallerPath+" canary --update", cause).reply
	err = b.updateNode(context.Background(), "canary")
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	f.reply = updateRunner(false, false, "", nil).reply
	err = b.updateNode(context.Background(), "canary")
	if err != nil {
		t.Fatal("failed updater retained its lock", err)
	}
}

func TestNodeUpdateRefusesLegacyConnectorAndUnexpectedStateChanges(t *testing.T) {
	for _, mode := range []string{"legacy", "enablement"} {
		f := updateRunner(true, true, "", nil)
		reply := f.reply
		installed := false
		f.reply = func(c call) ([]byte, error) {
			if c.name == "/bin/bash" {
				installed = true
			}
			if mode == "legacy" && strings.Contains(strings.Join(c.args, " "), "--property=ExecStart") {
				return []byte("/usr/local/bin/old-agent"), nil
			}
			data, err := reply(c)
			if mode == "enablement" && installed && c.name == "systemctl" && c.args[0] == "show" {
				data = []byte("ActiveState=active\nUnitFileState=enabled\n")
			}
			return data, err
		}
		err := updaterBackend(t, f).updateNode(context.Background(), "latest")
		if err == nil || (mode == "legacy" && installed) {
			t.Fatal("unsupported connector or state change was ignored", mode, err)
		}
	}
}
