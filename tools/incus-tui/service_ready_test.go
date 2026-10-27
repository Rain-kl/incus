package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestUIRestartUsesAPIReadinessBeforeCompleting(t *testing.T) {
	queries := 0
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		if c.name == "/usr/bin/incus" {
			queries++
		}
		return nil, nil
	}}
	m := model{backend: &backend{runner: f}, page: "incus"}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	m = asModel(t, updated)
	if cmd != nil || len(f.calls) != 0 {
		t.Fatal("restart executed before confirmation")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("confirmed restart not scheduled")
	}
	result := cmd().(resultMsg)
	if result.err != nil || queries != 1 || result.text != "操作完成" {
		t.Fatal("UI completed without checking the socket API", result, queries)
	}
}

func TestServiceManagementWaitsForAPIInsteadOfSystemctlExit(t *testing.T) {
	for _, action := range []string{"enable", "start", "restart"} {
		t.Run(action, func(t *testing.T) {
			queries := 0
			f := &fakeRunner{reply: func(c call) ([]byte, error) {
				if c.name == "/usr/bin/incus" {
					if !reflect.DeepEqual(c.args, []string{"query", "/1.0/payincus"}) {
						t.Fatal(c)
					}
					queries++
					if queries == 1 {
						return nil, errors.New("dial unix /var/lib/incus/unix.socket: no such file or directory")
					}
				}
				return nil, nil
			}}
			err := (&backend{runner: f}).manageService(context.Background(), "incus", action)
			if err != nil || queries != 2 || f.calls[0].name != "systemctl" {
				t.Fatal("service action did not wait for the socket API", err, queries)
			}
		})
	}
}

func TestServiceReadinessCancellationPreservesSocketFailure(t *testing.T) {
	cause := errors.New("dial unix: connection refused")
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		if c.name == "/usr/bin/incus" {
			return nil, cause
		}
		return nil, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := (&backend{runner: f}).manageService(ctx, "incus", "restart")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatal("API timeout lost its original error", err)
	}
	f = &fakeRunner{reply: func(call) ([]byte, error) { return nil, cause }}
	err = (&backend{runner: f}).manageService(context.Background(), "incus", "restart")
	if !errors.Is(err, cause) || len(f.calls) != 1 {
		t.Fatal("polled API after systemctl failed", err, f.calls)
	}
}

func TestServiceManagementDoesNotPollStoppedNodesOrConnector(t *testing.T) {
	for _, service := range [][2]string{{"incus", "stop"}, {"incus", "disable"}, {"incudal-agent", "restart"}} {
		f := &fakeRunner{}
		err := (&backend{runner: f}).manageService(context.Background(), service[0], service[1])
		if err != nil || len(f.calls) != 1 || f.calls[0].name != "systemctl" {
			t.Fatal("waited for an unrelated API", service, err, f.calls)
		}
	}
}
