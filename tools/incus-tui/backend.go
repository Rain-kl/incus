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
	"os/exec"
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
	if name == "/usr/bin/incus" {
		return runIncusAdapter(ctx, args, input)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type backend struct {
	runner      Runner
	path        string
	client      *http.Client
	releasePath string
	sourcePath  string
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
	err := b.connectorReady(ctx)
	if err != nil {
		return err
	}
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
			lines = append(lines, unit+": 状态读取失败")
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
	} else {
		lines = append(lines, "节点 API: 可连接")
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
	localHTTP := parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")
	if parsed.Scheme != "https" && !localHTTP {
		return "", errors.New("Panel 必须使用 HTTPS；仅本机调试允许 HTTP")
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

func (b *backend) exchange(ctx context.Context, panel, token string) (connection, error) {
	base, err := panelURL(panel)
	if err != nil {
		return connection{}, err
	}
	if !tokenPattern.MatchString(token) {
		return connection{}, errors.New("安装 token 格式不正确，应以 ait_ 开头")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/agent/install-config/"+token, nil)
	if err != nil {
		return connection{}, errors.New("无法构造凭据请求")
	}
	response, err := b.client.Do(req)
	if err != nil {
		return connection{}, errors.New("无法连接 Panel，或请求被取消；请检查地址、TLS 和网络")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return connection{}, fmt.Errorf("Panel 凭据兑换失败：HTTP %d（token 只能使用一次）", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(data) > 8192 {
		return connection{}, errors.New("Panel 凭据响应读取失败或过大")
	}
	// Parse the current Panel's two shell assignments as data; never source or execute them.
	c := connection{Panel: base}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || seen[key] || (key != "INCUDAL_AGENT_ID" && key != "INCUDAL_AGENT_SECRET") {
			return connection{}, errors.New("Panel 凭据响应格式不兼容")
		}
		seen[key] = true
		if len(value) < 2 || value[0] != '\'' || value[len(value)-1] != '\'' {
			return connection{}, errors.New("Panel 凭据响应格式不兼容")
		}
		value = value[1 : len(value)-1]
		if key == "INCUDAL_AGENT_ID" {
			c.ID = value
		} else {
			c.Secret = value
		}
	}
	return c, c.validate()
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
    ipv4.address: auto
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
			if err != nil || server.Environment.Clustered == nil {
				return errors.New("Incus server 响应格式不兼容")
			}
			if *server.Environment.Clustered {
				return errors.New("集群节点不支持自动初始化，请手动管理")
			}
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 Incus API 就绪超时：%w", ctx.Err())
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
		if queryErr != nil || entries == nil {
			return fmt.Errorf("Incus %s 响应格式不兼容", path)
		}
		if len(entries) != 0 {
			return errors.New("已有存储池或实例，拒绝覆盖；请使用 incus admin init 手动管理")
		}
	}
	data, err := b.run(ctx, "/usr/bin/incus", "query", "/1.0/networks?recursion=1")
	if err != nil {
		return err
	}
	var networks []struct {
		Name    string `json:"name"`
		Managed *bool  `json:"managed"`
	}
	err = json.Unmarshal(data, &networks)
	if err != nil || networks == nil {
		return errors.New("Incus 网络响应格式不兼容")
	}
	for _, network := range networks {
		if network.Managed == nil || network.Name == "" {
			return errors.New("Incus 网络响应缺少必要字段")
		}
		if *network.Managed || network.Name == "incusbr0" {
			return errors.New("已有托管网络或 incusbr0，拒绝覆盖")
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
	if err != nil || profile.Config == nil || profile.Devices == nil {
		return errors.New("Incus profile 响应格式不兼容")
	}
	if len(profile.Config) != 0 || len(profile.Devices) != 0 {
		return errors.New("default profile 已有配置，拒绝覆盖")
	}
	_, err = b.runner.Run(ctx, "/usr/bin/incus", []string{"--force-local", "admin", "init", "--preseed"}, preseed)
	if err != nil {
		return fmt.Errorf("初始化失败，可能已有部分资源，请手动检查后再操作：%w", err)
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
	if err != nil || len(data) > 65536 {
		return nil, "", errors.New("证书读取失败或文件过大")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("需要单个 PEM 公钥证书，不能包含私钥")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", errors.New("无效的 X.509 证书")
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

func (b *backend) listener(ctx context.Context) error {
	data, err := b.run(ctx, "/usr/bin/incus", "config", "get", "core.https_address")
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(data))
	if current != "" && current != "127.0.0.1:8443" {
		return errors.New("已有 HTTPS 监听配置，拒绝覆盖：" + current)
	}
	_, err = b.run(ctx, "/usr/bin/incus", "config", "set", "core.https_address", "127.0.0.1:8443")
	return err
}
