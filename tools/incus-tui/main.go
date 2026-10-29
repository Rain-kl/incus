// incus-tui provides local setup and storage management using the node API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var version = "v2.0.0"

// PayIncus: the reviewed token form is the confirmation; enrollment starts on submit with no second y/n prompt.
const tokenEnrollNote = "使用一次性 token 接入所填面板：启用 Incus 并等待 API，兑换凭据、导入面板公钥证书、配置本地监听 127.0.0.1:8443，保存配置并启动连接器。token 将被消费，已有其他监听地址将拒绝覆盖。按 Enter 开始接入。"

type field struct {
	label, value string
	secret       bool
	readonly     bool
}
type resultMsg struct {
	text       string
	err        error
	restartTUI bool
	pool       *encryptedPoolReceipt
	report     []string
}
type (
	statusMsg string
	model     struct {
		backend               *backend
		cursor, focus         int
		page, message, status string
		menuGroup             string
		readingOSC            bool
		fields                []field
		busy                  bool
		confirm               func(context.Context) error
		cancel                context.CancelFunc
		width                 int
		height                int
		updateChannel         string
		updating              bool
		relaunch              bool
		showLogs              bool
		logOffset             int
		poolRequest           *encryptedPoolRequest
		poolReceipt           *encryptedPoolReceipt
		report                []string
		quiet                 bool
	}
)

type menuItem struct {
	label  string
	action string
}

var menu = []menuItem{
	{"初始化", "initialize"},
	{"接入", "token"},
	{"ZFS", "zfs"},
	{"Xray", "xray"},
	{"修复", "repair"},
	{"更新", "update"},
	{"更多", "advanced"},
	{"刷新", "refresh"},
	{"退出", "quit"},
}

var xrayMenu = []menuItem{
	{"查看节点与运行状态", "xray-inspect"},
	{"测试代理节点", "xray-test"},
	{"重启 Xray 服务", "xray-restart"},
	{"查看 Xray 日志", "xray-logs"},
	{"重新配置 Xray（重建运行环境）", "xray-reconfigure"},
	{"返回主菜单", "back"},
}

var advancedMenu = []menuItem{
	{"手动配置 Panel 连接（Agent ID / 密钥）", "credentials"},
	{"手动导入 Panel 客户端证书（mTLS）", "certificate"},
	{"检查 / 修复反向隧道监听 127.0.0.1:8443", "listener"},
	{"Incus 服务管理", "incus"},
	{"连接器服务管理", "incudal-agent"},
	{"查看 Incus 实时日志", "incus-journal"},
	{"查看连接器实时日志", "connector-journal"},
	{"返回主菜单", "back"},
}

var repairMenu = []menuItem{
	{"修复网桥", "repair-bridge"},
	{"返回主菜单", "back"},
}

var updateMenu = []menuItem{
	{"开发版（canary）", "update-canary"},
	{"稳定版（正式 Release）", "update-latest"},
	{"应用已安装更新（无需下载）", "apply-installed"},
	{"返回主菜单", "back"},
}

var zfsManagementMenu = []menuItem{
	{"安装并检查 ZFS 依赖", "zfs-prepare"},
	{"创建加密 ZFS 存储池", "zpool"},
	{"输入密钥解锁 ZFS", "zfs-unlock"},
	{"将 ZFS 数据集绑定到 Incus", "zfs-bind"},
	{"查看 ZFS 池状态", "zfs-list"},
	{"注册已有加密数据集", "zfs-register"},
	{"返回主菜单", "back"},
}

func (m model) currentMenu() []menuItem {
	switch m.menuGroup {
	case "xray":
		return xrayMenu
	case "advanced":
		return advancedMenu
	case "zfs":
		return zfsManagementMenu
	case "repair":
		return repairMenu
	case "update":
		return updateMenu
	default:
		return menu
	}
}

func (m *model) backToMenu() {
	if m.page == "" {
		previousGroup := m.menuGroup
		m.menuGroup = ""
		for i, item := range menu {
			if item.action == previousGroup {
				m.cursor = i
				break
			}
		}
	}
	m.page = ""
	m.poolRequest = nil
	m.poolReceipt = nil
	m.report = nil
	m.quiet = false
	m.message = ""
	m.fields = nil
	m.confirm = nil
	m.updateChannel = ""
}

func (m model) Init() tea.Cmd { return tea.Batch(m.refresh(false), logTick()) }
func (m model) refresh(showLogs bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		// PayIncus: automatic status checks update the summary without operation output.
		if showLogs {
			ctx = context.WithValue(ctx, operationLogKey{}, m.backend.logs)
		}
		return statusMsg(m.backend.status(ctx))
	}
}

func operation(ctx context.Context, cancel context.CancelFunc, fn func(context.Context) error) tea.Cmd {
	return func() tea.Msg {
		defer cancel()
		logOperation(ctx, "操作开始")
		err := fn(ctx)
		if err != nil {
			logOperation(ctx, "操作失败：%v", err)
		} else {
			logOperation(ctx, "操作完成")
		}
		return resultMsg{text: "操作完成", err: err}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case logTickMsg:
		return m, logTick()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case statusMsg:
		m.status = string(msg)
	case resultMsg:
		m.updating = false
		m.updateChannel = ""
		if msg.restartTUI {
			m.relaunch = true
			return m, tea.Quit
		}
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.busy = false
		m.confirm = nil
		m.fields = nil
		m.page = ""
		m.report = nil
		m.quiet = false
		m.showLogs = true
		m.logOffset = 0
		m.message = msg.text
		if msg.err != nil {
			m.message = "操作失败：" + m.backend.logs.redact(msg.err.Error())
		}
		// PayIncus: Xray maintenance shows a friendly report instead of the raw operation log.
		if msg.report != nil {
			m.report = msg.report
			m.message = ""
			m.page = "report"
			m.showLogs = false
		}
		if msg.pool != nil {
			m.poolReceipt = msg.pool
			m.page = "pool-receipt"
			m.showLogs = false
		}
		return m, m.refresh(false)
	case tea.KeyMsg:
		// PayIncus: delayed terminal color reports are protocol replies, never form input.
		if msg.Type == tea.KeyRunes && msg.Alt && string(msg.Runes) == "]" {
			m.readingOSC = true
			return m, nil
		}
		if m.readingOSC {
			if msg.Type == tea.KeyCtrlG || msg.Type == tea.KeyEsc || (msg.Type == tea.KeyRunes && msg.Alt && string(msg.Runes) == "\\") {
				m.readingOSC = false
			}
			return m, nil
		}
		if msg.String() == "ctrl+c" {
			if m.updating {
				m.message = "正在更新软件包或重启服务，请勿中断；完整进度和错误会持续输出。"
				return m, nil
			}
			if m.cancel != nil {
				m.cancel()
				m.message = "正在取消操作并保存日志…"
				return m, nil
			}
			return m, tea.Quit
		}
		if m.page == "pool-receipt" {
			if msg.String() == "enter" || msg.String() == "esc" {
				m.backToMenu()
			}
			return m, nil
		}
		if m.page == "report" {
			if msg.String() == "enter" || msg.String() == "esc" || msg.String() == "q" {
				m.backToMenu()
			}
			return m, nil
		}
		if m.busy || m.showLogs {
			switch msg.String() {
			case "pgup":
				m.logOffset += max(1, m.height/2)
				return m, nil
			case "pgdown":
				m.logOffset = max(0, m.logOffset-max(1, m.height/2))
				return m, nil
			case "end":
				m.logOffset = 0
				return m, nil
			}
			if !m.busy && (msg.String() == "esc" || msg.String() == "l" || msg.String() == "enter") {
				m.showLogs = false
				return m, nil
			}
		}
		if !m.busy && m.page == "" && len(m.fields) == 0 && msg.String() == "l" {
			m.showLogs = true
			m.logOffset = 0
			return m, nil
		}
		if m.busy {
			if msg.String() == "esc" && m.cancel != nil {
				m.cancel()
				m.message = "正在取消；已执行的服务或初始化变更不会自动回滚。"
			}
			return m, nil
		}
		if msg.String() == "esc" {
			m.backToMenu()
			return m, nil
		}
		if m.page == "confirm" {
			if msg.String() == "y" || msg.String() == "Y" {
				m.busy = true
				m.logOffset = 0
				if m.updateChannel != "" {
					m.updating = true
					return m, nodeUpdateCommand(m.backend, m.updateChannel)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
				ctx = context.WithValue(ctx, operationLogKey{}, m.backend.logs)
				m.cancel = cancel
				if m.poolRequest != nil {
					request := *m.poolRequest
					m.poolRequest = nil
					return m, encryptedPoolOperation(ctx, cancel, m.backend, request)
				}
				return m, operation(ctx, cancel, m.confirm)
			}
			if msg.String() == "n" {
				m.page = ""
				m.message = ""
				m.confirm = nil
				m.poolRequest = nil
			}
			return m, nil
		}
		if len(m.fields) > 0 {
			if m.fields[m.focus].readonly && msg.Type != tea.KeyTab && msg.Type != tea.KeyDown && msg.Type != tea.KeyShiftTab && msg.Type != tea.KeyUp && msg.Type != tea.KeyEnter {
				return m, nil
			}
			switch msg.Type {
			case tea.KeyTab, tea.KeyDown:
				m.focus = (m.focus + 1) % len(m.fields)
			case tea.KeyShiftTab, tea.KeyUp:
				m.focus = (m.focus + len(m.fields) - 1) % len(m.fields)
			case tea.KeyBackspace, tea.KeyCtrlH:
				runes := []rune(m.fields[m.focus].value)
				if len(runes) > 0 {
					m.fields[m.focus].value = string(runes[:len(runes)-1])
				}
			case tea.KeyCtrlU:
				m.fields[m.focus].value = ""
			case tea.KeyRunes:
				if len(m.fields[m.focus].value)+len(string(msg.Runes)) <= 4096 {
					for _, r := range msg.Runes {
						if r >= 32 && r != 127 {
							m.fields[m.focus].value += string(r)
						}
					}
				}
			case tea.KeySpace:
				m.fields[m.focus].value += " "
			case tea.KeyEnter:
				return m.submit()
			}
			return m, nil
		}
		if m.page == "incus" || m.page == "incudal-agent" {
			actions := map[string]string{"1": "enable", "2": "start", "3": "stop", "4": "restart", "5": "disable"}
			action, ok := actions[msg.String()]
			if ok {
				unit := m.page
				m.page = "confirm"
				m.message = "确认对 " + unit + " 执行 " + action + "？停止或重启可能中断控制连接或实例管理。"
				m.confirm = func(ctx context.Context) error { return m.backend.manageService(ctx, unit, action) }
			}
			return m, nil
		}
		switch msg.String() {
		case "q":
			if m.menuGroup != "" {
				m.backToMenu()
				return m, nil
			}
			return m, tea.Quit
		case "up", "k":
			m.cursor = (m.cursor + len(m.currentMenu()) - 1) % len(m.currentMenu())
		case "down", "j":
			m.cursor = (m.cursor + 1) % len(m.currentMenu())
		case "enter":
			return m.selectMenu()
		}
	}
	return m, nil
}

func (m model) selectMenu() (tea.Model, tea.Cmd) {
	m.message = ""
	m.focus = 0
	items := m.currentMenu()
	if m.cursor < 0 || m.cursor >= len(items) {
		return m, nil
	}
	action := items[m.cursor].action
	m.updateChannel = ""
	if strings.HasPrefix(action, "zfs-") || action == "zpool" {
		return m.zfsMenu(action)
	}
	switch action {
	case "xray-inspect", "xray-test", "xray-restart", "xray-reconfigure":
		// PayIncus: Xray actions run directly and report a friendly summary; no per-action confirmation.
		action = strings.TrimPrefix(action, "xray-")
		m.busy = true
		m.quiet = true
		m.logOffset = 0
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		ctx = context.WithValue(ctx, operationLogKey{}, m.backend.logs)
		m.cancel = cancel
		return m, xrayOperation(ctx, cancel, m.backend, action)
	case "xray-logs":
		return m, tea.ExecProcess(exec.Command("journalctl", "-u", "incudal-xray.service", "-u", "incudal-egress-*.service", "-n", "100", "-f", "--no-pager"), func(err error) tea.Msg { return resultMsg{text: "Xray 日志查看已结束", err: err} })
	case "xray", "zfs", "advanced", "repair", "update":
		m.menuGroup = action
		m.cursor = 0
	case "back":
		m.backToMenu()
	case "initialize":
		m.page = "confirm"
		m.message = "启用 Incus，创建 default(dir)、incusbr0(10.10.0.1/22 NAT，禁用 IPv6)，补齐 default profile。匹配的基础资源可复用，已有实例或其它配置时拒绝覆盖。"
		m.confirm = m.backend.initialize
	case "repair-bridge":
		m.page = "confirm"
		m.message = "请先在面板开启节点维护，暂停创建实例。修复 incusbr0：备份配置，停止受影响的持久 LXC，删除旧网桥并重建 10.10.0.1/22 NAT 网络，恢复网卡及原运行状态。保留有效 IP，仅重新分配缺失/冲突地址；会中断网络。集群、VM 占用或运行中的临时容器将拒绝自动修复。"
		m.confirm = m.backend.repairBridge
	case "update-canary", "update-latest":
		m.updateChannel = strings.TrimPrefix(action, "update-")
		m.page = "confirm"
		m.message = "更新至" + updateChannelName(m.updateChannel) + "的最新 GitHub Release：下载并校验完整节点软件包，安装后自动重启原本运行的 Incus 和连接器，保持原开机启用状态，最后重新打开新版 TUI。渠道切换可能安装版本号更低的包。Incus 重启会执行节点 shutdown，可能停止实例；重新启动由已有启动策略和安全锁定状态决定。请安排维护窗口；更新开始后不能在界面中取消 apt/dpkg。"
	case "apply-installed":
		m.updateChannel = "installed"
		m.page = "confirm"
		m.message = "应用已安装的软件包：只读检查 root UID/GID 映射，重启原本运行的 Incus 和连接器，使新版二进制及映射生效；保持开机启用状态，不下载、不安装、不改映射。Incus shutdown 可能停止实例，后续启动按已有策略及安全锁决定。请安排维护窗口。"
	case "credentials":
		c := m.backend.loadConnection()
		m.page = "credentials"
		m.fields = []field{{label: "Panel 根地址", value: c.Panel}, {label: "Agent ID", value: c.ID}, {label: "Agent 密钥（保留原值可直接确认）", value: c.Secret, secret: true}}
	case "token":
		m.page = "token"
		m.message = tokenEnrollNote
		m.fields = []field{{label: "Panel 根地址", value: m.backend.loadConnection().Panel}, {label: "一次性安装 token（ait_，只使用一次）", secret: true}}
	case "certificate":
		m.page = "certificate"
		m.fields = []field{{label: "Panel 客户端 PEM 公钥证书的本地绝对路径"}}
	case "listener":
		m.page = "confirm"
		m.message = "启用本地 HTTPS 监听 127.0.0.1:8443，供反向隧道连接。已有其他监听地址时拒绝覆盖。仍需导入 Panel 证书，并在控制端启用隧道。"
		m.confirm = m.backend.listener
	case "incus":
		m.page = "incus"
	case "incudal-agent":
		m.page = "incudal-agent"
	case "incus-journal", "connector-journal":
		unit := "incus"
		if action == "connector-journal" {
			unit = "incudal-agent"
		}
		cmd, err := journalCommand(unit)
		if err != nil {
			m.message = err.Error()
			return m, nil
		}
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return resultMsg{text: "日志查看已结束", err: err} })
	case "refresh":
		return m, m.refresh(true)
	case "quit":
		return m, tea.Quit
	}
	return m, nil
}

func journalCommand(unit string) (*exec.Cmd, error) {
	if unit != "incus" && unit != "incudal-agent" {
		return nil, fmt.Errorf("unknown journal service")
	}
	return exec.Command("journalctl", "-u", unit, "-n", "100", "-f", "--no-pager"), nil
}

func (m model) submit() (tea.Model, tea.Cmd) {
	values := make([]string, len(m.fields))
	for i, f := range m.fields {
		values[i] = strings.TrimSpace(f.value)
	}
	if strings.HasPrefix(m.page, "zfs-") || m.page == "zpool" {
		return m.submitZFS(values)
	}
	switch m.page {
	case "credentials":
		c := connection{Panel: values[0], ID: values[1], Secret: values[2]}
		err := c.validate()
		if err != nil {
			m.message = err.Error()
			return m, nil
		}
		m.confirm = func(ctx context.Context) error {
			return m.backend.connect(ctx, c)
		}
		m.message = "保存并替换 Panel 配置（0600），启用并重启连接 Agent。节点在线后仍需验证 mTLS 实例管理连接。"
	case "token":
		_, err := panelURL(values[0])
		if err != nil {
			m.message = err.Error()
			return m, nil
		}
		if !tokenPattern.MatchString(values[1]) {
			m.message = "无效的 ait_ 安装 token"
			return m, nil
		}
		// PayIncus: submitting the reviewed token form is the confirmation; enrollment starts immediately.
		panel, token := values[0], values[1]
		m.fields = nil
		m.busy = true
		m.logOffset = 0
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		ctx = context.WithValue(ctx, operationLogKey{}, m.backend.logs)
		m.cancel = cancel
		return m, operation(ctx, cancel, func(ctx context.Context) error { return m.backend.enroll(ctx, panel, token) })
	case "certificate":
		if !strings.HasPrefix(values[0], "/") {
			m.message = "请输入绝对路径"
			return m, nil
		}
		data, fingerprint, err := readCertificate(values[0])
		if err != nil {
			m.message = err.Error()
			return m, nil
		}
		m.confirm = func(ctx context.Context) error { return m.backend.trust(ctx, data, fingerprint) }
		m.message = "信任此 Panel 客户端证书，授予节点管理权限。请核对 SHA256 指纹：\n" + fingerprint + "\n保留现有信任，不删除旧证书。"
	}
	m.fields = nil
	m.page = "confirm"
	return m, nil
}

func finalCarriageReturnSegment(line string) string {
	if !strings.ContainsRune(line, '\r') {
		return line
	}

	parts := strings.Split(line, "\r")
	for i := len(parts) - 1; i >= 0; i-- {
		segment := parts[i]
		trimmed := strings.TrimSpace(ansi.Strip(segment))
		if trimmed != "" {
			return segment
		}
	}

	return ""
}

func cleanText(value string) string {
	// PayIncus: strip the whole terminal sequence before removing individual control characters.
	value = ansi.Strip(value)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsRune(value, '\r') {
		lines := strings.Split(value, "\n")
		for i, line := range lines {
			if strings.ContainsRune(line, '\r') {
				lines[i] = finalCarriageReturnSegment(line)
			}
		}
		value = strings.Join(lines, "\n")
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || (r >= 32 && r != 127 && (r < 0x80 || r > 0x9f)) {
			return r
		}
		return -1
	}, value)
}

func (m model) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	if m.page == "pool-receipt" && m.poolReceipt != nil {
		return ansi.Hardwrap(m.poolReceipt.view(), width, false)
	}
	// PayIncus: Xray results render as a friendly report page, not the operation log.
	if m.page == "report" {
		return ansi.Hardwrap(m.reportView(), width, false)
	}
	if m.busy || m.showLogs {
		// PayIncus: Xray maintenance keeps the operation log off screen while it runs.
		if m.busy && m.quiet {
			return ansi.Hardwrap(m.progressView(), width, false)
		}
		return m.operationView(width)
	}
	return ansi.Hardwrap(m.render(), width, false)
}

func (m model) progressView() string {
	var out strings.Builder
	out.WriteString("Incus 节点管理 · " + version + "\n\n")
	out.WriteString(cleanText(m.status) + "\n\n")
	if m.message != "" {
		out.WriteString(cleanText(m.message) + "\n\n")
	}
	out.WriteString("正在执行 Xray 操作，请稍候… Esc 取消等待\n")
	return out.String()
}

func (m model) reportView() string {
	var out strings.Builder
	out.WriteString("Incus 节点管理 · " + version + "\n\n")
	for _, line := range m.report {
		out.WriteString(cleanText(line) + "\n")
	}
	out.WriteString("\nEnter / Esc / q 返回菜单\n")
	return out.String()
}

func (m model) render() string {
	var out strings.Builder
	out.WriteString("Incus 节点管理 · " + version + "\n\n")
	if m.backend != nil && m.backend.logs != nil && m.backend.logs.path != "" {
		out.WriteString("完整操作日志：" + m.backend.logs.path + "\n\n")
	}
	out.WriteString(cleanText(m.status) + "\n\n")
	if m.message != "" {
		out.WriteString(cleanText(m.message) + "\n\n")
	}
	if m.busy {
		if m.updating {
			out.WriteString("正在更新软件包并重启原运行服务，请勿关闭终端…\n")
			return out.String()
		}
		out.WriteString("正在执行，请稍候… Esc 取消等待\n")
		return out.String()
	}
	switch {
	case m.page == "confirm":
		out.WriteString("按 y 确认，n / Esc 返回\n")
	case len(m.fields) > 0:
		for i, f := range m.fields {
			prefix := "  "
			if m.focus == i {
				prefix = "> "
			}
			value := f.value
			if f.secret && value != "" {
				value = "********"
			}
			out.WriteString(prefix + f.label + "：" + cleanText(value) + "\n")
		}
		out.WriteString("\nTab 切换 · Ctrl+U 清空 · Enter 提交 · Esc 返回\n")
	case m.page == "incus" || m.page == "incudal-agent":
		out.WriteString(m.page + " 服务：\n1. 启用并启动\n2. 启动\n3. 停止\n4. 重启\n5. 取消开机启动\n\nEsc 返回\n")
	default:
		visible := m.height - 12
		if visible < 6 {
			visible = 6
		}
		first := 0
		if m.cursor >= visible {
			first = m.cursor - visible + 1
		}
		items := m.currentMenu()
		if m.menuGroup == "advanced" {
			out.WriteString("高级维护\n\n")
		} else if m.menuGroup == "xray" {
			out.WriteString("Xray 管理\n\n")
		} else if m.menuGroup == "zfs" {
			out.WriteString("ZFS 管理\n\n")
		} else if m.menuGroup == "repair" {
			out.WriteString("修复\n\n")
		} else if m.menuGroup == "update" {
			out.WriteString("更新 Incus 节点软件包\n\n")
		}
		last := min(first+visible, len(items))
		for i := first; i < last; i++ {
			label := items[i].label
			prefix := "  "
			if i == m.cursor {
				prefix = "> "
			}
			out.WriteString(prefix + label + "\n")
		}
		if m.menuGroup == "" {
			out.WriteString("\n↑/↓ 选择 · Enter 确认 · l 操作日志 · q 退出\n")
		} else {
			out.WriteString("\n↑/↓ 选择 · Enter 确认 · l 操作日志 · Esc / q 返回主菜单（菜单可滚动）\n")
		}
	}
	return out.String()
}

func initialModel(b *backend) model {
	m := model{backend: b, status: "正在读取服务状态…"}
	panel, token := os.Getenv("INCUDAL_PANEL_URL"), os.Getenv("INCUDAL_AGENT_INSTALL_TOKEN")
	_ = os.Unsetenv("INCUDAL_AGENT_INSTALL_TOKEN")
	if panel != "" && token != "" {
		_ = os.Unsetenv("INCUDAL_PANEL_URL")
		m.page = "token"
		m.message = tokenEnrollNote + "节点存储初始化可在主菜单完成。"
		m.fields = []field{{label: "Panel 根地址", value: panel}, {label: "一次性安装 token（ait_，只使用一次）", value: token, secret: true}}
	}
	return m
}

func main() {
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println("incus-tui " + version)
		return
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "请使用 sudo incus-tui")
		os.Exit(1)
	}
	logDir := "/var/log/incus-tui"
	err := os.MkdirAll(logDir, 0o700)
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法创建日志目录：", err)
		os.Exit(1)
	}
	logFile, err := os.CreateTemp(logDir, "session-"+time.Now().Format("20060102-150405")+"-*.log")
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法创建日志文件：", err)
		os.Exit(1)
	}
	logClosed := false
	defer func() {
		if logClosed {
			return
		}
		err := logFile.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "关闭日志文件失败：", err)
		}
	}()
	b := newBackend()
	b.logs = &operationLog{writer: logFile, path: filepath.Clean(logFile.Name())}
	// PayIncus: render diagnostics inside the alternate screen, never above the menu.
	program := tea.NewProgram(initialModel(b), tea.WithAltScreen())
	finalModel, err := program.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法启动终端界面：", err)
		os.Exit(1)
	}
	result, ok := finalModel.(model)
	if ok && result.relaunch {
		err = logFile.Close()
		logClosed = true
		if err == nil {
			err = syscall.Exec("/usr/bin/incus-tui", []string{"incus-tui"}, os.Environ())
		}
		fmt.Fprintln(os.Stderr, "更新完成，但重新打开新版 TUI 失败：", err)
		os.Exit(1)
	}
}
