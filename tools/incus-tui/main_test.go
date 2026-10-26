package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestUIRequiresConfirmationAndMasksSecrets(t *testing.T) {
	c := goodConnection()
	b := &backend{path: t.TempDir() + "/config.yaml", runner: &fakeRunner{}}
	{
		err := b.saveConnection(c)
		if err != nil {
			t.Fatal(err)
		}
	}
	m := model{backend: b, cursor: 1}
	updated, cmd := m.selectMenu()
	m = asModel(t, updated)
	if cmd != nil || strings.Contains(m.View(), c.Secret) {
		t.Fatal("secret displayed or action executed early")
	}
	updated, cmd = m.submit()
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" || m.confirm == nil {
		t.Fatal("missing confirmation")
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = asModel(t, updated)
	if cmd != nil || m.confirm != nil {
		t.Fatal("cancel executed action")
	}
	m = model{backend: b, cursor: 0}
	updated, cmd = m.selectMenu()
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" {
		t.Fatal("initialization bypassed confirmation")
	}
}

func TestUIServiceActionsAndFailure(t *testing.T) {
	m := model{backend: &backend{runner: &fakeRunner{}}, page: "incus"}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" || m.confirm == nil {
		t.Fatal("stop bypassed confirmation")
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = asModel(t, updated)
	if cmd == nil || !m.busy {
		t.Fatal("confirmed action not scheduled")
	}
	updated, _ = m.Update(resultMsg{err: errors.New("permission denied")})
	m = asModel(t, updated)
	if m.busy || !strings.Contains(m.View(), "permission denied") {
		t.Fatal("failure not visible")
	}
	if strings.Contains(cleanText("\x1b[31mhello\x07"), "\x1b") {
		t.Fatal("terminal control injection")
	}
}

func asModel(t *testing.T, value tea.Model) model {
	t.Helper()
	result, ok := value.(model)
	if !ok {
		t.Fatal("unexpected terminal model")
	}
	return result
}

func TestJournalAndOperationCancellation(t *testing.T) {
	for _, unit := range []string{"incus", "incudal-agent"} {
		cmd, err := journalCommand(unit)
		if err != nil || !reflect.DeepEqual(cmd.Args, []string{"journalctl", "-u", unit, "-n", "100", "-f", "--no-pager"}) {
			t.Fatal(cmd, err)
		}
	}
	{
		_, err := journalCommand("sshd")
		if err == nil {
			t.Fatal("unbounded journal access")
		}
	}
	m := model{backend: &backend{runner: &fakeRunner{}}, page: "confirm", confirm: func(ctx context.Context) error { return ctx.Err() }}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = asModel(t, updated)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, updated)
	result, ok := cmd().(resultMsg)
	if !ok || !errors.Is(result.err, context.Canceled) || !m.busy {
		t.Fatal("operation cancellation lost")
	}
}

func TestInstallerTokenPrefillIsMaskedAndRemovedFromEnvironment(t *testing.T) {
	t.Setenv("INCUDAL_PANEL_URL", "https://panel.example.com")
	t.Setenv("INCUDAL_AGENT_INSTALL_TOKEN", "ait_fixture_not_a_real_secret")
	m := initialModel(&backend{runner: &fakeRunner{}})
	if m.page != "token" || len(m.fields) != 2 || m.fields[0].value != "https://panel.example.com" || !m.fields[1].secret {
		t.Fatal("missing installer handoff")
	}
	if strings.Contains(m.View(), "ait_fixture_not_a_real_secret") || os.Getenv("INCUDAL_AGENT_INSTALL_TOKEN") != "" {
		t.Fatal("token was exposed or inherited")
	}
}
