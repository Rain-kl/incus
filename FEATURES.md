# PayIncus 定制功能记录

## 上游 LTS 基线

| 项目 | 当前值 |
| --- | --- |
| 官方仓库 | https://github.com/lxc/incus |
| 维护系列 | 7.0 LTS |
| 正式发布 tag | `v7.0.2` |
| 上游提交 | `01f81e5e607bbb5818410318e4b3496ad1820824` |
| 官方发布时间 | 2026-10-16 |
| 核对日期 | 2026-10-23 |

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
