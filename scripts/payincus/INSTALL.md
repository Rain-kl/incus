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

校验文件与 deb 来自同一 GitHub release，使用 HTTPS；SHA256 检测损坏和不匹配，不能替代独立签名或仓库账号信任。脚本不关闭 TLS 校验、不执行下载的配置、不修改 apt 软件源。常规安装模式不自动处理降级或重启现有 daemon；TUI 更新模式另见下文。安装已有节点前应安排维护窗口。

## 更新已安装节点

进入“更新”，选择“开发版（canary）”或“稳定版（正式 Release）”。确认后从 GitHub 下载对应渠道最新的本系统/架构完整节点包，校验 SHA256，再安装；没有稳定版或对应目标包时明确失败，不切换渠道。

更新使用包内 `/usr/libexec/incus/node-install.sh <canary|latest> --update`，允许渠道切换安装较低版本，也允许重新安装同版本，不自动移除其它包。软件包安装期间界面不会用 Ctrl+C/Esc 杀掉 apt/dpkg，完整输出保存在本次操作日志；请勿关闭终端。更新前的连接凭据、存储和实例配置不被安装器重新生成。

成功后自动重启原本运行的 Incus 和连接器，保持原开机启用状态，原本停止的服务保持停止，然后重新打开新版 TUI。必须安排维护窗口：现有 Incus unit 停止动作会执行节点 shutdown，实例后续启动由既有启动策略决定；启用节点安全模式时仍可能需要在面板批准恢复。安装或重启失败会保留实际错误和已完成变更，不自动回滚软件包。

服务管理中的 Incus 启动、重启和启用操作会等待真实节点 API 就绪（最多一分钟，外层取消仍生效），再显示操作完成；systemctl 的 active 状态本身不代表 socket 已创建。旧版 TUI 可能在重启后立即显示 socket 不存在，等待后主动刷新可确认最新状态；持续失败时应查看本次启动日志。

如果是通过安装脚本/apt 升级，旧进程可能仍在运行；磁盘文件已更新不等于服务已应用。普通安装器会提示已有运行服务需要重启，TUI 摘要会识别 `/proc/<MainPID>/exe` 的旧二进制已被替换状态。选择“更新 → 应用已安装更新（无需下载）”，维护确认后只读检查 ID 映射、重启原本运行的服务并重新打开 TUI，不重复下载或安装。已有版本没有该菜单时，可在“更多 → Incus 服务管理 → 重启”应用 Incus 更新；同样需安排维护窗口。

此入口更新已经使用本集成包的节点，不迁移独立旧 Agent。新功能需发布包含更新菜单及包内安装器的新包后才能使用；本地源码完成不等于已发布。常规安装命令保持原行为，只有 TUI 更新流程负责上述自动重启。

## 首次设置

1. 执行 `sudo incus-tui`，选择“一键初始化”。确认后启用并启动 Incus，等待本地 API 就绪；为无实例的非集群节点创建 `default`（dir 驱动）和 `incusbr0`（固定 `10.10.0.1/22` + NAT/DHCP、新桥 IPv6 禁用），给 default profile 添加根磁盘和 eth0。匹配的基础资源允许复用以继续部分失败的初始化；其它池、其它 profile 配置、任意项目已有实例时拒绝覆盖。

TUI 的搭建、配置命令与本地 API 响应会完整实时输出到终端，并保存到界面显示的 `/var/log/incus-tui/session-*.log`。HTTP/异步 operation 失败保留原始错误；敏感凭据遮蔽，其他输出不截断。Panel 地址可使用 HTTP 或 HTTPS，由管理员自行选择；一次性凭据请求不跟随重定向。
2. 在 Panel 创建或选择宿主机，生成一次性 `ait_` token。它通常在 30 分钟内有效且只能消费一次；粘贴 token 本身，不粘贴整条 shell 安装命令。
3. 选择“面板接入”，确认 HTTP/HTTPS 根地址与 token。TUI 自动启动 Incus 并等待 API，检查 `incusbr0` 与固定 IPv4 地址池：桥缺失时创建，已有不兼容桥时在消费 token 前停止并提示修复。随后兑换连接凭据和 Panel **公钥证书**、验证指纹、导入信任、设置 `127.0.0.1:8443` 回环监听、原子保存 `/etc/incudal-agent/config.yaml`（0600），再启用并重启连接器。既有其它监听不会被覆盖，既有证书不会被删除；不传输私钥。需要配套控制端的 JSON 接入响应。
4. 在 Panel 验证节点连接。手填 ID/密钥、手动导入证书和监听检查/修复都位于“高级维护”，用于诊断和维护；正常 token 接入无需分别执行。Agent 心跳与 mTLS 管理连接仍是不同的验证证据。

需要定制存储/ZFS/桥接网络/集群时，使用官方 `incus admin init`；TUI 的一键初始化是固定的基础配置，不自动迁移已有资源。dir 驱动的配额与快照能力有限，有存储配额需求时应选择经过验证的 ZFS/Btrfs 等方案。初始化失败会保留步骤与原始错误；相匹配的基础资源可重试补齐，已有实例的桥问题使用下述修复菜单。

## 修复网桥

先在面板开启节点维护，暂停新实例交付，再选择“修复 → 修复网桥”。确认后备份配置，停止受影响的持久 LXC，删除旧 incusbr0 并重建 `10.10.0.1/22` NAT 网络，恢复网卡/profile，最后仅启动原本运行的容器。有效且不冲突的 IP、MAC、磁盘与限速保留；缺失/冲突地址重新分配，映射会输出到日志。变更 IP 后须核对面板登记，心跳不会自动更新 IPAM。

原配置和阶段保存在 `/var/lib/incus-tui/repairs/bridge-*/state.json`（0600，可能包含容器敏感配置）。失败或取消保留原始原因和已完成步骤，不自动回滚，也不继续启动未完成的容器；按备份检查后再处理。网络级 forward、外部网卡/FAN、集群、VM 占用、运行中的临时容器或无法确认的引用会拒绝自动修复。已配置的 IPv6 保留；来宾手工静态配置不被直接编辑，修改已有 cloud-init 网络配置可能触发镜像重新执行 cloud-init。

## 运维

TUI 支持 Incus 和连接 Agent 的启动、停止、重启、启用并启动、关闭开机启动；所有写操作需界面内确认。查看日志使用 `journalctl -u incus` / `incudal-agent -n 100 -f`，退出日志后返回面板。服务操作最多等待三分钟，Esc 可取消等待；已经提交给 systemd 或 Incus 的变更不会自动回滚。Incus 停止命令可能由 systemd 在后台继续处理。

Incus 本地服务仅依赖 `network.target`，不因 DNS 的 `wait-online` 超时阻塞本地 socket；连接器仍等待网络在线。已有自定义 systemd 覆盖配置保持有效，需要自行检查。

## 非特权容器的 UID/GID 映射

节点包在安装/升级时，通过系统 `usermod` 准备 root 的 `/etc/subuid`、`/etc/subgid` 映射，避免非特权容器报 `System doesn't have a functional idmap setup`。每次 Incus 启动前只执行 `/usr/libexec/incus/prepare-idmap --check`，不会改写系统配置。干净系统采用官方默认 `root:1000000:1000000000`，已有有效分配原样保留；缺失条目补齐前检查冲突并备份原文件，发生冲突时保留配置并输出具体错误。不会修改已有容器文件所有权或改用特权容器。如果旧 incusd 已在缺少映射时启动，必须在维护窗口更新并重启 Incus，使其重新读取；只写文件不会刷新正在运行的 daemon。TUI 更新可按现有流程重启原本运行的服务，可能停止实例。

## 可选 ZFS 与节点管理

默认初始化仍使用 DIR。进入 `incus-tui` 的“ZFS 管理”子菜单，可安装检查依赖、创建专用磁盘或测试文件池、创建/注册加密数据集、解锁、绑定 Incus 和查看池状态。不会自动挑盘、迁移或覆盖已有数据。Debian 13 标准 deb822 源补 contrib 前会备份原文件；特殊内核、Secure Boot 和自定义源需管理员处理。

TUI 内 APT 使用 `DEBIAN_FRONTEND=noninteractive` 采用软件包默认配置，关闭 dpkg 伪终端和动态进度；OpenZFS 的 debconf 提示不会再等待隐藏的“确定”操作。apt-listchanges 使用文本输出，needrestart 只列出待重启服务。设置仅作用于 APT 子进程，不更改系统全局配置。安装和 DKMS 的全部 stdout/stderr、退出码及原始错误保留在操作日志；ANSI 终端控制序列完整清除，诊断文字保留。安装完成仍须通过 `modprobe zfs` 和 `zfs version` 才视为依赖就绪，非交互安装不能解决内核不兼容或编译失败。

依赖安装先检查运行内核的 ZFS 模块；已有模块时不安装 DKMS/headers。没有模块时，必须已有匹配内核的构建目录或 APT 中存在准确的 headers 包，否则停止并显示完整检查错误。OrbStack 使用定制内核，Debian 通用 headers 不能替代；其 `/sys/kernel/kheaders.tar.xz` 不是 TUI 已配置好的 DKMS 构建环境。TUI 不自动构建 OrbStack 自定义模块，也不更换内核；需管理员提供匹配环境或改用具备 ZFS 支持的标准内核虚拟机。限制说明：[OrbStack Linux machines](https://docs.orbstack.dev/machines/)。

创建根之前，将 TUI 显示的随机 hex 密钥保存到配套控制端，复制 SHA256 指纹回 TUI 核对。节点不主动保存明文密钥文件；终端录屏、剪贴板、swap/core dump 仍由管理员管理。可信终端中的 TUI 状态区同时提供节点服务端证书 SHA256，控制端需先人工核对保存，才允许新的管理/发钥链路连接。

安全模式默认关闭，DIR 节点也可在控制端开启。触发后全部 LXC 停止、API 持续锁定，ZFS 按注册情况卸载/清钥；Agent 与 incusd 保持通信。TUI 加载密钥不会批准节点恢复，须控制端授权后自动恢复锁定前运行的容器。重启、重连、关闭安全模式均不清除已有锁定。详情与限制见根目录 FEATURES.md 及配套 PayIncus 仓库 `docs/NODE_SECURITY.md`。

控制端不再分发独立 Agent。由控制端安装命令传入的 Panel 地址和一次性 token 会在 TUI 中预填并隐藏 token，确认后才兑换；无终端时稍后手工配置。此接入流程不提供独立旧 Agent 的升级迁移，已使用本集成包的节点更新见上文。
