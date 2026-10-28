package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

var poolPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,39}$`)

// PayIncus: DKMS needs headers for the running kernel, which may not be a distro kernel.
func (b *backend) zfsKernelPrerequisites(ctx context.Context, kernel string) (bool, bool, error) {
	modules := b.modulesPath
	if modules == "" {
		modules = "/lib/modules"
	}
	sysModules := b.sysModulesPath
	if sysModules == "" {
		sysModules = "/sys/module"
	}
	_, err := os.Stat(filepath.Join(sysModules, "zfs"))
	loaded := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, err
	}
	_, err = os.Stat(filepath.Join(modules, kernel, "build", "Makefile"))
	headers := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, err
	}
	if loaded {
		return true, headers, nil
	}
	available := false
	err = filepath.WalkDir(filepath.Join(modules, kernel), func(_ string, entry fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && (entry.Name() == "zfs.ko" || strings.HasPrefix(entry.Name(), "zfs.ko.")) {
			available = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, fmt.Errorf("检查 ZFS 内核模块：%w", err)
	}
	return available, headers, nil
}

func (b *backend) prepareZFS(ctx context.Context) error {
	releasePath := b.releasePath
	if releasePath == "" {
		releasePath = "/etc/os-release"
	}
	data, err := os.ReadFile(releasePath)
	if err != nil {
		return err
	}
	release := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			release[k] = strings.Trim(v, `"`)
		}
	}
	debian := release["ID"] == "debian" && release["VERSION_ID"] == "13"
	ubuntu := release["ID"] == "ubuntu" && release["VERSION_ID"] == "24.04"
	if !debian && !ubuntu {
		return errors.New("仅支持 Debian 和 Ubuntu")
	}
	kernelOutput, err := b.run(ctx, "uname", "-r")
	if err != nil {
		return err
	}
	kernel := strings.TrimSpace(string(kernelOutput))
	if kernel == "" || strings.ContainsAny(kernel, "/\\\n\r") {
		return fmt.Errorf("无效的运行内核版本 %q", kernel)
	}
	moduleAvailable, headersAvailable, err := b.zfsKernelPrerequisites(ctx, kernel)
	if err != nil {
		return err
	}
	logOperation(ctx, "运行内核：%s；ZFS 模块可用：%t；已有匹配 headers：%t", kernel, moduleAvailable, headersAvailable)
	if debian {
		path := b.sourcePath
		if path == "" {
			path = "/etc/apt/sources.list.d/debian.sources"
		}
		source, readErr := os.ReadFile(path)
		if readErr == nil {
			lines := strings.Split(string(source), "\n")
			changed := false
			for i, line := range lines {
				if strings.HasPrefix(line, "Components:") && !strings.Contains(" "+line+" ", " contrib ") {
					lines[i] = line + " contrib"
					changed = true
				}
			}
			if changed {
				// Preserve the administrator's original source file; never replace its URIs or signing keys.
				backup := path + ".before-incus-tui"
				file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return fmt.Errorf("APT 配置备份失败 %s：%w", backup, err)
				}
				_, err = file.Write(source)
				closeErr := file.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
				err = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
				if err != nil {
					return err
				}
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("读取 APT 配置 %s：%w", path, readErr)
		}
	}
	_, err = b.run(ctx, "apt-get", "update")
	if err != nil {
		return err
	}
	packages := []string{"install", "-y", "--no-install-recommends", "zfsutils-linux", "util-linux", "kmod"}
	if !moduleAvailable {
		packages = append(packages, "zfs-dkms")
		if !headersAvailable {
			headerPackage := "linux-headers-" + kernel
			metadata, lookupErr := b.run(ctx, "apt-cache", "show", headerPackage)
			if lookupErr == nil && !strings.Contains(string(metadata), "Package: "+headerPackage+"\n") {
				lookupErr = fmt.Errorf("APT 未返回 %s 的软件包信息", headerPackage)
			}
			if lookupErr != nil {
				environment := "请提供运行内核对应的 headers，或使用具备 ZFS 支持的标准内核虚拟机。"
				if strings.Contains(kernel, "orbstack") {
					environment = "当前为 OrbStack 管理的定制内核，Debian 通用 headers 不匹配；OrbStack 自定义模块构建不属正式支持范围，需自行准备匹配构建环境或使用标准内核虚拟机。"
				}
				return fmt.Errorf("运行内核 %s 没有可用的 ZFS 模块，且缺少匹配 headers。%s 本次未执行 ZFS 软件包安装。原始检查错误：%w", kernel, environment, lookupErr)
			}
			packages = append(packages, headerPackage)
		}
	}
	_, err = b.run(ctx, "apt-get", packages...)
	if err != nil {
		return fmt.Errorf("ZFS 依赖安装失败（APT/DKMS 原始输出见操作日志）：%w", err)
	}
	_, err = b.run(ctx, "modprobe", "zfs")
	if err != nil {
		return fmt.Errorf("ZFS 内核模块加载失败；未创建存储：%w", err)
	}
	_, err = b.run(ctx, "zfs", "version")
	return err
}

func (b *backend) createPool(ctx context.Context, name, device, size string) error {
	if (device == "") == (size == "") {
		return errors.New("磁盘路径与测试文件容量必须且只能填写一项")
	}
	if !poolPattern.MatchString(name) {
		return errors.New("池名称格式不正确")
	}
	var target string
	if size != "" {
		gib, err := strconv.ParseInt(size, 10, 64)
		if err != nil || gib < 1 || gib > 65536 {
			return errors.New("文件池容量必须为 1–65536 GiB")
		}
		root := os.Getenv("INCUS_DIR")
		if root == "" {
			root = "/var/lib/incus"
		}
		dir := filepath.Join(root, "disks")
		err = os.MkdirAll(dir, 0o700)
		if err != nil {
			return err
		}
		target = filepath.Join(dir, name+".img")
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("创建池文件 %s 失败，不覆盖已有文件：%w", target, err)
		}
		err = file.Close()
		if err != nil {
			return err
		}
		_, err = b.run(ctx, "fallocate", "-l", strconv.FormatInt(gib<<30, 10), target)
		if err != nil {
			return fmt.Errorf("文件预分配失败，未创建池：%w", err)
		}
	} else {
		if !strings.HasPrefix(device, "/dev/") || strings.Contains(device, "..") {
			return errors.New("请输入 /dev 下的专用磁盘或分区")
		}
		info, err := os.Stat(device)
		if err != nil {
			return fmt.Errorf("读取块设备 %s：%w", device, err)
		}
		if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return errors.New("不是可用的块设备")
		}
		output, err := b.run(ctx, "lsblk", "-n", "-o", "MOUNTPOINTS,FSTYPE", device)
		if err != nil {
			return fmt.Errorf("检查块设备 %s：%w", device, err)
		}
		if strings.TrimSpace(string(output)) != "" {
			return errors.New("设备含文件系统或挂载，拒绝覆盖；请管理员自行准备空设备")
		}
		target = device
	}
	_, err := b.run(ctx, "zpool", "create", "-o", "ashift=12", "-O", "mountpoint=none", "-O", "canmount=off", name, target)
	if err != nil {
		return fmt.Errorf("池创建失败；未使用强制覆盖，预分配文件保留供检查：%w", err)
	}
	return nil
}

func keyFingerprint(key string) string {
	data, _ := hex.DecodeString(key)
	sum := sha256.Sum256(data)
	clear(data)
	return hex.EncodeToString(sum[:])
}

func (m model) zfsMenu(action string) (tea.Model, tea.Cmd) {
	m.focus = 0
	switch action {
	case "zfs-prepare":
		m.page = "confirm"
		m.message = "安装 ZFS 工具；Debian 将在标准 deb822 源中添加 contrib 并备份原文件，安装匹配内核 headers/DKMS。不会选择或格式化磁盘。"
		m.confirm = m.backend.prepareZFS
	case "zpool":
		m.page = "zpool"
		m.fields = []field{{label: "新池名称"}, {label: "专用磁盘/分区 /dev/...（创建文件池时留空）"}, {label: "测试文件池容量 GiB（使用磁盘时留空）"}}
	case "zfs-unlock":
		m.page = "zfs-unlock"
		m.fields = []field{{label: "已注册加密数据集"}, {label: "ZFS hex 密钥", secret: true}}
	case "zfs-bind":
		m.page = "zfs-bind"
		m.fields = []field{{label: "新的 Incus 存储池名称"}, {label: "已创建且已解锁的 ZFS 数据集"}}
	case "zfs-register":
		m.page = "zfs-register"
		m.fields = []field{{label: "已有加密根数据集"}, {label: "已有 ZFS hex 密钥", secret: true}}
	case "zfs-list":
		m.busy = true
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15e9)
			defer cancel()
			ctx = context.WithValue(ctx, operationLogKey{}, m.backend.logs)
			output, err := m.backend.run(ctx, "zpool", "list", "-H", "-o", "name,health,size,alloc,free")
			return resultMsg{text: string(output), err: err}
		}
	}
	return m, nil
}

func (m model) submitZFS(values []string) (tea.Model, tea.Cmd) {
	switch m.page {
	case "zpool":
		m.message = fmt.Sprintf("确认创建池 %s：设备=%s，测试文件容量=%s GiB。设备创建可能不可逆；不会使用 -f 强制覆盖。", values[0], values[1], values[2])
		m.message += " 将自动创建 AES-256-GCM 加密数据集 " + values[0] + "/incus；完成后显示密钥，请保存后自行导入存储池。"
		m.poolRequest = &encryptedPoolRequest{name: values[0], device: values[1], size: values[2]}
		m.confirm = nil
	case "zfs-register":
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(values[1]) {
			m.message = "请输入 64 位十六进制 ZFS 密钥"
			return m, nil
		}
		m.message = "核验已有加密数据集并登记到节点；不会将密钥写入本机文件。请自行保存密钥。"
		m.confirm = func(ctx context.Context) error {
			c := newLocalClient()
			defer c.http.CloseIdleConnections()
			_, err := c.call(ctx, "POST", "/1.0/payincus/storage/register", map[string]string{"dataset": values[0], "key": values[1]})
			return err
		}
	case "zfs-unlock":
		m.message = "确认加载存储密钥；这不会解除节点锁定或自动启动容器，恢复仍需控制端批准。"
		m.confirm = func(ctx context.Context) error {
			c := newLocalClient()
			defer c.http.CloseIdleConnections()
			_, err := c.call(ctx, "POST", "/1.0/payincus/storage/unlock", map[string]string{"dataset": values[0], "key": values[1]})
			return err
		}
	case "zfs-bind":
		if !poolPattern.MatchString(values[0]) || !strings.Contains(values[1], "/") {
			m.message = "请输入池名称及专用子数据集"
			return m, nil
		}
		m.message = "确认将指定现有数据集交给 Incus 管理；它将管理其中的子数据集，不会迁移 DIR 数据。"
		m.confirm = func(ctx context.Context) error {
			c := newLocalClient()
			defer c.http.CloseIdleConnections()
			data, err := c.call(ctx, "GET", "/1.0/payincus/security", nil)
			if err != nil {
				return err
			}
			var registration struct {
				Targets []struct {
					Dataset string `json:"dataset"`
				} `json:"targets"`
			}
			err = json.Unmarshal(data, &registration)
			if err != nil {
				return fmt.Errorf("解析本机加密数据集注册信息：%w", err)
			}
			found := false
			for _, target := range registration.Targets {
				if target.Dataset == values[1] {
					found = true
				}
			}
			if !found {
				return errors.New("请先创建或注册此加密根并完成密钥托管")
			}
			_, err = c.call(ctx, "POST", "/1.0/storage-pools", map[string]any{"name": values[0], "driver": "zfs", "config": map[string]string{"source": values[1]}})
			return err
		}
	}
	m.fields = nil
	m.page = "confirm"
	return m, nil
}
