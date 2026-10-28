package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const configPath = "/etc/incudal-agent/config.yaml"

// Runner centralizes node API and system command adapters.
type Runner interface {
	Run(context.Context, string, []string, string) ([]byte, error)
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args []string, input string) ([]byte, error) {
	// PayIncus: package frontends must not open a second terminal UI inside operation logs.
	packageCommand := filepath.Base(name) == "apt-get"
	if packageCommand {
		args = append([]string{"-o", "Dpkg::Use-Pty=0", "-o", "Dpkg::Progress-Fancy=0"}, args...)
	}
	logOperation(ctx, "$ %s %s", name, strings.Join(args, " "))
	if name == "/usr/bin/incus" {
		return runIncusAdapter(ctx, args, input)
	}
	cmd := systemCommand(ctx, name, args...)
	if packageCommand {
		cmd.Env = append(cmd.Environ(), "DEBIAN_FRONTEND=noninteractive", "DEBIAN_PRIORITY=critical", "APT_LISTCHANGES_FRONTEND=text", "NEEDRESTART_MODE=l", "TERM=dumb")
		logOperation(ctx, "软件包安装使用非交互配置；采用包默认答案，needrestart 仅列出待重启服务；保留全部 stdout/stderr")
	}
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = strings.NewReader(input)
	var output bytes.Buffer
	stream := &commandLogWriter{ctx: ctx}
	writer := io.MultiWriter(&output, stream)
	cmd.Stdout = writer
	cmd.Stderr = writer
	err := cmd.Run()
	stream.flush()
	if err != nil {
		logOperation(ctx, "%s: %v", name, err)
		return output.Bytes(), fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(output.String()))
	}
	logOperation(ctx, "%s: completed (exit 0)", name)
	return output.Bytes(), nil
}

type backend struct {
	runner         Runner
	path           string
	client         *http.Client
	releasePath    string
	sourcePath     string
	modulesPath    string
	sysModulesPath string
	updateLockPath string
	logs           *operationLog
}

func newBackend() *backend {
	return &backend{runner: commandRunner{}, path: configPath, client: &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("Panel redirects are not allowed") },
	}}
}

func (b *backend) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "/usr/bin/incus" {
		args = append([]string{"--force-local"}, args...)
	}
	return b.runner.Run(ctx, name, args, "")
}

func (b *backend) service(ctx context.Context, unit, action string) error {
	if unit != "incus" && unit != "incudal-agent" {
		return errors.New("unknown service")
	}
	switch action {
	case "start", "stop", "restart", "enable", "disable":
	default:
		return errors.New("unknown service action")
	}
	args := []string{action, unit}
	if action == "enable" {
		args = []string{"enable", "--now", unit}
	}
	_, err := b.run(ctx, "systemctl", args...)
	return err
}

// PayIncus: Type=simple becoming active does not mean that the Unix API is ready.
func (b *backend) manageService(ctx context.Context, unit, action string) error {
	err := b.service(ctx, unit, action)
	if err != nil {
		return err
	}
	if unit == "incus" && (action == "start" || action == "restart" || action == "enable") {
		logOperation(ctx, "Incus 服务命令已完成，等待节点 API 就绪")
		return b.waitUpdatedNode(ctx)
	}
	return nil
}

func (b *backend) connectorReady(ctx context.Context) error {
	output, err := b.run(ctx, "systemctl", "show", "incudal-agent", "--property=ExecStart", "--value")
	if err != nil {
		return err
	}
	if !strings.Contains(string(output), "/usr/libexec/incus/payincus-agent") {
		return errors.New("节点连接服务配置不匹配，请检查 Incus 软件包与 systemd 配置")
	}
	return nil
}

func (b *backend) connect(ctx context.Context, c connection) error {
	contextLog(ctx).secret(c.Secret)
	err := b.connectorReady(ctx)
	if err != nil {
		return err
	}
	logOperation(ctx, "保存 Panel 连接配置：%s (0600)", b.path)
	err = b.saveConnection(c)
	if err != nil {
		return err
	}
	err = b.service(ctx, "incudal-agent", "enable")
	if err != nil {
		return err
	}
	return b.service(ctx, "incudal-agent", "restart")
}

func (b *backend) status(ctx context.Context) string {
	var lines []string
	for _, unit := range []string{"incus", "incudal-agent"} {
		data, err := b.run(ctx, "systemctl", "show", unit, "--property=ActiveState,SubState,UnitFileState", "--no-pager")
		if err != nil {
			lines = append(lines, unit+": 状态读取失败: "+err.Error())
			continue
		}
		lines = append(lines, unit+": "+strings.ReplaceAll(strings.TrimSpace(string(data)), "\n", "  "))
	}
	data, err := b.run(ctx, "/usr/bin/incus", "query", "/1.0/payincus")
	var capabilities struct {
		Extensions []string `json:"api_extensions"`
	}
	if err == nil {
		err = json.Unmarshal(data, &capabilities)
	}
	supported := false
	for _, extension := range capabilities.Extensions {
		if extension == "payincus_node_v1" {
			supported = true
		}
	}
	if err != nil || !supported {
		lines = append(lines, "节点 API: 不可用，请启动 Incus 或查看日志")
		if err != nil {
			lines = append(lines, err.Error())
		}
	} else {
		lines = append(lines, "节点 API: 可连接")
	}
	pending, pendingErr := b.installedUpdatePending(ctx)
	if pendingErr != nil {
		lines = append(lines, "运行版本检查失败："+pendingErr.Error())
	} else if pending {
		lines = append(lines, "待应用更新：Incus 仍运行已被替换的旧二进制，ID 映射等启动配置可能未生效。请在维护窗口选择“更新 → 应用已安装更新（无需下载）”。")
	}
	data, err = b.run(ctx, "/usr/bin/incus", "query", "/1.0")
	var identity struct {
		Environment struct {
			Fingerprint string `json:"certificate_fingerprint"`
		} `json:"environment"`
	}
	if err == nil {
		err = json.Unmarshal(data, &identity)
	}
	if err == nil && regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(identity.Environment.Fingerprint) {
		lines = append(lines, "节点证书 SHA256（控制端核对）："+identity.Environment.Fingerprint)
	}
	data, err = b.run(ctx, "/usr/bin/incus", "query", "/1.0/networks?recursion=1")
	if err != nil {
		lines = append(lines, "NAT 网桥检查失败："+err.Error())
	} else {
		var networks []nodeNetwork
		if json.Unmarshal(data, &networks) != nil || networks == nil {
			lines = append(lines, "NAT 网桥：网络列表格式不兼容")
		} else {
			ready := false
			for _, network := range networks {
				if network.Name == panelBridge && compatiblePanelNetwork(&network) {
					ready = true
				}
			}
			if ready {
				lines = append(lines, "NAT 网桥：incusbr0 · 10.10.0.1/22 · 就绪")
			} else {
				lines = append(lines, "NAT 网桥：缺失或配置不匹配，请使用“修复 → 修复网桥”")
			}
		}
	}
	return strings.Join(lines, "\n")
}

type connection struct{ Panel, ID, Secret string }

var (
	idPattern     = regexp.MustCompile(`^agt_[A-Za-z0-9_-]{24,64}$`)
	secretPattern = regexp.MustCompile(`^ias_[A-Za-z0-9_-]{32,96}$`)
	tokenPattern  = regexp.MustCompile(`^ait_[A-Za-z0-9_-]{32,96}$`)
)

func panelURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("请输入 Panel 根地址，例如 https://panel.example.com")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("Panel 地址必须使用 HTTP 或 HTTPS")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (c connection) validate() error {
	_, err := panelURL(c.Panel)
	if err != nil {
		return err
	}
	if !idPattern.MatchString(c.ID) || !secretPattern.MatchString(c.Secret) {
		return errors.New("Agent ID 或密钥格式不正确")
	}
	return nil
}

type enrollment struct {
	connection
	Certificate []byte
	Fingerprint string
}

func (b *backend) exchange(ctx context.Context, panel, token string) (enrollment, error) {
	contextLog(ctx).secret(token)
	base, err := panelURL(panel)
	if err != nil {
		return enrollment{}, err
	}
	if !tokenPattern.MatchString(token) {
		return enrollment{}, errors.New("安装 token 格式不正确，应以 ait_ 开头")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/agent/install-config/"+token+"?format=json", nil)
	if err != nil {
		return enrollment{}, redactedDiagnostic(ctx, fmt.Errorf("构造 Panel 凭据请求：%w", err))
	}
	req.Header.Set("Accept", "application/json")
	response, err := b.client.Do(req)
	if err != nil {
		return enrollment{}, redactedDiagnostic(ctx, fmt.Errorf("Panel 凭据请求失败：%w", err))
	}
	defer response.Body.Close()
	logOperation(ctx, "Panel GET %s/api/agent/install-config/[REDACTED]: HTTP %d", base, response.StatusCode)
	if response.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 8193))
		if readErr != nil {
			return enrollment{}, fmt.Errorf("Panel HTTP %d: reading error response: %w", response.StatusCode, readErr)
		}
		return enrollment{}, fmt.Errorf("Panel 凭据兑换失败：HTTP %d（token 只能使用一次）: %s", response.StatusCode, safePayload(ctx, data))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 131073))
	if err != nil {
		return enrollment{}, fmt.Errorf("Panel 凭据响应读取失败：%w", err)
	}
	if len(data) > 131072 {
		return enrollment{}, errors.New("Panel 接入响应超过128 KiB，拒绝解析")
	}
	var config struct {
		SchemaVersion                     int    `json:"schemaVersion"`
		AgentID                           string `json:"agentId"`
		AgentSecret                       string `json:"agentSecret"`
		PanelClientCertificate            string `json:"panelClientCertificate"`
		PanelClientCertificateFingerprint string `json:"panelClientCertificateFingerprint"`
	}
	err = json.Unmarshal(data, &config)
	if err != nil {
		return enrollment{}, fmt.Errorf("解析 Panel JSON 接入响应（需要新版控制端）：%w", err)
	}
	c := connection{Panel: base, ID: config.AgentID, Secret: config.AgentSecret}
	contextLog(ctx).secret(c.Secret)
	if config.SchemaVersion != 1 {
		return enrollment{}, fmt.Errorf("不支持的 Panel 接入协议版本 %d，需要版本1", config.SchemaVersion)
	}
	err = c.validate()
	if err != nil {
		return enrollment{}, err
	}
	cert, fingerprint, err := parseCertificate([]byte(config.PanelClientCertificate))
	if err != nil {
		return enrollment{}, fmt.Errorf("Panel 客户端公钥证书：%w", err)
	}
	if config.PanelClientCertificateFingerprint != fingerprint {
		return enrollment{}, errors.New("Panel 客户端证书 SHA256 指纹不匹配")
	}
	logOperation(ctx, "Panel 凭据兑换完成；客户端公钥证书 SHA256：%s", fingerprint)
	return enrollment{connection: c, Certificate: cert, Fingerprint: fingerprint}, nil
}

func (b *backend) loadConnection() connection {
	data, err := os.ReadFile(b.path)
	if err != nil {
		return connection{}
	}
	var c connection
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "panel_url":
			c.Panel = value
		case "agent_id":
			c.ID = value
		case "agent_secret":
			c.Secret = value
		}
	}
	return c
}

func (b *backend) saveConnection(c connection) error {
	c.Panel = strings.TrimRight(c.Panel, "/")
	err := c.validate()
	if err != nil {
		return err
	}
	dir := filepath.Dir(b.path)
	err = os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}
	// Atomic rename preserves the last complete configuration on write failure.
	file, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	content := fmt.Sprintf("panel_url: %s\nagent_id: %s\nagent_secret: %s\nheartbeat_interval_seconds: 10\nrequest_timeout_seconds: 10\n", strconv.Quote(c.Panel), strconv.Quote(c.ID), strconv.Quote(c.Secret))
	_, err = file.WriteString(content)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), b.path)
}

const preseed = `config: {}
networks:
- name: incusbr0
  type: bridge
  config:
    ipv4.address: 10.10.0.1/22
    ipv4.nat: "true"
    ipv6.address: none
storage_pools:
- name: default
  driver: dir
profiles:
- name: default
  config: {}
  devices:
    root:
      type: disk
      path: /
      pool: default
    eth0:
      type: nic
      network: incusbr0
      name: eth0
`

func (b *backend) initialize(ctx context.Context) error {
	err := b.service(ctx, "incus", "enable")
	if err != nil {
		return err
	}
	// Type=simple is not readiness; wait for the real socket API with a bounded context.
	for {
		data, readyErr := b.run(ctx, "/usr/bin/incus", "query", "/1.0")
		if readyErr == nil {
			var server struct {
				Environment struct {
					Clustered *bool `json:"server_clustered"`
				} `json:"environment"`
			}
			err = json.Unmarshal(data, &server)
			if err != nil {
				return fmt.Errorf("解析 Incus server 响应：%w", err)
			}
			if server.Environment.Clustered == nil {
				return errors.New("Incus server 响应格式不兼容")
			}
			if *server.Environment.Clustered {
				return errors.New("集群节点不支持自动初始化，请手动管理")
			}
			break
		}
		logOperation(ctx, "等待 Incus API 就绪：%v", readyErr)
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 Incus API 就绪：%w；最后错误：%v", ctx.Err(), readyErr)
		case <-time.After(time.Second):
		}
	}
	for _, path := range []string{"/1.0/storage-pools", "/1.0/instances?all-projects=true"} {
		data, queryErr := b.run(ctx, "/usr/bin/incus", "query", path)
		if queryErr != nil {
			return queryErr
		}
		var entries []string
		queryErr = json.Unmarshal(data, &entries)
		if queryErr != nil {
			return fmt.Errorf("解析 Incus %s 响应：%w", path, queryErr)
		}
		if entries == nil {
			return fmt.Errorf("Incus %s 响应格式不兼容", path)
		}
		if len(entries) != 0 {
			if path != "/1.0/storage-pools" || len(entries) != 1 || entries[0] != "/1.0/storage-pools/default" {
				return errors.New("已有其它存储池或实例，拒绝基础初始化；网络缺失请使用“修复 → 修复网桥”")
			}
			data, queryErr = b.run(ctx, "/usr/bin/incus", "query", entries[0])
			if queryErr != nil {
				return queryErr
			}
			var pool struct {
				Driver string `json:"driver"`
			}
			if json.Unmarshal(data, &pool) != nil || pool.Driver != "dir" {
				return errors.New("已有 default 存储池不是 dir，拒绝基础初始化")
			}
		}
	}
	data, err := b.run(ctx, "/usr/bin/incus", "query", "/1.0/networks?recursion=1")
	if err != nil {
		return err
	}
	var networks []struct {
		Name    string            `json:"name"`
		Managed *bool             `json:"managed"`
		Type    string            `json:"type"`
		Config  map[string]string `json:"config"`
	}
	err = json.Unmarshal(data, &networks)
	if err != nil {
		return fmt.Errorf("解析 Incus 网络响应：%w", err)
	}
	if networks == nil {
		return errors.New("Incus 网络响应格式不兼容")
	}
	for _, network := range networks {
		if network.Managed == nil || network.Name == "" {
			return errors.New("Incus 网络响应缺少必要字段")
		}
		if *network.Managed || network.Name == panelBridge {
			if !compatiblePanelNetwork(&nodeNetwork{Name: network.Name, Managed: *network.Managed, Type: network.Type, Config: network.Config}) || network.Name != panelBridge {
				return errors.New("已有其它网络或不匹配的 incusbr0，拒绝覆盖；请使用“修复 → 修复网桥”")
			}
		}
	}
	data, err = b.run(ctx, "/usr/bin/incus", "query", "/1.0/profiles/default")
	if err != nil {
		return err
	}
	var profile struct {
		Config  map[string]string            `json:"config"`
		Devices map[string]map[string]string `json:"devices"`
	}
	err = json.Unmarshal(data, &profile)
	if err != nil {
		return fmt.Errorf("解析 Incus profile 响应：%w", err)
	}
	if profile.Config == nil || profile.Devices == nil {
		return errors.New("Incus profile 响应格式不兼容")
	}
	if !compatibleInitialProfile(profile.Config, profile.Devices) {
		return errors.New("default profile 已有配置，拒绝覆盖")
	}
	logOperation(ctx, "初始化配置：\n%s", preseed)
	_, err = b.runner.Run(ctx, "/usr/bin/incus", []string{"--force-local", "admin", "init", "--preseed"}, preseed)
	if err != nil {
		return fmt.Errorf("初始化失败，已完成步骤保留；基础资源匹配时可重试，已有实例请使用“修复 → 修复网桥”：%w", err)
	}
	return nil
}

func readCertificate(path string) ([]byte, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return nil, "", fmt.Errorf("读取证书 %s：%w", path, err)
	}
	return parseCertificate(data)
}

func parseCertificate(data []byte) ([]byte, string, error) {
	if len(data) > 65536 {
		return nil, "", errors.New("证书文件超过65536字节")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("需要单个 PEM 公钥证书，不能包含私钥")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("解析 X.509 证书：%w", err)
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return nil, "", errors.New("证书尚未生效或已经过期")
	}
	digest := sha256.Sum256(cert.Raw)
	return data, hex.EncodeToString(digest[:]), nil
}

func (b *backend) trust(ctx context.Context, cert []byte, fingerprint string) error {
	block, _ := pem.Decode(cert)
	if block == nil {
		return errors.New("证书无效")
	}
	digest := sha256.Sum256(block.Bytes)
	if hex.EncodeToString(digest[:]) != fingerprint {
		return errors.New("证书指纹不匹配")
	}
	file, err := os.CreateTemp("", "incus-panel-*.crt")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(cert)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	_, err = b.run(ctx, "/usr/bin/incus", "config", "trust", "add-certificate", file.Name(), "--name", "panel")
	return err
}

func (b *backend) checkListener(ctx context.Context) error {
	data, err := b.run(ctx, "/usr/bin/incus", "config", "get", "core.https_address")
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(data))
	if current != "" && current != "127.0.0.1:8443" {
		return errors.New("已有 HTTPS 监听配置，拒绝覆盖：" + current)
	}
	return nil
}

func (b *backend) listener(ctx context.Context) error {
	err := b.checkListener(ctx)
	if err != nil {
		return err
	}
	_, err = b.run(ctx, "/usr/bin/incus", "config", "set", "core.https_address", "127.0.0.1:8443")
	return err
}
