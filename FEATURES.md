# PayIncus 定制功能记录

## 上游 LTS 基线

| 项目 | 当前值 |
| --- | --- |
| 官方仓库 | https://github.com/lxc/incus |
| 维护系列 | 7.0 LTS |
| 正式发布 tag | `v7.0.2` |
| 上游提交 | `01f81e5e607bbb5818410318e4b3496ad1820824` |
| 官方发布时间 | 2026-10-16 |
| 核对日期 | 2026-10-26 |

上游始终以正式 LTS 版本为准，仅同步 LTS 维护版本及经过兼容性验证的新 LTS 系列。普通功能版、开发分支及未发版提交均不纳入基线或同步目标。定制提交建立在记录的 LTS 基线上。

## 节点 API 与精简连接 Agent

### 目的与行为

将原 PayIncus 宿主 Agent 的网络控制、资源采集、Caddy 运维和节点日志采集迁入 incusd 内部独立模块。Agent 保留 HMAC 心跳、反向 WebSocket 连接、认证与字节/日志转发；实例操作继续使用未经改写的原生 Incus API。官方 `cmd/incus-agent` 仍为虚拟机内部 Agent，未作修改。

代码位于 `internal/server/payincus/`；宿主连接程序为 `cmd/payincus-agent/`。导入源为 Rain-kl/payincus 提交 `76b78bf9e656128f8bcab2cc9434443f0368a6ca`，BSD-3-Clause 版权与声明保留在该目录。Xray 的 MPL-2.0 声明及固定版本校验信息保留在 `egress/assets/`。

### 上游接入点与安全边界

- 唯一既有上游运行时文件改动：`cmd/incusd/api.go` 调用 `registerPayIncus`，附近有 `PayIncus:` 注释。
- 新增 `cmd/incusd/payincus.go` 使用原有 `Daemon.createCmd`、`APIEndpoint`、`allowPermission`、`response.ManualResponse` 注册独立 API 子树。
- 全部扩展接口要求 Incus 认证及服务器级 `can_edit` 权限，包括只读的全节点指标和日志。连接认证不能代替 Incus 请求授权，不开放未经认证的扩展接口。
- 通过 `Daemon.setupChan` 等待 API 基本就绪，再启动网络恢复后的定期协调；通过 `Daemon.shutdownCtx` 取消协调、控制请求和日志流。工作线程 panic 被记录并终止该线程，节点应通过状态检测发现失败。
- 未修改上游 API 扩展列表、数据库模式和生成文件。定制能力通过 `GET /1.0/payincus` 的 `api_extensions` 探测，当前为 `payincus_node_v1`。
- 扩展是节点本地接口，明确拒绝 `target` 参数；集群调用者必须连接具体节点，不能假定上游会自动转发定制接口。

### API 与单元测试对应关系

所有普通响应使用 Incus 风格的 `type/status/status_code/metadata` 或 `type/error/error_code` 封装。以下写接口为有超时的同步接口，不冒充异步 operation；原生创建、启动等 API 继续使用 Incus 原有异步 operation。

| 接口 | 行为与限制 | 对应测试 |
| --- | --- | --- |
| `GET /1.0/payincus` | 返回扩展能力；默认禁用 | `service_test.go`、`nodeclient/client_test.go`、`cmd/incusd/payincus_test.go` |
| `GET /1.0/payincus/report?interval=60` | 采集宿主资源、实例状态、流量、GPU、Caddy 与网络状态；interval 为 30–3600；单并发 | `service_test.go`、`report/report_test.go`、`report/gpu_test.go`、`report/incus_api_test.go` |
| `GET /1.0/payincus/network` | 仅返回已应用状态，不返回代理凭据 | `service_test.go` |
| `PUT /1.0/payincus/network` | 沿用现有 revision 与 schemaVersion 数据结构；v2 含套餐出口；95 秒上限 | `service_test.go`、`egress/egress_test.go`、`egress/groups_test.go`、`nodeclient/client_test.go` |
| `POST /1.0/payincus/proxy-probes` | 固定参数代理探测；40 秒上限 | `service_test.go`、`nodeclient/client_test.go`、`egress/resolver_test.go` |
| `POST /1.0/payincus/caddy` | 仅 install/uninstall；互斥；15 分钟上限 | `service_test.go` |
| `GET /1.0/payincus/logs?instance=NAME` | 实例作用域 journal/QEMU 日志，SSE；名称校验、32 流并发、15 分钟及行数/长度限制 | `service_test.go`、`diagnostics/task_logs_test.go`、`nodeclient/client_test.go` |
| `GET /1.0/payincus/journal?unit=incudal-agent&lines=100` | 仅 incus/incudal-agent 两个服务；1–1000 行历史，继续跟随；共享流限制 | `service_test.go`、`nodeclient/client_test.go`（含正常 SSE 与非法输入）；系统 journal 实测另见验收限制 |

认证接线测试 `cmd/incusd/payincus_test.go` 启动测试 Daemon，验证可信 Unix socket 请求成功及未认证 HTTP 请求被拒绝。`nodeclient/client_test.go` 覆盖 Unix socket、能力缺失、拒绝响应、格式变动、断线取消和 SSE 资源释放。

### 对上游 API 的依赖

网络与采集模块调用本地 Incus Unix socket，遵循 `INCUS_DIR`（默认 `/var/lib/incus`）；不连接 LXD 或 snap 兼容路径。网络操作在 inventory 阶段先探测 `network_acl` 和 `network_bridge_acl_devices`，能力缺失即停止。

| 上游接口 | 用途 | 契约测试 |
| --- | --- | --- |
| `GET /1.0` | 网络 ACL 能力检查 | `egress/incus_api_test.go` |
| `GET /1.0/networks?recursion=1` | 网络清单 | `egress/incus_api_test.go` |
| `GET /1.0/instances?recursion=1` | 网络清单、实例报告 | `egress/incus_api_test.go`、`report/incus_api_test.go` |
| `GET /1.0/instances/{name}/state` | 状态及精确整数流量计数 | `report/incus_api_test.go` |
| `GET /1.0/network-acls/{name}` | 确认 ACL 归属 | `egress/incus_api_test.go` |
| `POST /1.0/network-acls` | 创建托管 ACL | `egress/incus_api_test.go` |
| `PUT /1.0/network-acls/{name}` | 更新托管 ACL | `egress/incus_api_test.go` |
| `PATCH /1.0/networks/{name}` | ACL 与 DNS 配置 | `egress/incus_api_test.go` |
| `PATCH /1.0/instances/{name}` | NIC ACL 配置 | `egress/incus_api_test.go` |
| `GET /1.0/operations/{id}/wait?timeout=10` | 等待写操作；处理 Pending/Running/Cancelling、失败及取消 | `egress/incus_api_test.go` |

与旧实现相比，异步等待不再将第一次长轮询仍在运行的操作直接判为失败；错误及未知响应类型会被拒绝。指标采集拒绝非 sync 响应，并测试大于 JavaScript 安全整数的流量计数不丢失精度。

### 控制端兼容

配套 Rain-kl/payincus 改动位于 `server/src/lib/incus/payincus-node-api.ts` 与 `tunnel-manager.ts`。新节点通告 `payincus_node_v1` 后，控制端经原有 mTLS IncusClient 调用上述扩展，JSON 参数不做业务结构转换；能力再次通过实际 API 校验。

旧节点保留原控制帧协议，新节点连接程序也保留控制帧到本地 API 的短期兼容转发，支持滚动升级。此适配层不包含网络、采集或 Caddy 实现。配套测试为 `server/scripts/test-payincus-node-api.ts` 与 `test-agent-tunnel-protocol-guards.ts`，覆盖请求映射、能力缺失、错误传播、断线取消及旧协议回归。

### 启动恢复、打包和升级

- `cmd/payincus-network-worker/` 是启动前恢复与网络命名空间探测的小工具。网络模块不再复制或以 Agent 旗标执行 incusd 本体，而是优先使用已复制的恢复工具自身或 incusd 同目录工具，再通过 PATH 找到该工具，继续复制为持久化的 `network-worker`。
- 保留原有状态路径 `/var/lib/incudal-agent/network`、Incudal nftables 所有权及 systemd 启动保护，避免迁移时丢失策略或启动顺序反转。恢复工具不能依赖一个尚未启动的 incusd API。
- 默认不开启扩展；定制软件包为 incus.service 设置 `INCUS_PAYINCUS=1`。模板在 `deploy/payincus/`，使用 `/usr/libexec/incus/` 布局；其他发行版必须按自身原生打包路径调整。
- `scripts/payincus/build.sh` 准备并校验固定 Xray 发布包，再构建定制 incusd、CLI、官方 Guest Agent、incus-user、连接 Agent 和恢复工具。仅暂存二进制及许可声明，不是完整发行版安装包；必须保留 `doc/packaging.md` 要求的依赖、服务、socket、组和辅助文件。
- 连接 Agent 使用原 `/etc/incudal-agent/config.yaml` 配置，保持 `incudal-agent.service` 名称，替换旧服务时必须停止旧进程，禁止两个网络执行器同时运行。
- 连接 Agent 不再执行旧的二进制自更新指令；节点升级必须由定制 Incus 软件包完成，不能把心跳自更新变成远程替换 incusd。

### 连接与认证接口测试

`panel/client_test.go` 覆盖 `/api/agent/heartbeat` 的方法、路径、身份、HMAC 签名、连接指令解析、拒绝及取消；`protocol/signing_test.go` 覆盖规范化与签名；`tunnel/control_test.go` 和 `tunnel/tunnel_test.go` 覆盖反向连接认证、并发复用、取消及旧协议兼容。

### 上游同步核对事项

同步 LTS 时复核 `createCmd` 的认证授权与请求限制、setup/shutdown 生命周期、ManualResponse 对 SSE 的处理、上述 REST 契约和 API extensions、原生软件包路径以及自动启动前的网络保护。若注册入口发生冲突，优先调整新增适配文件，保持上游 api.go 仅有注册调用，不扩散定制逻辑。

### 验证与验收范围

- 定制模块与两个宿主工具的 `go test -p 2 -race` 通过，包含固定 Xray amd64 payload 的配置及本地代理测试；生成 payload 未纳入 Git。
- 隔离 Debian 容器中，完整 incusd 编译通过；`go test -tags libsqlite3 ./cmd/incusd -run TestPayIncus` 通过，验证真实 Daemon 路由及认证边界。
- 定制 incusd、精简 Agent 与恢复工具二进制构建通过；固定 Xray v26.3.27 下载包和可执行文件的双重 SHA-256 校验通过。
- 新模块及两个宿主工具的 golangci-lint v2.15.0 检查为零问题；上游单行赋值、块后换行、混合缩进、尾随空格守卫通过。构建脚本 Bash 语法、Xray 准备脚本 Python 语法及 Git whitespace 检查通过。
- 全仓 `go test -p 2 -tags libsqlite3 -v ./...`：63 个含测试包通过、113 个包无测试，2 个包失败。未修改的 v7.0.2 在同一隔离环境复现相同失败：`cmd/lxc-to-incus` 的 `TestConvertNetworkConfig` 无法创建测试容器；`internal/server/sys` 的 `TestHaveMacAdmin` 受 AppArmor 权限环境限制。
- 控制端类型检查、原生 API 契约测试、隧道新旧协议测试、package-egress、egress、agent-auth、响应脱敏守卫及根目录 `make check` 通过；未运行需独立数据库的 opt-in 出口测试，本轮未改数据库结构或资源分配逻辑。
- 全仓 `make static-analysis` 已运行：全仓 golangci-lint 为零问题，许可证阶段被上游现有依赖 `github.com/donovanhide/eventsource` 缺少可识别许可证文件阻断；未运行该阶段之后的生成文件检查。旧版 go-licenses 还存在 Go 工具链识别问题，改用 v2 后确认上述依赖阻断，未改动上游依赖或补造许可声明。

未部署到生产节点。完整的真实容器创建、代理数据路径、重启恢复及软件包升级验收仍须在隔离节点执行；之前 PayIncus Agent 的通过记录不自动算作定制 incusd 的验收记录。

## 上游同步记录

| 日期 | 基线 | 变更 |
| --- | --- | --- |
| 2026-10-23 | v7.0.2 LTS | 本地切换到正式 tag；增加规则与独立定制模块，未使用未发版上游代码。 |

未配置自动 LTS release 检查或合并工作流；后续仅按 AGENTS.md 同步正式 LTS。

## 集成 CI 与预发布

- `.github/workflows/payincus.yml` 在 fork 的 main/canary、PR 和 `payincus-v*` tag 上运行；使用 GitHub 托管的 Ubuntu 24.04 amd64/arm64 原生 runner，Go 版本读取 LTS go.mod。
- CI 校验 LTS tag 与提交，准备固定 Xray，运行定制模块竞态测试、真实 daemon 接线及全部 daemon 单元测试、上游 CLI/shared 测试，再构建全部集成二进制并打包。原生完整宿主测试仍需适当内核环境，不能用构建通过代替真实节点验收。
- 上游 `.github/workflows/build.yml`、`tests.yml`、`release.yml` 各 job 增加带注释的官方仓库条件；保留上游定义，由 fork 专用流程承担集成测试与服务端发布，避免等待仅上游有的自托管 runner 或误发布客户端包。
- `scripts/payincus/package.sh` 生成 Ubuntu 24.04 的 amd64/arm64 deb、rootfs 归档、带源提交/LTS 基线/API 能力的清单和 SHA256SUMS。deb 依赖通过 dpkg-shlibdeps 计算；包含三个独立服务、官方 Guest Agent、恢复工具及许可声明。可选的 socket 激活不纳入此包，VM 的 QEMU/UEFI 为额外安装项。
- 原生包与官方 incus/incus-base/incus-client 声明冲突及替代，安装前必须安排迁移维护窗口；postinst 只创建权限组并重载 systemd，不自动启用、启动或重启服务。不会通过旧 Agent OTA 替换 incusd。
- `payincus-v7.0.2-rc.N` tag 在两个架构测试/构建成功后发布 GitHub prerelease，包含两架构产物与校验；普通分支仅上传 Actions artifacts。测试标签不改变记录的上游 LTS 版本。
- 远程 fork 原 main 含未发版上游历史；集成分支保留该历史，合并时保持已验证的 v7.0.2 LTS 源码树及定制实现，禁止将远程 main 的未发版文件带入交付。
- CI 与发布结果以对应 GitHub Actions run 和 Release 的实际状态为准；在结果确认前不记为通过。

本地新增发布检查：Actionlint 验证 fork 集成工作流通过；ShellCheck、Bash 语法和完整 amd64 二进制/原生 deb/归档打包通过，SHA256SUMS 逐项校验通过。本地打包环境为 Debian 13，仅用于验证脚本，正式 Ubuntu 24.04 双架构产物以 Actions 构建为准，不上传本地临时产物。

CI 接线修正：fork 初始没有官方 release tag，校验步骤从官方仓库显式获取 v7.0.2 tag 并核对固定 SHA，不以 fork 自有 tag 代替上游发布依据。新增干净 Ubuntu 24.04 容器安装 deb、解析原生依赖、检查 CLI/daemon/连接器入口及权限组的验收步骤，安装成功后才上传产物。

发布归档将 rootfs 的 UID/GID 固定为 0，避免把构建 runner 的文件所有者带到节点；deb 使用 dpkg-deb 的 root-owner-group。

### canary 自动集成发布

新增 `.github/workflows/payincus-publish.yml`，在同仓库 canary 的 push 集成工作流成功后，下载该 run 的双架构产物，逐项核对 SHA256、架构、源提交及固定 LTS 基线，再发布唯一的 `integration-7.0.2-<提交前12位>` prerelease。发布目标为已验证的准确提交，发布说明附 CI 链接。PR、其他仓库、其他分支或失败 run 不可触发发布；工作流只读取受信任 push 的说明文件，不执行下载产物中的脚本。版本化 RC tag 发布流程保留。此流程将持续集成与发布分离，不依赖本地手工创建 tag；不触及生产环境。

CI 权限隔离：daemon 测试先由普通 runner 编译测试程序，再以 root 执行该程序；Go 下载与编译不使用 root，避免后续 CLI/shared 测试遇到 root 创建的 0600 模块缓存锁。不跳过失败用例，不修改上游 API 行为。

## 一键安装与 incus-tui（2026-10-23）

### 行为与边界

- 新增独立 Bubble Tea v1.3.10 TUI，入口 `tools/incus-tui/`。使用单独 Go module 固定依赖，不改上游 `go.mod` / `go.sum` 或 daemon；定制安装包将命令安装为 `/usr/bin/incus-tui`，依赖许可收集在 `NOTICE.tui`，保留 README 中声明的 MIT 许可及作者。
- `scripts/payincus/install.sh [latest|canary] [--no-tui]` 仅检测 OS/架构、查询 Rain-kl/incus GitHub release、下载匹配 deb 及 SHA256、通过 apt 安装，然后交接 TUI。缺少 curl/Python 时通过 apt 补齐安装器依赖。支持 Debian 13、Ubuntu 24.04 的 amd64/arm64，其他目标明确拒绝，不使用不匹配的 Ubuntu 包。
- `latest` 只接受正式 `payincus-vX.Y.Z` release，`canary` 只接受集成 prerelease；无包、错误平台、无校验文件、校验失败均不安装，不跨渠道或回退旧包。canary 搜索最近 100 个 release。旧发布的通用包名不再被新安装器选中。
- 包名增加 `linux-<ID>-<VERSION_ID>-<arch>`，manifest 新增 `targetDistribution`、实际 channel，Debian 13 使用 Debian 原生依赖构建及安装验证。稳定 tag `payincus-v7.0.2` 发布稳定版并标 latest，RC 继续 prerelease，canary 仍只发布已验证 push 的唯一集成 tag；上游基线仍为官方 v7.0.2 LTS，不改为普通功能版。
- TUI 提供 Panel 凭据配置及一次性 token 兑换、原子 0600 配置保存、连接服务启动、服务生命周期、实时 journal、固定基础初始化、mTLS 公钥证书指纹确认及本地隧道监听。仅写入确认过的配置，不运行旧 Agent 安装器，不在终端或错误信息显示 token/密钥；已有旧连接服务覆盖时在兑换前停止并给出诊断。
- 一键初始化只面向全新非集群节点：dir 存储、IPv4 NAT 网桥、default profile；拒绝覆盖已有资源，失败不自动删除资源，部分写入需人工检查。内核、KVM、AppArmor、外层嵌套权限及 UID/GID 映射不由安装器/TUI 自动修改。Agent 在线与实例管理的 mTLS 验证分开。
- 仅调整本 fork 自有 `deploy/payincus/incus.service`，改为 `network.target`，避免 DNS wait-online 阻塞本地 daemon；连接器的网络等待保持原有行为。包安装仍不自动启动/重启服务，避免升级时直接影响运行中的节点。

### 上游/API 接入与测试

该轮功能没有新增上游源码修改。其本地 CLI 接线在下文 2026-10-26 节点功能中已改为集中 Unix API 适配；管理员默认 remote 不参与本地操作。子进程无 shell 拼接，可取消。HTTP 兑换 20 秒超时，禁止重定向、限制响应大小、不自动重试一次性 token。

| 接口 | 接入方式与兼容性 | 单元测试 |
| --- | --- | --- |
| GitHub releases/latest、releases、release assets | 按 release 状态/tag、系统/架构选择匹配包；HTTPS、同发布 SHA256，无其他来源 fallback | `scripts/payincus/test-install.py` 执行真实安装器，mock OS/下载/apt；覆盖两渠道×两系统×双架构、损坏包、无包、非法 URL、draft、错误架构/系统/渠道 |
| `GET /api/agent/install-config/:token` | 与当前 PayIncus 两行单引号 shell assignment 契约一致；仅解析数据，严禁 source/eval；HMAC 凭据供原连接器使用 | `TestPanelTokenProtocol`：真实 TLS HTTP fixture，路径/方法、权限/过期/服务错误、格式变化、重复字段、注入、体积、重定向、取消、错误脱敏 |
| `GET /1.0/payincus` | 经 CLI query 读取 metadata，探测 `payincus_node_v1`；不修改扩展清单 | `TestStatusCapabilityAndFailure` |
| `GET /1.0`、storage-pools、instances?all-projects=true、networks?recursion=1、profiles/default | v7.0.2 真实 metadata 契约；确认就绪、非集群、无现有资源，拒绝格式变化 | `TestInitializeContract`、`TestInitializeRefusesExistingAndMalformedState`、`TestInitializePropagatesInterfaceErrors` |
| `incus admin init --preseed` | 复用正式 LTS 的 preseed/异步 operation 等待与错误处理；TUI 不重写 operation 实现，不重试写请求 | 同上，验证 preseed、参数、失败传播及不覆盖已有状态 |
| `incus config get/set core.https_address` | 仅设置空/相同监听，不覆盖其他监听 | `TestServicesAndListenerContracts` |
| `incus config trust add-certificate` | 校验单个 PEM X.509、公钥证书有效期与 SHA256；私有临时文件、完成后清理，不删除其他 trust | `TestCertificateTrustContract` |
| systemctl show/start/stop/restart/enable/disable、journalctl | 固定服务白名单；show 检查连接器实际启动入口，journal 交回终端进行实时跟随 | `TestServicesAndListenerContracts`、`TestConnectorPreflightAndConnection`、`TestStatusCapabilityAndFailure`、`TestJournalAndOperationCancellation`、UI 测试及 PTY 验证 |
| 本地配置写入、交互确认 | 原子 rename、0600，掩码输入，不在确认前执行写操作 | `TestAtomicPrivateConfiguration`、`TestUIRequiresConfirmationAndMasksSecrets`、`TestUIServiceActionsAndFailure`、`TestRunnerCancellationAndFailure` |

操作说明见 `scripts/payincus/INSTALL.md`。上游 LTS 同步时复核 CLI query 的 metadata 输出、force-local、preseed 字段/operation 错误语义、profiles/实例/网络 API 与 server_clustered 字段；Panel 升级时复核 token assignment 契约及 systemd 服务路径。未修改 Incus 数据库和原生 API，未部署生产。

### 验证记录

本地验证通过：

- 隔离 TUI module 的 `go test -race ./...`、`go vet ./...`、按根仓配置运行的 golangci-lint（零问题）；amd64 静态构建及 arm64 交叉编译。
- 真实安装脚本的 fixture 测试，覆盖两渠道、两系统、双架构，以及校验/元数据/平台/渠道错误时不执行 apt 安装；Bash 语法、ShellCheck、两份发布 workflow 的 actionlint。
- Debian 13 amd64 原生环境完整执行 `build.sh`，编译所有 daemon/CLI/guest/connector/network-worker/TUI；执行新格式集成 deb/归档/manifest 打包；正式 `payincus-v7.0.2` 打包验证 stable channel、原生版本号及 SHA256。
- 全新 Debian 13 容器通过 apt 安装本地新 deb，验证 TUI、CLI、daemon、连接 Agent 入口、权限组、操作说明和依赖许可文件。
- 真实 PTY 验证菜单导航、初始化确认与取消、密钥掩码、退出恢复终端；journal 子进程以 fixture 测试 Ctrl+C 后返回面板，未连接真实宿主 journal 或生产节点。

以上本地产物来自开发工作树，manifest 标记 dirty，不上传为正式 release。推送后的四目标 CI 负责验证 Ubuntu 24.04 / Debian 13 的 amd64 / arm64 原生包安装，通过后发布对应提交的 canary 集成包；结果以该提交 Actions run 与 release 为准。实际宿主初始化/容器创建与 Panel 联通仍待节点验收。之前已记录的上游全仓静态/内核测试限制继续适用，本轮未重复运行无关的完整上游测试。

## 节点 CLI 诱捕、持续通信与控制端恢复（2026-10-26）

### 行为与边界

- 可选扩展 `payincus_security_v1`，由定制包 `INCUS_PAYINCUS=1` 启用。关键命令诱捕默认关闭，不要求安装 ZFS，DIR 节点同样可启用。所有受管本机 Linux CLI 调用均提交脱敏分类、UID/PID、时间与诱捕判定；不保存参数值、stdin、密码或密钥。非 Linux CLI 保留原行为。
- exec/shell/console、file、导出/发布/复制/移动、配置写入及启动/恢复等关键命令在执行前触发；普通 list/info/help/version 和明确的只读元数据子命令只审计。`info --show-log` 也属于敏感访问。未知命令、别名及外部别名按敏感调用处理，别名展开前再次检查；审计不覆盖被替换的二进制或绕开 CLI 的宿主操作。
- 触发先原子持久化统一 `locked` 状态，再异步停止本节点全部 LXC 容器。已有认证授权之后的 API 检查覆盖 Unix、TLS 与反向隧道；启动驱动、启动任务与 ZFS 挂载入口另行检查，启动读锁确保正在启动的容器不会晚于停止核验完成才启动。
- 普通 CLI、受限接口及新扩展的失败统一返回 HTTP 500 / `Internal server error`，不包含审计或锁定原因，不伪造成功。专用管理状态/上报接口保留真实状态供控制端诊断；这不是对宿主 root 隐藏程序、状态文件或源码的保证。
- incusd、连接 Agent、状态/告警/恢复接口及 LXC 必需的 onstop/onstopns 回调继续运行；onstart 不放行。状态不是各接口独立副本；关闭诱捕、断网、重连和重启均不解除已存在的锁定。开启诱捕或存在注册加密存储的节点重启时，在存储导入与自动启动前进入锁定。
- 未配置 ZFS 时报告 `storage/not_configured`，只宣称 API 锁定与容器停止；不宣称明文 DIR 获得加密保护。存储占用、停止失败、卸载失败均独立上报，API 锁定继续保持。失败后控制端可再次发起锁定以重试。
- 恢复需要已有 Incus 管理权限的远程 mTLS 身份、当前 revision、已完成的容器停止核验及所有注册存储密钥可用。Unix socket 不允许修改策略或批准恢复。这里复用的是 Incus 信任管理身份，并不是独立的硬件控制端身份；持有其它受信任管理员私钥的人也有相同权限。恢复不自动拉起容器，管理员应在控制端确认存储就绪后再启动。
- 主机完整 root 仍可读取运行中明文/密钥、篡改程序、状态或信任数据库、直接挂载/访问容器与绕过此机制。本功能用于告警、限制常规操作与增加取数成本，不提供抵御恶意宿主的隔离保证；既有数据流和执行中的文件传输也不保证被瞬间撤回。

### 状态、告警与密钥链路

- 状态、跨进程锁和最多 2048 条本地事件位于 `$INCUS_DIR/payincus-security/`（默认 `/var/lib/incus`），原子替换、0600 文件、目录同步；损坏/不可读时 API 拒绝敏感操作。队列满时 CLI 返回普通内部错误，关键操作触发的锁定不回滚。
- `/payincus-audit.socket` 仅接收单个命令分类，使用内核 SO_PEERCRED 获取真实 UID/PID；原始参数在 CLI 内分类后即丢弃。socket 不提供恢复能力；可访问者可制造告警/拒绝服务，无法据此获得数据访问权。daemon 离线时 root CLI 可持久排队，但不清除已有锁定。
- 连接 Agent 每 2 秒检查状态/待发送事件，变化立即投递，未变化状态最多每 30 秒发送一次；每批最多 128 条。控制端 `POST /api/agent/security-report` 校验实际请求体 HMAC、时间戳及 nonce，数据库事务持久化/去重后才 ACK。断网保留事件重试；日志只输出通用投递失败。节点上报时控制端清理 30 天前事件（离线期间不主动清理），页面展示最近 100 条；告警进入管理日志。
- ZFS 仅注册独立子数据集加密根：AES-256-GCM、hex、`keylocation=prompt`。身份绑定 dataset GUID 和 SHA256 密钥指纹。创建使用随机 32 字节密钥，必须先由管理员保存控制端并在 TUI 输入相同指纹；节点以 stdin 将密钥交给 ZFS，不生成密钥文件。`load-key -n` 验证已加载状态下的密钥，依据 OpenZFS 正式手册契约。
- 告警时在停止核验后，按后代优先顺序卸载已挂载文件系统，执行 `zfs unload-key -r` 并核验根 `keystatus=unavailable`。不销毁池或数据。busy 会失败并保留锁定；LXC 外的进程/VM 占用及独立加密子根可能阻止卸载，需管理员处理，不能误报完全清钥。
- 加载密钥和告警动作互斥；TUI/控制端加载密钥不会清除 API 锁定。控制端批准恢复前检查所有注册根的身份和密钥状态；无需提供不存在的 DIR 密钥。保护现有数据须管理员自行迁移，不能原地把未加密 ZFS/DIR 变成加密。
- PayIncus 控制端配套使用现有 `ENCRYPTION_KEY` 经独立用途 HKDF 派生 AES-256-GCM 托管密钥，绑定 hostId/dataset/version；解密失败不兼容返回明文，不新增 KMS 技术栈。新管理链路必须先人工核对并保存节点服务端证书 SHA256，再使用指纹校验的 mTLS（直连及隧道均校验）；旧节点客户端的宽松证书处理不用于本链路。控制端迁移、权限、备份说明见配套仓库 `docs/NODE_SECURITY.md`。
- 管理员可在控制端领取原始密钥，经持久敏感操作审计后提供，再到 TUI 隐藏输入。原始密钥没有自动过期/即时撤销属性；已复制者需换钥才能撤销。本次没有实现换钥、托管主密钥在线轮换或自动迁移。
- “不存盘”指程序不主动写明文密钥文件或普通日志，不保证 Go/JS 内存副本、宿主 swap/core dump、终端录屏、剪贴板或外部运维软件没有副本；生产管理员需单独配置这些系统策略。控制端主密钥及数据库必须分别备份，丢失主密钥无法解密托管数据。

### TUI ZFS 管理与本地 API 适配

- 入口 `tools/incus-tui/{zfs,local_api}.go`。Debian 13 安装 zfsutils-linux/zfs-dkms/匹配内核 headers/util-linux，在标准 deb822 源添加 contrib 前独占备份原文件并保留 URI/签名配置；自定义 sources.list 由管理员配置。Ubuntu 24.04 优先系统模块，缺失时补 DKMS/headers。失败保留诊断，不绕过 Secure Boot 或更换内核。
- 显式确认后创建专用磁盘/分区池，或预分配测试文件池；拒绝已有文件系统/挂载，不使用 zpool `-f`，不自动挑盘。文件池不是生产推荐，失败时保留预分配文件供检查。池、加密根、Incus 绑定分步执行；默认一键初始化仍是 DIR，不自动迁移或修改已有实例配置。
- 创建加密根时在专用 TUI 页展示随机密钥，后续确认页不重复展示；解除存储时隐藏输入。TUI 状态提供本地 API 返回的服务端证书 SHA256，管理员通过可信终端核对后保存控制端。终端/剪贴板由管理员管理。
- TUI 的生产实现已改为 Unix socket API 适配，避免管理动作误触 CLI 诱捕。保留原 Runner fixture 接口；集中处理 ETag、原生请求、异步 Pending/Running/Success/Failure、格式错误与取消，不盲目重试写操作。此前安装器/TUI 记录中的 CLI 接线由此适配层替代。

### 接口、测试与上游同步核对

| 接口/接入点 | 测试与核对内容 |
| --- | --- |
| CLI 分类、持久状态、revision、队列/ACK、无 ZFS、启动与停止并发 | `internal/server/payincus/security/security_test.go`：`TestPersistentTripwireWithoutZFS`、`TestRestartCorruptionAndFailedStop`、`TestConcurrentEventsDoNotLoseLock`、`TestCommandPolicyAndNoArgumentLeaks`、`TestContainmentWaitsForInFlightStart` |
| GET `/1.0/payincus/security`、`/report`，POST `/policy`、`/lock`、`/recover`、`/ack` | 同文件 `TestHTTPAuthorityReplayAndFormats`：Unix 与远程授权、参数/格式、过期 revision、节点上报保活；`service_test.go/TestPublicNodeErrorsRemainGeneric` 验证实际扩展响应不泄露失败原因 |
| POST `/create`、`/register`、`/unlock`；ZFS create/load-key/unmount/unload-key/get | `TestStorageKeyLifecycleAndBusyFailure`、`TestStorageHTTPEnrollmentUnlockAndRecovery`、`storage_live_test.go/TestZFSNativeLifecycle`；stdin 交钥、指纹/GUID、已加载密钥验证、busy/错误/取消、互斥、恢复不自动清锁 |
| `/1.0/payincus` 子路径原生路由 | `cmd/incusd/payincus_test.go/TestPayIncusAuthenticatedRoute`：实际 daemon 子路由、扩展能力和未认证拒绝；修复此前 ServeMux 缺少末尾斜线子树导致子路由 404 的问题 |
| Agent HMAC 上报 | `internal/server/payincus/panel/client_test.go/TestSecurityReportSignedContract`：规范路径/请求体签名、ACK、错误与取消；控制端报告验证/事务/去重测试 |
| TUI 原生 `/1.0`、storage-pools、networks、profiles/default、certificates、operations/*/wait | `tools/incus-tui/local_api_test.go`：真实 LTS JSON、请求/ETag、Pending 到 Success、失败/异步错误/取消/格式变化；现有 `backend_test.go` 保留不覆盖已有资源等回归 |
| TUI ZFS 安装、确认、池创建及指纹展示 | 同文件与 `backend_test.go`：发行版、APT 备份、内核依赖、模块失败回退、危险请求拒绝、密钥只读展示/确认/隐藏 |

必要上游修改仅为：`cmd/incus/{main,main_aliases}.go` 的执行前观察；`cmd/incusd/daemon.go` 的认证后检查/启动前锁定/密钥请求禁用 debug body 捕获；`cmd/incusd/instances.go` 的启动任务检查；`internal/server/instance/drivers/driver_lxc.go` 的启动围栏；ZFS `driver_zfs.go` 的 Mount 和 `driver_zfs_volumes.go` 的 MountVolume 检查。各处保留 PayIncus 注释。上游 LTS 同步必须复核授权顺序、停止内部回调方法/路径、Start 签名与失败清理、池挂载重试、API JSON/ETag/异步 operation 及别名执行顺序，避免恢复时重放旧启动任务。安全模块和 TUI 独立文件承担实现；不修改上游数据库格式或依赖模块。

### 本地验证记录

- 扩展模块/连接 Agent race 测试、TUI race 与 lint、前后端类型检查及双端构建已完成；详细结果以本次最终测试记录为准。全量上游静态检查既有许可和内核限制仍按前文记录，不将其算作通过。
- 在隔离 Debian 13 中编译真实 daemon/CLI，运行 daemon 测试；实际启动 LXC fixture，验证关键 CLI 返回通用错误、容器停止、锁定期间文件 API 拒绝、报告保活、控制端 mTLS 恢复、关闭诱捕不清锁、重启保持锁定且不延迟自动启动。
- 配套控制端使用独立 PostgreSQL 测试库与真实 incusd 联通，验证指纹、策略、手动锁定、停止核验、恢复授权、密钥加密入库/领取；测试中没有连接生产。
- 在独立 QEMU TCG 虚拟机中运行 Debian 13 6.12.113 内核与 OpenZFS 2.3.9，使用预分配匿名文件 vdev，真实验证 AES 创建、已加载密钥验证、错误密钥、busy 卸载拒绝、数据卸载不可见、密钥加载与 API 恢复分离、恢复后内容完整，以及文件池 export/import-N/加载的回归。`scripts/payincus/test-zfs-vm.sh` 将该测试接入 Debian amd64 CI；仅 guest 内加载模块，不接触宿主磁盘。磁盘池、Ubuntu 内核/Secure Boot、真实服务升级仍需相应目标验收，不将本地文件 vdev 当作这些场景的证据。
- 真实 PTY 验证新增 ZFS 密钥创建页、已有加密根注册、取消与终端恢复；捕获的测试密钥仅在内存中校验，不保存终端录屏。真实 daemon debug 模式下确认拒绝的密钥请求仍返回通用错误，日志不包含提交的密钥。
- 新增脚本 ShellCheck、workflow actionlint 及改动范围 native lint 通过。原全仓静态/内核测试限制仍适用。发布、推送、CI run 和生产部署需独立记录，以对应提交的 Actions 与 Release 为准；本地验收未操作生产。

## 节点程序统一归属 Incus，移除独立 Agent 链路（2026-10-26）

- 配套 PayIncus 控制端已移除 `agent/` 源码、独立 Agent 编译/发布、binary/manifest 代理、自更新 API 与旧业务 RPC 回退；控制端镜像不再包含 Agent 构建产物。此前记录的新旧兼容过渡方案由本条替代，不新增升级迁移程序、旧配置搬运或历史协议兼容层。
- 唯一宿主连接器为本仓库 `cmd/payincus-agent`，随 incus-payincus deb 管理。删除 WebSocket 业务 RPC 执行器、局部方法转换适配和心跳独立升级指令；保留原生 API 字节转发、`payincus_node_v1` 能力握手和节点日志转发。控制端通过原生 mTLS API 执行业务操作，旧 RPC 不再可用。
- 心跳始终携带 `nodeApi=payincus_node_v1` 及能力声明，包含告警锁定时的最小报告；保留正常报告的资源能力。控制端拒绝缺失原生节点声明的报告，不触发旧节点更新。
- 控制端安装入口仅下载并执行本仓库 `scripts/payincus/install.sh latest|canary`。安装脚本职责仍仅为选择正式 release 包、校验、apt 安装和进入 TUI。Panel 地址与一次性 token 从环境交给 TUI，字段掩码显示并在进入界面时移除 token 环境变量，用户确认后才兑换；没有交互终端时由管理员稍后手工配置。服务 unit 必须匹配软件包提供的连接器；不接管、迁移或修复旧服务配置。
- 适用平台仍为 Debian 13 / Ubuntu 24.04 的 amd64、arm64。面向全新节点；旧节点不提供原地升级、数据迁移、配置继承或协议回退。节点凭据接口沿用同一 HMAC 身份契约，不修改现有上游代码、数据库或依赖。
- 测试：`cmd/payincus-agent/main_test.go/TestNativeHeartbeatContractIncludesLockedNodes` 真实 Unix/HTTP 验证普通与锁定心跳、能力保留；`tunnel/control_test.go/TestNativeIncusStreamAndRejectedRPC` 真实签名 WebSocket 验证握手、字节中继、旧 RPC 不执行及断线取消；`nodeclient/client_test.go` 直接验证原生 HTTP 请求；`tools/incus-tui/main_test.go/TestInstallerTokenPrefillIsMaskedAndRemovedFromEnvironment` 验证交接不暴露 token。控制端另有分发边界、安装脚本双渠道、HMAC、超时/取消与并发控制契约测试。
- 上游 API/扩展仍为 7.0.2 LTS 原生 API 和 `payincus_node_v1`，无新增上游修改点。后续同步核对 HELLO 帧、原生请求及 TUI 初始化/凭据确认行为。移除的旧 RPC 测试由原生传输与拒绝旧 RPC 的契约测试替代；不把删除兼容代码视为旧节点迁移验收。
