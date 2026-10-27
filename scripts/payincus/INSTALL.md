# 安装与节点管理

安装器只选择 GitHub 发布包、校验 SHA256 并通过 apt 安装；成功后打开 `incus-tui`，不自动初始化、启动或重启服务。

## 支持的平台

| 系统 | 架构 | 包 |
| --- | --- | --- |
| Debian 13 | amd64 / arm64 | 对应 Debian 13 原生 deb |
| Ubuntu 24.04 | amd64 / arm64 | 对应 Ubuntu 24.04 原生 deb |

不把 Ubuntu 二进制包当作任意 Debian 系统的通用包。其他版本、发行版和架构明确拒绝安装。依赖由目标系统 apt 源解析；VM 仍需另外安装 QEMU/UEFI 并提供 `/dev/kvm`。嵌套容器环境还需外层配置 UID/GID 映射、嵌套权限及内核能力，TUI 不改写外层环境。

## 安装命令

以下命令需本变更已推送到对应分支，且该渠道已发布包含 TUI 的新包后使用；本地开发完成不等于远端已发布。

稳定版：

```bash
curl -fsSL https://raw.githubusercontent.com/Rain-kl/incus/main/scripts/payincus/install.sh -o /tmp/incus-install.sh
sudo bash /tmp/incus-install.sh latest
```

canary 集成版：

```bash
curl -fsSL https://raw.githubusercontent.com/Rain-kl/incus/canary/scripts/payincus/install.sh -o /tmp/incus-install.sh
sudo bash /tmp/incus-install.sh canary
```

也支持 `curl ... | sudo bash -s -- canary`，交互界面会重新打开 `/dev/tty`。无交互终端时只安装，提示稍后运行 `sudo incus-tui`；自动化安装可传第二个参数 `--no-tui`。

`latest` 默认使用 GitHub 最新稳定发布，要求 tag 为 `payincus-v7.0.2` 这类正式版本；没有稳定包时失败，不降级到预发布。`canary` 在最近 100 个 GitHub release 中选择发布时间最新的 `integration-<LTS>-<SHA前12位>` prerelease。选择该 release 后必须有对应系统/架构 deb 和 `SHA256SUMS-<系统>-<架构>`；不回退到旧 release 或其他平台。旧版仅提供通用文件名的集成包不符合新安装契约，需发布新包。

校验文件与 deb 来自同一 GitHub release，使用 HTTPS；SHA256 检测损坏和不匹配，不能替代独立签名或仓库账号信任。脚本不关闭 TLS 校验、不执行下载的配置、不修改 apt 软件源、不自动处理降级。安装已有节点前应安排维护窗口；升级安装不会自动重启现有 daemon，TUI 操作运行中的服务需要明确确认。

## 首次设置

1. 执行 `sudo incus-tui`，选择“一键初始化”。确认后启用并启动 Incus，等待本地 API 就绪；为全新非集群节点创建 `default`（dir 驱动）和 `incusbr0`（自动 IPv4 + NAT、IPv6 禁用），给 default profile 添加根磁盘和 eth0。已有存储池、任意项目实例、托管网络、同名网桥或 default profile 配置时拒绝覆盖。

TUI 的搭建、配置命令与本地 API 响应会完整实时输出到终端，并保存到界面显示的 `/var/log/incus-tui/session-*.log`。HTTP/异步 operation 失败保留原始错误；敏感凭据遮蔽，其他输出不截断。Panel 地址可使用 HTTP 或 HTTPS，由管理员自行选择；一次性凭据请求不跟随重定向。
2. 在 Panel 创建或选择宿主机，启用 Agent，取得 `agt_` ID 和 `ias_` 密钥，或生成一次性 `ait_` token。TUI 可直接粘贴 token，不需要运行旧安装命令。此 token 通常 30 分钟有效且只能消费一次；不要粘贴整条 shell 安装命令。
3. 选择配置 Panel 连接。根地址允许 HTTP 或 HTTPS，由管理员自行选择；密钥/token 输入掩码显示。确认后原子写入 `/etc/incudal-agent/config.yaml`（0600），启用并重启 `incudal-agent.service`。连接服务配置必须匹配软件包提供的 unit，不匹配时拒绝操作且不消费 token；不执行旧服务迁移。
4. 如果使用反向隧道，在 Panel 启用隧道，目标为 `127.0.0.1:8443`；TUI 选择“设置反向隧道本地监听”。该操作不会覆盖已有其他 HTTPS 监听地址。
5. 把 Panel 客户端的 PEM **公钥证书**放到节点，选择“导入 Panel 客户端证书”，核对 SHA256 指纹后确认。此证书将授予 Panel 节点管理权限；不导入私钥，不删除已有信任。证书和 Agent token 是不同用途，Agent 心跳在线不等于 mTLS 实例管理连接已验证。
6. 在 Panel 验证宿主机连接。

需要定制存储/ZFS/桥接网络/集群时，使用官方 `incus admin init`；TUI 的一键初始化是固定的基础配置，不自动迁移已有资源。dir 驱动的配额与快照能力有限，有存储配额需求时应选择经过验证的 ZFS/Btrfs 等方案。初始化失败可能留下部分资源，不会自动删除或盲目重试，请检查错误后手动处理。

## 运维

TUI 支持 Incus 和连接 Agent 的启动、停止、重启、启用并启动、关闭开机启动；所有写操作需界面内确认。查看日志使用 `journalctl -u incus` / `incudal-agent -n 100 -f`，退出日志后返回面板。服务操作最多等待三分钟，Esc 可取消等待；已经提交给 systemd 或 Incus 的变更不会自动回滚。Incus 停止命令可能由 systemd 在后台继续处理。

Incus 本地服务仅依赖 `network.target`，不因 DNS 的 `wait-online` 超时阻塞本地 socket；连接器仍等待网络在线。已有自定义 systemd 覆盖配置保持有效，需要自行检查。

## 可选 ZFS 与节点管理

默认初始化仍使用 DIR。运行 `incus-tui` 可显式安装 ZFS 依赖、创建专用磁盘或测试文件池、创建/注册 AES-256-GCM 子数据集并绑定到 Incus。不会自动挑盘、迁移或覆盖已有数据。Debian 13 标准 deb822 源补 contrib 前会备份原文件；特殊内核、Secure Boot 和自定义源需管理员处理。

创建根之前，将 TUI 显示的随机 hex 密钥保存到配套控制端，复制 SHA256 指纹回 TUI 核对。节点不主动保存明文密钥文件；终端录屏、剪贴板、swap/core dump 仍由管理员管理。可信终端中的 TUI 状态区同时提供节点服务端证书 SHA256，控制端需先人工核对保存，才允许新的管理/发钥链路连接。

关键 CLI 诱捕默认关闭，DIR 节点也可在控制端开启。触发后全部 LXC 停止、API 持续锁定，ZFS 按注册情况卸载/清钥；Agent 与 incusd 保持通信。TUI 加载密钥不会批准节点恢复，须控制端授权后按需启动容器。重启、重连、关闭诱捕均不清除已有锁定。详情与限制见根目录 FEATURES.md 及配套 PayIncus 仓库 `docs/NODE_SECURITY.md`。

控制端不再分发独立 Agent。由控制端安装命令传入的 Panel 地址和一次性 token 会在 TUI 中预填并隐藏 token，确认后才兑换；无终端时稍后手工配置。此流程面向全新节点，不提供旧 Agent 或旧节点升级迁移。
