package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// Locate menu entries by action: order and labels are presentation detail.
	cursorWhere := func(group, action string) int {
		for i, item := range (model{menuGroup: group}).currentMenu() {
			if item.action == action {
				return i
			}
		}
		return -1
	}
	credentials := cursorWhere("advanced", "credentials")
	if credentials < 0 {
		t.Fatal("advanced menu is missing the credentials action")
	}
	m := model{backend: b, menuGroup: "advanced", cursor: credentials}
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
	initialize := cursorWhere("", "initialize")
	if initialize < 0 {
		t.Fatal("main menu is missing the initialize action")
	}
	m = model{backend: b, cursor: initialize}
	updated, cmd = m.selectMenu()
	m = asModel(t, updated)
	if cmd != nil || m.page != "confirm" {
		t.Fatal("initialization bypassed confirmation")
	}
}

// PayIncus: the reviewed token form is the confirmation; Enter starts enrollment with no second y/n prompt.
func TestTokenSubmissionStartsEnrollmentWithoutSecondConfirmation(t *testing.T) {
	b := &backend{path: filepath.Join(t.TempDir(), "config.yaml"), runner: &fakeRunner{}}
	m := model{backend: b, page: "token", fields: []field{{value: "https://panel.example.com"}, {value: "ait_" + strings.Repeat("x", 43)}}}
	updated, cmd := m.submit()
	m = asModel(t, updated)
	if cmd == nil || !m.busy || m.page == "confirm" || m.confirm != nil {
		t.Fatal("token enrollment still required a second confirmation")
	}
	if m.cancel == nil {
		t.Fatal("token enrollment did not schedule a cancellable operation")
	}
	m.cancel()
}

func TestTokenSubmissionRejectsInvalidInput(t *testing.T) {
	for _, values := range [][]string{
		{"ftp://panel.example.com", "ait_" + strings.Repeat("x", 43)},
		{"https://panel.example.com", "ait_short"},
	} {
		m := model{backend: &backend{path: filepath.Join(t.TempDir(), "config.yaml"), runner: &fakeRunner{}}, page: "token",
			fields: []field{{value: values[0]}, {value: values[1]}}}
		updated, cmd := m.submit()
		m = asModel(t, updated)
		if cmd != nil || m.busy || m.confirm != nil || m.message == "" {
			t.Fatal("invalid token input was not rejected")
		}
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

func TestTerminalColorReplyCannotPollutePanelAddress(t *testing.T) {
	address := "http://100.75.147.95:3000"
	m := model{backend: &backend{runner: &fakeRunner{}}, page: "token", fields: []field{{label: "Panel", value: address}}}
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Alt: true, Runes: []rune{']'}},
		{Type: tea.KeyRunes, Runes: []rune("11;rgb:fefe/ffff/ffff")},
		{Type: tea.KeyRunes, Alt: true, Runes: []rune{'\\'}},
	} {
		updated, _ := m.Update(key)
		m = asModel(t, updated)
	}
	if m.fields[0].value != address || m.readingOSC {
		t.Fatal("terminal protocol response entered the form")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, updated)
	if m.page != "" {
		t.Fatal("ordinary Escape no longer returns to the menu")
	}
}
