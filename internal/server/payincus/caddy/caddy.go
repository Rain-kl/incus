package caddy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// 面板<->Agent 已有 HMAC/隧道鉴权；Caddy Admin 只绑定宿主机回环 localhost，
// 无管理凭据、无证书。Agent 安装时也不从面板拉取脚本（消除脚本注入面），
// Caddyfile 内容由本包内联生成。

// Constants define protocol identifiers and timing limits.
const (
	caddyAdminPort = 2019
	caddyConfigDir = "/etc/caddy"
	caddyfilePath  = "/etc/caddy/Caddyfile"
)

// Status 描述本机 Caddy 可用状态（随心跳上报）。
type Status struct {
	Available bool   `json:"available"`
	Port      int    `json:"port"`
	Version   string `json:"version"`
	Active    bool   `json:"active"`
}

// Detect 快速探测：caddy 二进制存在且 systemd 服务 active 即视为可用。
func Detect() Status {
	status := Status{Available: false, Port: caddyAdminPort}
	{
		_, err := exec.LookPath("caddy")
		if err != nil {
			return status
		}
	}

	version, err := exec.Command("caddy", "version").Output()
	if err == nil {
		status.Version = strings.TrimSpace(string(version))
	}

	status.Active = serviceActive("caddy")
	status.Available = status.Active
	return status
}

func serviceActive(unit string) bool {
	err := exec.Command("systemctl", "is-active", "--quiet", unit).Run()
	return err == nil
}

// Install 在宿主机本地部署 Caddy（仅回环 admin，无公网监听）。
// 幂等：已安装且服务正常时直接返回 success。
func Install(ctx context.Context) error {
	// 1. 安装/更新 caddy 二进制（Ubuntu/Debian apt）。
	{
		err := installPackage(ctx)
		if err != nil {
			return err
		}
	}

	// 2. 写最小 Caddyfile：只暴露回环 Admin API，不监听任何公网端口。
	{
		err := writeCaddyfile()
		if err != nil {
			return err
		}
	}

	// 3. systemd enable + start。
	{
		err := ensureService(ctx)
		if err != nil {
			return err
		}
	}

	// 4. 探活 127.0.0.1:2019。
	{
		err := waitAdminReady(ctx, caddyAdminPort)
		if err != nil {
			return err
		}
	}

	log.Printf("[caddy] installed and ready on 127.0.0.1:%d", caddyAdminPort)
	return nil
}

// Uninstall 从宿主机彻底移除 Caddy：
// 停止并禁用服务、删除 systemd unit、Caddyfile、二进制与 apt 源。
// 卸载后下个心跳自然上报 available=false，面板据此把 caddy_enabled 置回 false。
func Uninstall(ctx context.Context) error {
	// 1. 停止并禁用服务（unit 不存在时 systemctl 会失败，忽略）。
	_ = exec.CommandContext(ctx, "systemctl", "stop", "caddy").Run()
	_ = exec.CommandContext(ctx, "systemctl", "disable", "caddy").Run()

	// 2. 删除 systemd unit 与 socket（若有）并 reload。
	_ = os.Remove("/etc/systemd/system/caddy.service")
	_ = os.Remove("/etc/systemd/system/caddy.socket")
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()

	// 3. 删除配置与数据。
	_ = os.RemoveAll(caddyConfigDir)
	_ = os.RemoveAll("/var/lib/caddy")
	_ = os.RemoveAll("/var/log/caddy")

	// 4. 删除管理二进制。
	_ = os.Remove("/usr/bin/caddy")
	_ = os.Remove("/usr/local/bin/caddy")

	// 5. 删除 APT 仓库源与 keyring（若 apt 安装过）。
	_ = os.Remove("/etc/apt/sources.list.d/caddy-stable.list")
	_ = os.Remove("/usr/share/keyrings/caddy-stable-archive-keyring.gpg")

	log.Printf("[caddy] uninstalled: binary, unit, config and apt source removed")
	return nil
}

func installPackage(ctx context.Context) error {
	{
		_, err := exec.LookPath("caddy")
		if err == nil {
			return nil // 已装
		}
	}

	// 动态识别发行版 codename，避免硬编码 bullseye 导致非 Debian 11 装不上。
	codename := detectDebianCodename()

	{
		err := runStep(ctx, "apt-get", "update", "-qq")
		if err != nil {
			return err
		}
	}
	{
		err := runStep(ctx, "apt-get", "install", "-y", "-qq", "curl", "debian-keyring", "debian-archive-keyring", "apt-transport-https", "openssl", "gpg")
		if err != nil {
			return err
		}
	}

	// Caddy 官方源：先下载 ASCII armored 公钥，再 dearmor 为二进制 keyring，
	// 否则 apt 报 "unsupported filetype"/NO_PUBKEY 拒绝该仓库。
	keyring := "/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
	{
		err := runStep(ctx, "bash", "-c",
			"curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o "+keyring)
		if err != nil {
			return err
		}
	}
	{
		err := runStep(ctx, "bash", "-c",
			"echo 'deb [signed-by="+keyring+"] https://dl.cloudsmith.io/public/caddy/stable/deb/debian/ "+codename+" main' > /etc/apt/sources.list.d/caddy-stable.list")
		if err != nil {
			return err
		}
	}
	{
		err := runStep(ctx, "apt-get", "update", "-qq")
		if err != nil {
			return err
		}
	}
	{
		err := runStep(ctx, "apt-get", "install", "-y", "-qq", "caddy")
		if err != nil {
			// 新发行版（如 Ubuntu 26.04）cloudsmith 可能还没有对应 codename，
			// apt 源不可用时回退官方静态二进制，保证 Caddy 仍可安装。
			log.Printf("[caddy] apt install failed, falling back to official static binary: %v", err)
			{
				fallbackErr := installStaticBinary(ctx)
				if fallbackErr != nil {
					return fmt.Errorf("caddy apt install failed and static binary fallback also failed: %w (apt: %v)", fallbackErr, err)
				}
			}
		}
	}

	return nil
}

// installStaticBinary 下载 Caddy 官方静态二进制到 /usr/bin/caddy。
func installStaticBinary(ctx context.Context) error {
	arch := runtime.GOARCH
	// Go 的 amd64/arm64 映射到 Caddy 下载参数。
	switch arch {
	case "amd64", "arm64":
	default:
		return fmt.Errorf("unsupported arch for static caddy: %s", arch)
	}

	url := "https://caddyserver.com/api/download?os=linux&arch=" + arch
	tmp := "/tmp/caddy-download"
	cmd := exec.CommandContext(ctx, "curl", "-fsSL", "-o", tmp, url)
	{
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("download caddy static binary: %w (output: %s)", err, truncate(string(output), 512))
		}
	}
	{
		err := runStep(ctx, "install", "-m", "0755", tmp, "/usr/bin/caddy")
		if err != nil {
			return err
		}
	}

	_ = os.Remove(tmp)
	return nil
}

// detectDebianCodename 从 /etc/os-release 读取 VERSION_CODENAME；失败回退 bullseye。
func detectDebianCodename() string {
	content, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "bullseye"
	}

	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "VERSION_CODENAME=") {
			value := strings.TrimPrefix(line, "VERSION_CODENAME=")
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			if value != "" {
				return value
			}
		}
	}

	return "bullseye"
}

func runStep(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("caddy install step %q failed: %w (output: %s)", strings.Join(append([]string{name}, args...), " "), err, truncate(string(output), 512))
	}

	return nil
}

func writeCaddyfile() error {
	{
		err := os.MkdirAll(caddyConfigDir, 0o755)
		if err != nil {
			return err
		}
	}

	content := `# Managed by incudal-agent: Caddy Admin API on loopback only.
# No public listener, no credentials, no certificates.
{
    admin localhost:2019
    auto_https disable_redirects
}
`
	{
		err := os.WriteFile(caddyfilePath, []byte(content), 0o644)
		if err != nil {
			return err
		}
	}

	return nil
}

func ensureService(ctx context.Context) error {
	// 兼容非 deb 包安装（手动二进制）：caddy.service unit 可能不存在，
	// 缺了直接 enable/restart 会失败。没有就写一个标准 unit。
	{
		err := ensureSystemdUnit()
		if err != nil {
			return err
		}
	}

	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "caddy"},
		{"restart", "caddy"},
	} {
		cmd := exec.CommandContext(ctx, "systemctl", args...)
		{
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl %s failed: %w (output: %s)", strings.Join(args, " "), err, truncate(string(output), 512))
			}
		}
	}

	return nil
}

// ensureSystemdUnit 保证 caddy.service 是我们生成的 root 运行版本。
// 幂等重写：旧版可能残留 User=caddy（会导致 Caddyfile 权限失败），
// 因此只要文件缺失或内容与我们期望的不一致就覆盖。
func ensureSystemdUnit() error {
	const unitPath = "/etc/systemd/system/caddy.service"
	content := `[Unit]
Description=Caddy
After=network.target

[Service]
Type=simple
ExecStart=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/bin/caddy reload --config /etc/caddy/Caddyfile
TimeoutStopSec=5s
LimitNOFILE=1048576
LimitNPROC=512
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
`

	// 已存在且内容一致则跳过，避免每次 install 都触碰 unit 文件。
	{
		existing, err := os.ReadFile(unitPath)
		if err == nil && string(existing) == content {
			_ = os.MkdirAll(caddyConfigDir, 0o755)
			return nil
		}
	}
	{
		err := os.WriteFile(unitPath, []byte(content), 0o644)
		if err != nil {
			return err
		}
	}

	_ = os.MkdirAll(caddyConfigDir, 0o755)
	return nil
}

func waitAdminReady(ctx context.Context, port int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/config/", port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		client := &http.Client{Timeout: 2 * time.Second}
		{
			resp, err := client.Get(url)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode < 500 {
					return nil
				}
			}
		}

		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("caddy admin not ready on 127.0.0.1:%d within 30s", port)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	return s[:limit] + "..."
}
