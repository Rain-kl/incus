// incus-tui provides local installation setup without changing upstream daemon code.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var version = "development"

type field struct {
	label, value string
	secret       bool
}
type resultMsg struct {
	text string
	err  error
}
type (
	statusMsg string
	model     struct {
		backend               *backend
		cursor, focus         int
		page, message, status string
		fields                []field
		busy                  bool
		confirm               func(context.Context) error
		cancel                context.CancelFunc
		width                 int
	}
)

var menu = []string{
	"一键初始化 Incus（dir 存储 + NAT 网桥）",
	"配置 Panel 连接（Agent ID / 密钥）",
	"使用 Panel 一次性安装 token 配置连接",
	"导入 Panel 客户端证书（mTLS）",
	"设置反向隧道本地监听 127.0.0.1:8443",
	"Incus 服务管理",
	"连接 Agent 服务管理",
	"查看 Incus 实时日志",
	"查看连接 Agent 实时日志",
	"刷新状态",
	"退出",
}

func (m model) Init() tea.Cmd { return m.refresh() }
func (m model) refresh() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return statusMsg(m.backend.status(ctx))
	}
}

func operation(ctx context.Context, cancel context.CancelFunc, fn func(context.Context) error) tea.Cmd {
	return func() tea.Msg {
		defer cancel()
		err := fn(ctx)
		return resultMsg{text: "操作完成", err: err}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case statusMsg:
		m.status = string(msg)
	case resultMsg:
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.busy = false
		m.confirm = nil
		m.fields = nil
		m.page = ""
		m.message = msg.text
		if msg.err != nil {
			m.message = "操作失败：" + msg.err.Error()
		}
		return m, m.refresh()
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		if m.busy {
			if msg.String() == "esc" && m.cancel != nil {
				m.cancel()
				m.message = "正在取消；已执行的服务或初始化变更不会自动回滚。"
			}
			return m, nil
		}
		if msg.String() == "esc" {
			m.page = ""
			m.message = ""
			m.fields = nil
			m.confirm = nil
			return m, nil
		}
		if m.page == "confirm" {
			if msg.String() == "y" || msg.String() == "Y" {
				m.busy = true
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				m.cancel = cancel
				return m, operation(ctx, cancel, m.confirm)
			}
			if msg.String() == "n" {
				m.page = ""
				m.message = ""
				m.confirm = nil
			}
			return m, nil
		}
		if len(m.fields) > 0 {
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
				m.confirm = func(ctx context.Context) error { return m.backend.service(ctx, unit, action) }
			}
			return m, nil
		}
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "up", "k":
			m.cursor = (m.cursor + len(menu) - 1) % len(menu)
		case "down", "j":
			m.cursor = (m.cursor + 1) % len(menu)
		case "enter":
			return m.selectMenu()
		}
	}
	return m, nil
}

func (m model) selectMenu() (tea.Model, tea.Cmd) {
	m.message = ""
	m.focus = 0
	switch m.cursor {
	case 0:
		m.page = "confirm"
		m.message = "为全新节点启用 Incus，创建 default(dir)、incusbr0(IPv4 NAT，禁用 IPv6)，配置 default profile。已有资源将拒绝自动初始化。"
		m.confirm = m.backend.initialize
	case 1:
		c := m.backend.loadConnection()
		m.page = "credentials"
		m.fields = []field{{label: "Panel 根地址", value: c.Panel}, {label: "Agent ID", value: c.ID}, {label: "Agent 密钥（保留原值可直接确认）", value: c.Secret, secret: true}}
	case 2:
		m.page = "token"
		m.fields = []field{{label: "Panel 根地址", value: m.backend.loadConnection().Panel}, {label: "一次性安装 token（ait_，只使用一次）", secret: true}}
	case 3:
		m.page = "certificate"
		m.fields = []field{{label: "Panel 客户端 PEM 公钥证书的本地绝对路径"}}
	case 4:
		m.page = "confirm"
		m.message = "启用本地 HTTPS 监听 127.0.0.1:8443，供反向隧道连接。已有其他监听地址时拒绝覆盖。仍需导入 Panel 证书，并在控制端启用隧道。"
		m.confirm = m.backend.listener
	case 5:
		m.page = "incus"
	case 6:
		m.page = "incudal-agent"
	case 7, 8:
		unit := "incus"
		if m.cursor == 8 {
			unit = "incudal-agent"
		}
		cmd, err := journalCommand(unit)
		if err != nil {
			m.message = err.Error()
			return m, nil
		}
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return resultMsg{text: "日志查看已结束", err: err} })
	case 9:
		return m, m.refresh()
	case 10:
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
		m.confirm = func(ctx context.Context) error {
			err := m.backend.connectorReady(ctx)
			if err != nil {
				return err
			}
			c, err := m.backend.exchange(ctx, values[0], values[1])
			if err != nil {
				return err
			}
			err = m.backend.saveConnection(c)
			if err != nil {
				return errorsAfterToken(err)
			}
			err = m.backend.service(ctx, "incudal-agent", "enable")
			if err != nil {
				return err
			}
			return m.backend.service(ctx, "incudal-agent", "restart")
		}
		m.message = "兑换一次性 token，保存新凭据并重启连接 Agent。token 将被消费，不执行控制端的旧安装脚本。"
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

func errorsAfterToken(err error) error {
	return fmt.Errorf("token 已消费，但配置保存失败；请在控制端重新生成 token：%w", err)
}

func cleanText(value string) string {
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
	return ansi.Hardwrap(m.render(), width, false)
}

func (m model) render() string {
	var out strings.Builder
	out.WriteString("Incus 节点管理 · " + version + "\n\n")
	out.WriteString(cleanText(m.status) + "\n\n")
	if m.message != "" {
		out.WriteString(cleanText(m.message) + "\n\n")
	}
	if m.busy {
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
		for i, label := range menu {
			prefix := "  "
			if i == m.cursor {
				prefix = "> "
			}
			out.WriteString(prefix + label + "\n")
		}
		out.WriteString("\n↑/↓ 选择 · Enter 确认 · q 退出\n")
	}
	return out.String()
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
	m := model{backend: newBackend(), status: "正在读取服务状态…"}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法启动终端界面：", err)
		os.Exit(1)
	}
}
