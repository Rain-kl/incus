package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type logTickMsg struct{}

func logTick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return logTickMsg{} })
}

func (l *operationLog) tail() ([]string, error) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.recent...), l.err
}

// PayIncus: status and command output have separate bounded regions, even during package updates.
func (m model) operationView(width int) string {
	height := m.height
	if height <= 0 {
		height = 24
	}
	header := "Incus 节点管理 · " + version + " | 操作日志"
	state := "操作已结束"
	if m.busy {
		state = "正在执行，请稍候… Esc 取消等待"
		if m.updating {
			state = "正在更新软件包并重启原运行服务，请勿关闭终端…"
		}
	} else if m.message != "" {
		state = m.message
	}
	var logs *operationLog
	if m.backend != nil {
		logs = m.backend.logs
	}
	entries, err := logs.tail()
	path := "未启用文件日志"
	if logs != nil && logs.path != "" {
		path = "完整操作日志：" + logs.path
	}
	if err != nil {
		path = "日志文件写入失败：" + err.Error()
	}
	rows := strings.Split(ansi.Hardwrap(strings.Join(entries, "\n"), width, false), "\n")
	available := max(1, height-7)
	offset := min(m.logOffset, max(0, len(rows)-available))
	end := len(rows) - offset
	start := max(0, end-available)
	mode := "实时跟随"
	if offset > 0 {
		mode = "历史浏览"
	}
	lines := []string{header, cleanText(state), path, fmt.Sprintf("── 命令输出 · %s · 最近 200 条（完整内容见文件） ──", mode)}
	lines = append(lines, rows[start:end]...)
	for len(lines) < height-3 {
		lines = append(lines, "")
	}
	lines = append(lines, "── 操作控制 ──", "PgUp/PgDn 翻页 · End 跟随最新")
	if m.busy {
		if m.updating {
			lines = append(lines, "更新期间不能取消，请等待完成")
		} else {
			lines = append(lines, "Esc 取消等待；已执行的变更不会自动回滚")
		}
	} else {
		lines = append(lines, "Enter / Esc / l 返回菜单 · 菜单按 l 重开日志")
	}
	for i := range lines {
		lines[i] = ansi.Truncate(cleanText(lines[i]), width, "…")
	}
	return strings.Join(lines[:min(len(lines), height)], "\n")
}
