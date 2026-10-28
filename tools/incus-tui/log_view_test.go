package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestLogViewBoundsAndNavigation(t *testing.T) {
	var disk strings.Builder
	l := &operationLog{writer: &disk, path: "/fixture/session.log"}
	ctx := context.WithValue(context.Background(), operationLogKey{}, l)
	for i := range 250 {
		logOperation(ctx, "line-%03d 中文 %s", i, strings.Repeat("x", 100))
	}
	entries, _ := l.tail()
	if len(entries) != 200 || !strings.Contains(disk.String(), "line-000") {
		t.Fatal("UI tail is unbounded or full disk log was lost")
	}
	for _, size := range [][2]int{{80, 24}, {40, 12}, {120, 40}} {
		m := model{backend: &backend{logs: l}, busy: true, width: size[0], height: size[1], status: "STATUS MUST NOT MIX WITH OUTPUT"}
		view := m.View()
		if len(strings.Split(view, "\n")) > size[1] || strings.Contains(view, m.status) || !strings.Contains(view, "操作控制") {
			t.Fatalf("operation view overflow or mixed status: %s", view)
		}
		for _, row := range strings.Split(view, "\n") {
			if ansi.StringWidth(row) > size[0] {
				t.Fatal("log row exceeds terminal width")
			}
		}
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		m = asModel(t, updated)
		if m.logOffset == 0 || !strings.Contains(m.View(), "历史浏览") {
			t.Fatal("log paging failed")
		}
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
		m = asModel(t, updated)
		if m.logOffset != 0 {
			t.Fatal("live follow failed")
		}
		updated, _ = m.Update(resultMsg{err: fmt.Errorf("fixture failure")})
		m = asModel(t, updated)
		if m.busy || !m.showLogs || !strings.Contains(m.View(), "fixture failure") {
			t.Fatal("operation result was not retained")
		}
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if asModel(t, updated).showLogs {
			t.Fatal("cannot return from logs to menu")
		}
	}
}
