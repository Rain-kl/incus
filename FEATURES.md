# PayIncus 定制功能记录

## 上游 LTS 基线

| 项目 | 当前值 |
| --- | --- |
| 官方仓库 | https://github.com/lxc/incus |
| 维护系列 | 7.0 LTS |
| 正式发布 tag | `v7.0.2` |
| 上游提交 | `01f81e5e607bbb5818410318e4b3496ad1820824` |
| 官方发布时间 | 2026-10-16 |
| 核对日期 | 2026-10-27 |

上游始终以正式 LTS 版本为准，仅同步 LTS 维护版本及经过兼容性验证的新 LTS 系列。普通功能版、开发分支及未发版提交均不纳入基线或同步目标。定制提交建立在记录的 LTS 基线上。

## 存储心跳、数据库容量与加密稀疏 Loop 扩容（2026-10-28）

- 目的：移除控制端容量查询对节点网络延迟的依赖，修复删除后重建池时的容量归零与无法创建实例；为绑定外部加密数据集的托管 ZFS Loop 提供准确扩容路径。
- 节点行为：`report/storage.go` 使用本地 Unix socket 的 `GET /1.0/storage-pools?recursion=1`、池 `/resources` 及现有 `/1.0/payincus/storage`，采集池容量、状态、加密/手动锁和底层文件系统信息，放入正常签名心跳。总采集预算 8 秒、资源请求并发 4、池最多 256；加密清单有独立 2 秒预算，包含尚未绑定 Incus 的外部数据集。完整/截断/失败明确区分，不把失败当作空清单。`report.go` 将原生 `/1.0/resources` 的硬盘和网卡信息随心跳上报，新增 `storage-inventory-v1` 能力；实例清单携带根盘池名；原生硬盘采集失败以 `storageAvailable=false` 标记，控制端保留上次硬盘信息。
- 立即上报：连接器 `cmd/payincus-agent/main.go` 在原周期计时器之外消费容量为 1 的刷新通知通道；`tunnel` 增加 `HEARTBEAT=0x0b` 并握手通告 `heartbeat-trigger-v1`，只接受流 0、空载荷，复用现有已认证连接。管理端通知只触发正常上报，不另开拉取通道，不改默认 10 秒周期。incusd 停机/采集失败时连接器继续上报不可用状态，本地调用最多占用 45 秒，为心跳传输保留请求预算；不会执行故障期间的 Caddy 指令。旧节点缺少能力时管理端明确拒绝通知，没有主动抓取兼容分支。
- 扩容：新增 `payincus_storage_pool_resize_v1` 与 `POST /1.0/payincus/storage/resize`（`dataset,sizeGiB`）。必须为远程 mTLS 控制端、未锁节点、已登记 GUID/父池指纹、在线的单个普通文件 vdev；存储写互斥。`security/storage_resize.go` 从原生 ZFS 拓扑定位自定义路径，通过截断增长保持新增区间稀疏，再运行 `zpool online -e`。只允许增长或同目标重试，拒绝缩容/非法大小/非托管池/块设备/多 vdev/符号链接；文件增长成功但 ZFS 扩展失败不回缩，支持同大小重试。创建时预分配的池新增区间也按稀疏方式增长。
- 控制端配套：Rain-kl/payincus 新增数据库存储快照；GET 和容量分配只读库，显式套餐绑定不借其它池；无心跳/锁盘/离线不可分配，已有容量保留。详情见该仓库 `docs/STORAGE_HEARTBEAT.md`。新控制端需要应用 schema 并配套更新节点，首个有效心跳前不分配；本条不表示已经发布或部署。
- 上游源码接入点：仅 `internal/server/storage/drivers/driver_zfs.go` 的 `Update` 增加已标记 `PayIncus:` 的大小校验，在原生 Loop 文件截断前拒绝非法值及缩容；原因是保护原有 vdev 数据，重试等值增长保留。其余实现均在定制模块。后续 LTS 同步核对原生 `size` 更新与卷资源 API、池状态 `Created`、实例 `expanded_devices`、异步/错误封装及 mTLS 授权，不改变原生 API 结构。
- 接口测试：`report/storage_test.go` 覆盖原生递归清单/资源 API、ZFS 密钥状态、失败及取消；`tunnel/control_test.go` 验证握手、心跳通知与原生字节流；`security/storage_resize_test.go` 覆盖远程授权、GUID/登记、失败后同大小重试、稀疏性/旧数据、缩容、多 vdev 和取消；`storage_resize_live_test.go` 在显式标记的隔离 QEMU 内运行真实加密 ZFS；`cmd/payincus-agent/main_test.go` 覆盖 daemon 故障时仍发送心跳，`internal/server/storage/drivers/driver_zfs_payincus_test.go` 验证原生池 PATCH 拒绝缩容/非法大小且保留文件内容。`scripts/payincus/test-zfs-vm.sh` 纳入该用例，集成 CI 增加节点/存储及原生驱动 race 合约，不增加下载内核和编译 ZFS 的 CI 任务。
- 已验证：定制模块及连接器 Go 单元/race 测试、go vet、原生存储驱动单元回归；隔离 VM 中原有 ZFS 生命周期与新建 1 GiB 加密稀疏池 → 2 GiB 扩容、ZFS 实际容量增加、原文件可读、重复目标和拒绝缩容均通过。控制端数据库/API/并发分配回归、类型检查及双端构建通过；没有对用户生产节点执行操作。

## TUI 节点更新下载进度条与回车覆盖行渲染（2026-10-28）

- 目的：修复 `incus-tui` 更新节点软件包时 `node-install.sh` 使用 `curl --silent` 静默下载 `.deb`，导致终端长时间停在 `Installing ...` 无任何进度反馈的问题。
- 行为与限制：`scripts/payincus/install.sh` 在下载软件包前输出包名与体积（MiB），将 `.deb` 下载参数由 `--silent` 改为 `COLUMNS=48 curl --fail --progress-bar --show-error`，并在 SHA256 校验通过后输出确认行；元数据与 `SHA256SUMS` 小文件保持静默下载。`tools/incus-tui/{operation_log.go,main.go}` 支持处理回车符 `\r` 进度刷新：未完结的 `\r` 进度帧在 UI 日志尾部（`operationLog.recent`）原地刷新同一行，待 `\n` 或命令收尾时再将最终帧写入磁盘会话日志，避免刷屏或污染落盘日志；`cleanText` 对含 `\r` 的单行保留最后一个非空片段。
- 配置与兼容性：无需新增配置；`COLUMNS=48` 加上 RFC3339 时间戳前缀控制在 80 列终端单行内。不影响原有 `--update` 非交互 APT、SHA256 校验及服务重启契约。
- 代码接入点与上游 API：`scripts/payincus/{install.sh,test-install.py}`、`tools/incus-tui/{operation_log.go,operation_log_test.go,main.go}`；全部位于独立安装脚本与 TUI 模块，不涉及上游源码、Incus API 或扩展变更。
- 测试与验证：`test-install.py` 验证 `--progress-bar`、`COLUMNS=48`、下载提示与 SHA256 校验输出；`operation_log_test.go` 新增 `TestCarriageReturnProgressUpdatesLiveTailWithoutFloodingDisk` 验证分片 `\r` 进度帧原地更新 UI 尾部且不刷写磁盘日志。`python3 scripts/payincus/test-install.py`、`bash -n scripts/payincus/install.sh` 及 `tools/incus-tui` 的 `go test -race ./...`、`go vet ./...` 均通过。

## TUI 系统命令路径与日志页面（2026-10-28）

- 目的：修复非登录 root 环境缺少 `/usr/sbin:/sbin` 时，一键 ZFS 依赖安装已成功却找不到 `modprobe`；隔离菜单、节点状态与命令输出，避免日志冲散界面。
- 行为：`tools/incus-tui/system_command.go` 在每次执行时重新解析命令，PATH 查找失败后检查标准系统目录，并为子进程补齐系统 PATH；保留调用者已有 PATH 和绝对路径，不修改父进程环境。APT 的非交互、原始错误和取消语义保留。软件包/模块实际缺失时仍报告错误，不把失败当作成功，不自动删除包管理锁。
- 界面：使用 alternate screen；执行中与结束后进入独立日志页，状态、文件路径、输出及操作提示分区。内存只保留最近 200 条已脱敏日志，完整内容继续写入原会话文件。PgUp/PgDn 翻页，End 跟随最新；结束后 Enter/Esc/l 返回，菜单按 l 重开。更新事务仍不可取消。
- 接入点：`tools/incus-tui/{backend.go,system_command.go,operation_log.go,log_view.go,main.go}`；仅独立 TUI 模块，无新增上游 API/扩展、权限或数据格式。无需配置，适用于已支持的 Debian/Ubuntu；不修改上游文件。同步 LTS 时复核 Bubble Tea alternate-screen 与命令执行生命周期。
- 测试：`system_command_test.go` 覆盖空 PATH、子进程系统目录及安装后重新查找；`log_view_test.go` 覆盖 80×24/40×12/120×40 边界、日志保留、分页、结束失败展示与返回菜单；现有 APT 非交互、ZFS 和更新取消保护测试保留。菜单守卫按现有“ZFS/更多”标签核对，修正旧标签断言。
- 本地验证：TUI `go test -race ./...`、`go vet ./...`、`golangci-lint run --new-from-rev HEAD ./...` 及 Linux arm64 静态构建通过；控制仓库 `make check` 通过。Incus 全仓 `make check INCUS_OFFLINE=1` 因本机缺少 cowsql 在构建前失败；全量 TUI lint 有 8 项既有问题，增量检查为 0 项。真机验证：`10.211.55.11`（Debian 13 arm64 / 6.12.113+deb13-arm64）备份原程序到 `/var/backups/incus-tui/incus-tui-before-log-path-20261028`，原子替换 `/usr/bin/incus-tui` 为 `a228a4ea1906+local-tui-log-path`。真实 80×24 PTY 以 `PATH=/usr/bin:/bin` 启动，从 ZFS 菜单确认一键依赖配置，APT、`modprobe zfs`、`zfs version` 均 exit 0；验证 PgUp 历史、End 跟随、返回菜单及退出。会话日志 `/var/log/incus-tui/session-20261028-173312-2652880864.log`；原始 PTY 记录 `/home/ryan/incus-tui-pty-test.ansi`。仅验证依赖配置，未创建/格式化存储池，未重启 Incus 或连接器。

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

- 2026-10-27 用户授权的 debian-test 全流程验收发现：仅通过 dpkg-shlibdeps 引入 `liblxc1t64` 不包含 `/usr/lib/<multiarch>/lxc/rootfs` 及 liblxc 运行辅助程序，真实非特权 LXC 在 forklxc 启动时失败。`scripts/payincus/package.sh` 增加 Debian 13 / Ubuntu 24.04 的 `liblxc-common` 显式运行依赖，由发行版包提供目录/工具，不手工造空目录或启用特权容器。测试节点安装该依赖后，用户前端成功创建实例 #7 `e2e-20261027-lxc-fixed`，节点 ID `u2-fbhgstgw`，Alpine 3.21 aarch64、10.10.1.210，真实运行且浏览器终端执行 id/uname/ip 命令通过。完整功能验收仍在进行，未将已完成的创建/终端步骤当作全组件通过。
- 同次验收发现原任务采集只跟随 journal/QEMU，LXC 根因留在节点 lxc.log，面板仅见 `forklxc exit status 1`。`internal/server/payincus/diagnostics/task_logs.go` 增加 lxc.log/forklxc.log 并发跟随，共用原有按实例约束、日志边界与取消回收；轮询 250ms，取消时最后读取剩余内容以降低快速失败漏日志。原有 4000 行/7500 字符与单次 64 KiB 边界仍保留，不将此称为无限日志。diagnostics/tunnel race tests 通过，`TestLXCLogDrainsLastFailureWhenTaskEnds` 覆盖末尾错误保留。新连接器已在测试节点本地安装，未发布；传输层最终送达与整条失败流程仍需验收。
- `.github/workflows/payincus.yml` 在 fork 的 main/canary、PR 和 `payincus-v*` tag 上运行；使用 GitHub 托管的 Ubuntu 24.04 amd64/arm64 原生 runner，Go 版本读取 LTS go.mod。
- 按 owner 2026-10-27 要求，集成 CI 仅保留 LTS tag/提交校验、固定 Xray、四目标原生构建/打包、包信息/内容与 SHA256 检查、产物上传及原发布接线。移除竞态/单元/daemon/CLI/TUI 测试、go vet、ZFS 虚拟机测试、安装器 fixtures、脚本检查及干净系统安装验收；不新增替代自动测试工作流，测试源码保留供单独运行。Debian 目标的构建依赖仅在 Debian 构建容器中安装，Ubuntu 目标保留本机构建依赖，去掉仅用于已移除测试的工具及失去用途的 matrix.image。CI 成功仅证明构建与产物检查通过，不代表测试、安装或真实节点验收通过。
- 上游 `.github/workflows/build.yml`、`tests.yml`、`release.yml` 各 job 增加带注释的官方仓库条件；保留上游定义，由 fork 专用流程承担构建与服务端发布，避免等待仅上游有的自托管 runner 或误发布客户端包。
- `scripts/payincus/package.sh` 生成 Ubuntu 24.04 的 amd64/arm64 deb、rootfs 归档、带源提交/LTS 基线/API 能力的清单和 SHA256SUMS。deb 依赖通过 dpkg-shlibdeps 计算；包含三个独立服务、官方 Guest Agent、恢复工具及许可声明。可选的 socket 激活不纳入此包，VM 的 QEMU/UEFI 为额外安装项。
- 原生包与官方 incus/incus-base/incus-client 声明冲突及替代，安装前必须安排迁移维护窗口；postinst 创建权限组、准备 root ID 映射并重载 systemd，不自动启用、启动或重启服务。不会通过旧 Agent OTA 替换 incusd。
- `payincus-v7.0.2-rc.N` tag 在四目标构建与产物检查成功后发布 GitHub prerelease，包含两系统/两架构产物与校验；普通分支的集成 job 上传 Actions artifacts，canary 另由发布工作流消费。测试标签不改变记录的上游 LTS 版本。
- 远程 fork 原 main 含未发版上游历史；集成分支保留该历史，合并时保持已验证的 v7.0.2 LTS 源码树及定制实现，禁止将远程 main 的未发版文件带入交付。
- CI 与发布结果以对应 GitHub Actions run 和 Release 的实际状态为准；在结果确认前不记为通过。
- CI 加速改动本地验证：两份 fork 工作流的 actionlint、安装器五项 fixtures（含发布条件回归）、控制仓库 `make check` 及 `git diff --check` 通过。仅调整 fork 集成调度与构建依赖，无上游 daemon/API 或发布条件变更；未执行远程 Actions，不声称已验证耗时或远程产物。

本地新增发布检查：Actionlint 验证 fork 集成工作流通过；ShellCheck、Bash 语法和完整 amd64 二进制/原生 deb/归档打包通过，SHA256SUMS 逐项校验通过。本地打包环境为 Debian 13，仅用于验证脚本，正式 Ubuntu 24.04 双架构产物以 Actions 构建为准，不上传本地临时产物。

CI 接线修正：fork 初始没有官方 release tag，校验步骤从官方仓库显式获取 v7.0.2 tag 并核对固定 SHA，不以 fork 自有 tag 代替上游发布依据。先前增加的干净系统安装验收已按上述 CI 加速要求移除；当前在构建、打包及产物检查通过后上传产物。

发布归档将 rootfs 的 UID/GID 固定为 0，避免把构建 runner 的文件所有者带到节点；deb 使用 dpkg-deb 的 root-owner-group。

### 非特权容器 root ID 映射准备（2026-10-27）

- 23:18 重启后的短暂 socket 错误另有根因：`incus.service` 为 Type=simple，systemctl 于进程启动即返回，而旧 TUI 的服务管理立即记录“操作完成”并刷新，早于 Unix socket 创建。只读核实新 PID 1009、启动前 root mapping 检查通过、socket 存在且 `/1.0/payincus` 已返回能力；不再把该瞬时提示当作 ID 映射修复失败。`backend.go/manageService` 为 TUI 的 Incus start/restart/enable 增加有界真实 API 就绪等待，复用更新流程的轮询；停止/禁用和连接器操作不等待 Incus。等待期间完整记录原始错误，超时/取消带最后 API 原因，未修改服务类型或上游启动逻辑。
- 就绪等待验证：`service_ready_test.go` 覆盖真实 UI 确认/完成路径、socket 首次不存在随后可用、三种启动动作、超时保留原始错误、systemctl 失败后不轮询，以及停止/连接器动作不轮询。TUI race suite（仅跳过上述既有菜单文案断言）、go vet、Linux arm64/amd64 编译、根 make check、diff 检查通过。仅只读观察 owner 已执行的真实节点重启，没有再次重启或部署本地修改；等待逻辑仍是未发布源码，不把这次只读观察算作新版节点验收。
- 23:09 再次失败的只读证据：节点已安装 `7.0.2~integration.be665dd0aa8f`，映射文件于 23:09:11 正确生成，但 PID 196 于 23:06:19 启动时缓存 `ErrNoUserMap`，`/proc/196/exe` 仍为 `incusd (deleted)`；重载后的 ExecStartPre 未在该旧进程启动时执行。常规安装器不主动重启的边界没有闭环展示。本轮不改节点，修复集中在安装完成提示和 TUI：`installed_update.go/installedUpdatePending` 通过 systemctl MainPID 和只读 `/proc/<pid>/exe` 检查已被替换的运行程序；API 可连接时也明确显示待应用更新，检测失败保留具体错误。
- 更新菜单增加“应用已安装更新（无需下载）”：危险操作确认后共用软件包更新锁，只读执行 `prepare-idmap --check`，复用 `update.go/restartUpdatedServices` 重启原本运行的 Incus/连接器、等节点 API、核对启用状态并重新打开已安装 TUI；不下载、不安装、不写映射。原本停止的服务保持停止；重启可能停止实例，必须维护窗口执行。普通安装模式检测已有运行中的 Incus 后打印待重启提示，包括 `--no-tui`；`--update` 仍由 TUI 管理重启，postinst 不自动重启。未改变发布或上游 API。
- 待应用更新回归：`installed_update_test.go` 覆盖真实命令参数、deleted/当前/停止/无效 PID、读取错误、API 可连接仍警告、仅重启原运行服务、先只读检查、失败传播、共享更新锁及确认后执行；安装器 fixtures 六项覆盖运行服务提示与原更新流程。`go test -race ./... -skip '^TestMenuGroupsKeepMaintenanceAndZFSOffHome$'`、`go vet ./...`、Linux arm64/amd64 编译、根 `make check`、Bash 语法和 diff 检查通过。完整 TUI 测试的既有菜单文案守卫仍要求“ZFS 管理/高级维护”，与当前“ZFS/更多”不符，该项单独跳过，未恢复用户菜单文案；未把跳过算作全量通过。没有在 debian-test 执行重启、替换二进制或创建实例；新入口尚未发布。
- 根因：原包依赖 `uidmap`，但 postinst 仅创建权限组，未落实正式 LTS `doc/installing.md` 的 root subuid/subgid 分配。`shared/idmap/set_load.go/NewSetFromSystem("root")` 在两文件和映射工具存在、但无 root 条目时返回 `ErrNoUserMap`；`internal/server/sys/os.go/getIdmapset` 因此缓存 nil，LXC `findIdmap` 返回 `System doesn't have a functional idmap setup`。只读检查 debian-test 证实两文件均为 0 字节，启动日志为 `Unable to parse system idmap: No map found for user`，未修改节点。
- 修复集中于独立 `scripts/payincus/prepare-idmap.py`，包内路径 `/usr/libexec/incus/prepare-idmap`；`package.sh` 声明 `uidmap`/`passwd`/`python3` 运行依赖并在 postinst 准备映射，`deploy/payincus/incus.service` 的 ExecStartPre 仅执行 `--check` 只读验证，不在启动时补配置、创建锁或备份。干净安装使用官方默认 `root:1000000:1000000000`；已有至少 65536 个 ID 的 root/数字 UID 0 分配按上游用户名优先规则原样保留，一侧缺失时沿用另一侧有效范围。保留小范围 raw.idmap 委托，不缩减或替换旧范围，不修改容器权限模式、磁盘 UID/GID 或实例配置；不主动重启服务。
- 自动补充前验证两文件格式、范围、现有委托与宿主用户/组 ID 冲突；冲突、缺失工具、非普通文件或写入失败均带路径/原始错误退出，不改用特权容器或绕过 namespace 隔离。新增条目前独占备份原文件（0600），缺失文件仅独占创建空文件；映射条目通过一次 `usermod --add-subuids/--add-subgids` 操作写入，复用 shadow 的系统账户数据库锁，不自行实现文件替换。随后回读确认新分配存在且旧分配未减少。自身准备调用另用文件锁协调；不实现跨文件失败回滚，usermod 部分失败保留原始输出和备份。标准系统 subid 文件为目标，特殊 NSS/subid 插件和并发外部账户管理需管理员协调，未实现迁移或在线重载。
- 升级已运行但缓存 nil 的 daemon 时，仅补文件不足以恢复；沿用已有 TUI 更新的维护窗口重启流程读取新映射，安装器和 postinst 不单独重启业务。已有自定义范围容量不足、内核禁止 user namespace、额外 raw.idmap 要求及容器来宾问题仍可能单独失败，不将文件准备当作容器成功启动的证据。
- 无上游源码/API/数据库变更；上游同步时核对正式 LTS NewSetFromSystem 的用户名/数字 UID 优先级、FilterPOSIX 最小范围及 daemon 加载时机。`scripts/payincus/test-prepare-idmap.py` 覆盖干净/空文件、幂等、已有自定义/数字 root、部分配置补齐、备份/权限、委托/宿主账户冲突、格式错误、symlink、工具缺失、usermod 请求/原始错误、启动检查无写入及包/service 接线；测试不接回集成 CI。
- 本次验证：上述 10 项 Python 测试、package.sh Bash 语法、控制仓库 `make check`、diff 空白检查通过。在一次性 Debian 13 / Ubuntu 24.04 arm64 容器中安装真实 uidmap/passwd/python3，使用基于当前正式 LTS 源码构建的 `idmap.NewSetFromSystem("root")` 探针，复现空文件 `No map found for user`；真实 usermod 补齐后解析得到两条 `u/g 0 1000000 1000000000` 映射，只读检查通过，重复准备无重复条目。未在该容器启动 daemon 或创建 LXC，未部署/重启 debian-test，尚未发布新包。

## 一键安装与 incus-tui（2026-10-23）

### 行为与边界

- 新增独立 Bubble Tea v1.3.10 TUI，入口 `tools/incus-tui/`。使用单独 Go module 固定依赖，不改上游 `go.mod` / `go.sum` 或 daemon；定制安装包将命令安装为 `/usr/bin/incus-tui`，依赖许可收集在 `NOTICE.tui`，保留 README 中声明的 MIT 许可及作者。
- `scripts/payincus/install.sh [latest|canary] [--no-tui]` 仅检测 OS/架构、查询 Rain-kl/incus GitHub release、下载匹配 deb 及 SHA256、通过 apt 安装，然后交接 TUI。缺少 curl/Python 时通过 apt 补齐安装器依赖。支持 Debian 13、Ubuntu 24.04 的 amd64/arm64，其他目标明确拒绝，不使用不匹配的 Ubuntu 包。
- `latest` 只接受正式 `payincus-vX.Y.Z` release，`canary` 只接受集成 prerelease；无包、错误平台、无校验文件、校验失败均不安装，不跨渠道或回退旧包。canary 搜索最近 100 个 release。旧发布的通用包名不再被新安装器选中。
- 包名增加 `linux-<ID>-<VERSION_ID>-<arch>`，manifest 新增 `targetDistribution`、实际 channel，Debian 13 使用 Debian 原生依赖构建及安装验证。稳定 tag `payincus-v7.0.2` 发布稳定版并标 latest，RC 继续 prerelease，canary 仍只发布已验证 push 的唯一集成 tag；上游基线仍为官方 v7.0.2 LTS，不改为普通功能版。
- TUI 提供 Panel 凭据配置及一次性 token 兑换、原子 0600 配置保存、连接服务启动、服务生命周期、实时 journal、固定基础初始化、mTLS 公钥证书指纹确认及本地隧道监听。仅写入确认过的配置，不运行旧 Agent 安装器，不在终端或错误信息显示 token/密钥；已有旧连接服务覆盖时在兑换前停止并给出诊断。
- 一键初始化面向无实例的非集群节点：dir 存储、固定 IPv4 NAT 网桥、default profile；相匹配的 default(dir)/incusbr0/基础 profile 可以复用以继续未完成的初始化，其它资源拒绝覆盖。失败不自动删除资源。内核、KVM、AppArmor、外层嵌套权限及 UID/GID 映射不由安装器/TUI 自动修改。Agent 在线与实例管理的 mTLS 验证分开。
- 仅调整本 fork 自有 `deploy/payincus/incus.service`，改为 `network.target`，避免 DNS wait-online 阻塞本地 daemon；连接器的网络等待保持原有行为。包安装仍不自动启动/重启服务，避免升级时直接影响运行中的节点。

### 操作诊断与自选 HTTP 连接（2026-10-27）

- 目的：解决一键初始化将原始 Incus 错误替换为“本机服务内部错误”、成功命令输出不可见和退出后日志消失的问题。`tools/incus-tui/{operation_log,backend,local_api,main,zfs}.go` 在普通终端屏幕持续打印全部 stdout/stderr、API 方法/路径/状态及完整响应，同时写入 `/var/log/incus-tui/session-*.log`（目录 0700、文件 0600）。不截断长行；日志不能写入时在终端明确报错。凭据字段、一次性 token、Agent 密钥及本次提交的 ZFS 密钥遮蔽，其他故障诊断保留。
- HTTP 错误保留 Incus error envelope；异步 operation 保留原始 `err` 与状态码；传输、取消、ZFS 命令及文件操作错误保留原始原因。仍拒绝覆盖已有资源，初始化部分失败后不自动删除存储池或网桥。Ctrl+C 在操作中取消并等待日志收尾，空闲时退出。
- 启动和操作结束后的自动状态检查只更新界面状态摘要（含证书指纹与检查错误），不输出命令/API 诊断，也不写入操作日志；主动选择“刷新状态”及其它新操作仍完整打印并保存本次输出。接入点仅为 `tools/incus-tui/main.go` 的刷新调度，不改变服务、Incus API 或错误返回契约，无新增配置。
- 2026-10-27 静默启动验证：TUI module 的 `go test -race ./...`、`go vet ./...`、Linux arm64 构建及控制仓库 `make check` 通过。`debian-test` 已替换为 `b3c523812079+local-quiet-startup`；真实 PTY 验证启动显示摘要/证书且操作日志为空，主动刷新完整记录命令/API，查看 ZFS 池时保留当前未安装 zpool 的原始错误，随后的自动状态刷新不追加诊断。未改变节点服务或存储；旧二进制备份为 `/var/backups/incus-tui/incus-tui-before-quiet-startup-20261027`。
- Panel 根地址允许用户自行选择 HTTP 或 HTTPS，包括非回环地址；不额外要求 TLS。保留 URL 结构校验、一次性 token 语义和禁止凭据兑换重定向。连接器的 HTTP/HTTPS 契约不变，无新增配置键或数据库格式。
- 上游接口仍为已记录的 7.0.2 LTS 原生 API 与 `payincus_node_v1`，没有上游源码接入点变更。后续同步核对 error envelope、operation `status_code/err` 和 Bubble Tea 普通屏幕输出行为。
- 验证：`go test -race ./...`（独立 TUI module）通过；`local_api_test.go/TestLocalAPIPreservesErrorsAndFullLogs` 覆盖 HTTP/异步失败与取消原因；`operation_log_test.go` 覆盖实时 stdout/stderr、退出码、128 KB 长行、密钥遮蔽和非回环 HTTP。OrbStack 真机与二进制替换结果另行记录，不以单元测试代替真实初始化成功。
- 本地 OrbStack Debian 13 arm64 真机已验证：新 TUI 正常启动、完整命令/API 日志持续输出并保存，`http://100.75.147.95:3000` 可进入凭据兑换确认页（取消确认，未消费实际 token）。`go vet ./...` 与 Linux arm64 编译通过，安装版本为 `02fc62644b01+local-diagnostics`，旧 TUI 备份在 `/var/backups/incus-tui/incus-tui-02fc62644b01`。未重启 Incus/连接器，未宣称已完成节点初始化；虚拟机现有 default 存储池和空 profile 保留。

### 一次性 token 接入与菜单收敛（2026-10-27）

- 主菜单收敛为初始化、一次性 token 接入、ZFS 管理、修复、更新、高级维护、刷新和退出。七项 ZFS 操作保留在 ZFS 管理子菜单；手填凭据、手动证书导入、本地监听修复及服务/日志管理移到高级维护。子菜单 Esc/q 返回主菜单，操作结束仍留在所属分组；原有危险操作确认与密钥保护保持。
- `tools/incus-tui/enrollment.go` 在确认后检查连接器 unit，启用 Incus 并最多等待一分钟 API 就绪，再校验已有监听；随后单次兑换 token、校验证书 SHA256 与有效期、保留既有信任并幂等导入 Panel 公钥证书、设置回环监听、原子保存 0600 凭据、启用并重启连接器。预检查失败不消费 token；后续失败保留原始错误和成功步骤，不自动重复兑换。接入不覆盖已有其它监听、不删除已有信任或存储，存储初始化仍为独立操作。
- 控制端 `GET /api/agent/install-config/:token?format=json` 返回 schemaVersion=1、连接凭据、Panel 公钥证书和指纹；服务端在消费 token 前校验证书就绪，不分发私钥。证书路径与 Incus 验证连接共享来源。无 format 的旧文本响应仍兼容旧版 TUI；新版完整接入需要配套控制端 JSON 契约。HTTP/HTTPS 继续由管理员选择，兑换禁止重定向。
- Bubble Tea v1 延迟的 OSC 颜色响应在输入层识别为终端协议数据，不能写入 Panel 地址或 token；普通 Esc 行为保留。入口为 `main.go`，不修改第三方模块或上游 Incus 源码。
- 验证：独立 TUI module 的 `go test -race ./...`、`go vet ./...` 和 Linux arm64 编译通过；`enrollment_test.go` 覆盖完整步骤、已有证书、各阶段失败、预检查不消费 token、真实本地控制端协议；`main_test.go` 覆盖 OSC 回复隔离。配套控制端类型检查、证书/安装/响应脱敏/路由 ID 守卫及根目录 make check 通过。全局管理路由守卫在未修改的 admin-tasks.ts/egress.ts 上存在既有失败，未按本轮修复完成计入。
- OrbStack `debian-test` 已本地替换并真实 PTY 验证版本 `2a8c290cdc00+local-enrollment`、token 确认页、ZFS/高级维护子菜单、返回及 OSC 输入隔离；测试未消费实际节点 token。旧 TUI 备份于 `/var/backups/incus-tui/incus-tui-02fc62644b01-20261027`。这是本地替换，尚未把该二进制发布为 GitHub canary 包。

### 基础网络初始化与“修复 → 修复网桥”（2026-10-27）

- 原因与修正：安装脚本只交接 TUI，旧 token 接入不检查 NAT 网络；旧初始化逐步写入资源，失败后又拒绝一切已有资源，且 `ipv4.address=auto` 与面板固定地址池不一致。`tools/incus-tui/{backend,local_api,network_setup,enrollment}.go` 改为 `incusbr0=10.10.0.1/22`、IPv4 NAT/DHCP；基础初始化先检查/创建桥，再创建或复用 default(dir)，最后补齐 profile。匹配的部分结果可重试；其它池、其它 profile 配置、已有实例和集群不被基础初始化覆盖。token 兑换前检查网络：缺失才创建，已有不兼容网络明确要求修复，不消费 token。不自动初始化存储。启动摘要同时显示桥是否就绪，自动检查保持静默。
- 新菜单在确认后调用独立 `tools/incus-tui/network_repair.go`。清点所有项目实例/profile，仅处理引用 incusbr0 的 NIC 和没有 NIC 的 LXC；先在 `/var/lib/incus-tui/repairs/bridge-*/state.json` 保存原网络、原配置、地址计划及阶段（目录 0700、文件 0600），再正常停止原本运行的持久 LXC，临时改为不引用网桥的 p2p NIC，删除旧桥、重建受管 NAT 网络、恢复 profile/NIC，最后只启动原本运行的容器。临时 NIC 保持接口名与 MAC，避免仅因解绑网桥触发 cloud-init 重跑。磁盘、根设备、其它 NIC、限速、profile 配置保持；共享 profile 的静态租约改为容器各自持有。
- 保留有效且唯一的 IPv4，缺失时优先保留运行中地址；仅缺失/冲突/不属于新网段的地址从 10.10.1.1—10.10.3.254 分配。变更时同步该容器的 Incus proxy connect 地址和已有 cloud-init network-config 的地址/网关。需要重新应用 cloud-init 网络配置时 Incus 会重置 cloud-init instance-id，来宾启动行为由镜像决定；不编辑来宾手工维护的网络文件。控制端心跳不会改写 IPAM 登记，地址变更映射会完整输出并保存，管理员须核对面板地址记录。修复前需在面板暂停节点上的新实例交付，TUI 不代替面板事务锁。
- 保留受管桥原有其它配置（含已配置 IPv6）；IPv4 网段改变时移除不适配的旧 DHCP 动态范围。非托管同名 Linux bridge 仅在端口均可确认为目标容器、停机解绑后端口为空时用无 shell 的 `ip link delete` 删除；拒绝物理上联网卡、外部接口/FAN、集群、VM 占用、运行中的 ephemeral 实例及未知 profile 引用。存在网络级 forward 时在停止容器前拒绝，避免删除规则；本轮不迁移这些规则。
- Incus 接口仍为正式 7.0.2 LTS：`GET /1.0` 探测 `profiles_all_projects/instance_all_projects`，跨项目 `GET /instances`、`GET /profiles`，`GET/PATCH /instances/:name?project=` 与 `/profiles/:name?project=`，`PUT /instances/:name/state?project=`、`GET/POST/DELETE /networks`、可用时 `GET /networks/incusbr0/forwards`。写回前重读并使用 ETag，异步等待与错误继续由 `local_api.go` 集中处理。无上游源码修改、数据库迁移或新依赖。修复失败/取消保留原始错误、失败阶段、备份及已完成变更，不继续自动启动；没有宣称自动回滚。重试前先按备份检查残留配置，不盲目丢弃备份。
- 测试：`network_setup_test.go` 覆盖各阶段部分失败后重试、不重复创建、固定网段、已有网络不被接入覆盖；`enrollment_test.go` 验证网络预检查失败不消费 token。`network_repair_test.go` 使用隔离 HTTP 服务执行真实请求，覆盖跨项目、继承 NIC、IP/MAC/根盘保留、冲突分配、proxy/cloud-init 地址变更、缺失/非托管桥、外部端口拒绝、766 地址池耗尽、备份权限、取消与原始错误保留、菜单确认。按 owner 要求，本轮不在 debian-test 执行修复，也不替换程序；隔离 API 测试不作为真实内核/来宾联网验收。
- 验证结果：独立 TUI module 的 `go test -race ./...`、`go vet ./...`、Linux arm64/amd64 编译，安装器隔离测试 `python3 scripts/payincus/test-install.py` 及控制仓库 `make check` 均通过。未发布或执行本轮真实节点修复；真实桥设备、来宾网络重新配置及停止/启动的验收仍需在 owner 指定的独立验证步骤完成。

### TUI 节点更新与渠道选择（2026-10-27）

- `tools/incus-tui/{main,update}.go` 新增“更新 → 开发版 / 稳定版”，分别映射 `canary` / `latest`；确认后从 Rain-kl/incus 的对应 GitHub Release 安装完整节点软件包。`scripts/payincus/package.sh` 将同一安装器随包放入 `/usr/libexec/incus/node-install.sh`，TUI 只调用此固定本地入口，不下载执行分支上的脚本。版本筛选、平台/架构限制、下载 HTTPS 和 SHA256 校验均复用 `scripts/payincus/install.sh`。
- 新增安装器 `--update` 模式：不打开嵌套 TUI；非交互 apt、`needrestart` 仅列出待重启项，`--reinstall --allow-downgrades --no-remove` 允许切换渠道或处理非时间排序的 SHA 版本，并拒绝自动移除其它包。没有合适的渠道 Release、包、校验文件、合法 URL 或 SHA256 时失败，不回退到别的渠道/平台。原安装模式保持原契约；不自动初始化、兑换 token 或改凭据。
- 更新前读取 Incus/连接器运行与开机启用状态并核对连接器 unit 为集成包版本；更新成功后仅重启原本运行的服务，先 Incus、等待原生节点 API 就绪，再连接器，最后核对运行及启用状态保持。原本停止的服务不主动启动；连接器运行而 Incus 停止的矛盾状态在安装前拒绝，避免 systemd Wants 连带启动 Incus。更新成功关闭旧 TUI 日志并 exec 新二进制，避免旧进程继续显示旧版本号；失败保留原始错误与已完成步骤，不伪装成回滚。
- 包更新及服务重启在确认后不能通过界面 Ctrl+C/Esc 中断，完整 stdout/stderr 持续显示并保存；下载/apt 自身失败仍返回真实原因。`/run/lock/incus-tui-update.lock` 的进程级 flock 防止两个 TUI 更新交错，进程退出自动释放锁。维护窗口内操作：现有 `incus.service` 的 ExecStop 执行节点 shutdown，会触发实例关机，后续启动取决于既有 autostart 策略；安全锁定与面板批准恢复不会被更新流程绕过。此功能更新已经使用本集成包的节点，不迁移独立旧 Agent/旧协议，不修改上游守护进程源码或数据库格式。
- 接口与验证：GitHub Releases/资产及系统 apt 由原安装器适配；就绪检查复用正式 7.0.2 LTS `GET /1.0/payincus`。`update_test.go` 覆盖渠道选择、确认、原运行服务集合与启用状态、各阶段错误传播、API 等待取消、事务不可在界面取消、成功重新打开 TUI/失败不重开；`scripts/payincus/test-install.py` 执行真实脚本的隔离 fixtures，覆盖两渠道×两系统×双架构更新、非交互环境、重新安装/渠道切换、APT 失败及校验拒绝。CI 干净目标系统的安装验收检查包内更新器存在且 `--help` 可执行。按 owner 要求不在 debian-test 执行更新或替换二进制；真实节点包替换/服务重启仍需独立验收，本地测试不代表发布生效。
- 本地验证通过：TUI module 的 `go test -race ./...`、`go vet ./...`、Linux arm64/amd64 构建；安装器五项测试、Bash 语法检查及控制仓库 `make check`。另覆盖并发更新锁、失败后释放锁、旧连接器拒绝和意外启用状态变更。未运行真实节点更新，未推送或发布本轮代码。

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

以上本地产物来自开发工作树，manifest 标记 dirty，不上传为正式 release。推送后的四目标 CI 负责 Ubuntu 24.04 / Debian 13 的 amd64 / arm64 原生包构建与产物检查，通过后发布对应提交的 canary 集成包；按上述加速调整不再执行安装验收。结果以该提交 Actions run 与 release 为准。实际宿主初始化/容器创建与 Panel 联通仍待节点验收。之前已记录的上游全仓静态/内核测试限制继续适用，本轮未重复运行无关的完整上游测试。

## 节点 CLI 安全模式、持续通信与控制端恢复（2026-10-26）

### 行为与边界

- 可选扩展 `payincus_security_v1`，由定制包 `INCUS_PAYINCUS=1` 启用。关键命令安全模式默认关闭，不要求安装 ZFS，DIR 节点同样可启用。所有受管本机 Linux CLI 调用均提交脱敏分类、UID/PID、时间与安全模式判定；不保存参数值、stdin、密码或密钥。非 Linux CLI 保留原行为。
- exec/shell/console、file、导出/发布/复制/移动、配置写入及启动/恢复等关键命令在执行前触发；普通 list/info/help/version 和明确的只读元数据子命令只审计。`info --show-log` 也属于敏感访问。未知命令、别名及外部别名按敏感调用处理，别名展开前再次检查；审计不覆盖被替换的二进制或绕开 CLI 的宿主操作。
- 触发先原子持久化统一 `locked` 状态，再异步停止本节点全部 LXC 容器。已有认证授权之后的 API 检查覆盖 Unix、TLS 与反向隧道；启动驱动、启动任务与 ZFS 挂载入口另行检查，启动读锁确保正在启动的容器不会晚于停止核验完成才启动。
- CLI 审计拒绝以及 daemon 安全锁定检查拒绝的非白名单 API 仍返回 HTTP 500 / `Failed to begin transaction: sql: database disk image is malformed (11)`，不包含审计或锁定原因，不伪造成功。已通过锁定检查的安全管理接口保留经过脱敏的原始错误状态码与固定错误文案；锁定期间白名单内的恢复、策略和解锁接口同样保留可诊断错误。普通管理失败不再伪装数据库损坏。此机制不保证向宿主 root 隐藏程序、状态文件或源码。
- incusd、连接 Agent、状态/告警/恢复接口及 LXC 必需的 onstop/onstopns 回调继续运行；onstart 不放行。状态不是各接口独立副本；关闭安全模式、断网、重连和重启均不解除已存在的锁定。开启安全模式或存在注册加密存储的节点重启时，在存储导入与自动启动前进入锁定。
- 未配置 ZFS 时报告 `storage/not_configured`，只宣称 API 锁定与容器停止；不宣称明文 DIR 获得加密保护。存储占用、停止失败、卸载失败均独立上报，API 锁定继续保持。失败后控制端可再次发起锁定以重试。
- 恢复需要已有 Incus 管理权限的远程 mTLS 身份、当前 revision、已完成的容器停止核验及所有注册存储密钥可用。Unix socket 不允许修改策略或批准恢复。这里复用的是 Incus 信任管理身份，并不是独立的硬件控制端身份；持有其它受信任管理员私钥的人也有相同权限。恢复经控制端原生 API 自动恢复锁定前运行的容器，具体见后续安全模式恢复条目。
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

- 入口 `tools/incus-tui/{zfs,local_api}.go`。Debian 13 在标准 deb822 源添加 contrib 前独占备份原文件并保留 URI/签名配置；自定义 sources.list 由管理员配置。Debian 13 / Ubuntu 24.04 均先检测运行内核已有的 ZFS 模块与 `/lib/modules/<kernel>/build/Makefile`：模块可用时只补用户态工具、util-linux 和 kmod，模块缺失才安装 DKMS。已有匹配 headers 不重复安装；否则先用 APT 查询准确 headers 包，缺失则在安装前停止并保留内核版本及原始错误，不盲目安装拼出的包名。不绕过 Secure Boot 或更换内核。
- OrbStack 定制内核的 headers 通常不在 Debian APT 源；`/sys/kernel/kheaders.tar.xz` 的存在不等于已具备 DKMS 所需的匹配构建目录。TUI 不自动解包或尝试不受 OrbStack 正式支持的自定义模块构建。没有现成 ZFS 模块和构建环境时明确提示需管理员准备环境或使用标准内核虚拟机；不宣称 ZFS 初始化成功。本次仅修改独立 TUI 模块，无上游 API、配置格式或 daemon 接入点变化。
- 2026-10-27 验证：独立 TUI module 的 `go test -race ./...`、`go vet ./...`、Linux arm64 构建及控制仓库 `make check` 通过。已替换 `debian-test` 的 `/usr/bin/incus-tui` 为 `2a8c290cdc00+local-zfs-preflight`，前一版本备份到 `/var/backups/incus-tui/incus-tui-before-zfs-preflight-20261027`。真实 PTY 从 ZFS 菜单执行依赖检查，准确报告 OrbStack 内核缺少模块和匹配 headers，保留 APT 原始错误，未执行软件包安装；完整日志 `/var/log/incus-tui/session-20261027-124821-3370806038.log` 权限 0600。未创建池、加载自定义模块或更换内核；没有将环境限制当作 ZFS 成功验收。
- 显式确认后创建专用磁盘/分区池，或预分配测试文件池；拒绝已有文件系统/挂载，不使用 zpool `-f`，不自动挑盘。文件池不是生产推荐，失败时保留预分配文件供检查。池、加密根、Incus 绑定分步执行；默认一键初始化仍是 DIR，不自动迁移或修改已有实例配置。
- 创建加密根时在专用 TUI 页展示随机密钥，后续确认页不重复展示；解除存储时隐藏输入。TUI 状态提供本地 API 返回的服务端证书 SHA256，管理员通过可信终端核对后保存控制端。终端/剪贴板由管理员管理。
- TUI 的生产实现已改为 Unix socket API 适配，避免管理动作误触 CLI 安全模式。保留原 Runner fixture 接口；集中处理 ETag、原生请求、异步 Pending/Running/Success/Failure、格式错误与取消，不盲目重试写操作。此前安装器/TUI 记录中的 CLI 接线由此适配层替代。

### 接口、测试与上游同步核对

| 接口/接入点 | 测试与核对内容 |
| --- | --- |
| CLI 分类、持久状态、revision、队列/ACK、无 ZFS、启动与停止并发 | `internal/server/payincus/security/security_test.go`：`TestPersistentSecurityModeWithoutZFS`、`TestRestartCorruptionAndFailedStop`、`TestConcurrentEventsDoNotLoseLock`、`TestCommandPolicyAndNoArgumentLeaks`、`TestContainmentWaitsForInFlightStart` |
| GET `/1.0/payincus/security`、`/report`，POST `/policy`、`/lock`、`/recover`、`/ack` | 同文件 `TestHTTPAuthorityReplayAndFormats`：Unix 与远程授权、参数/格式、过期 revision、节点上报保活；`service_test.go/TestPublicNodeErrorsRemainGeneric` 验证实际扩展响应不泄露失败原因 |
| POST `/create`、`/register`、`/unlock`；ZFS create/load-key/unmount/unload-key/get | `TestStorageKeyLifecycleAndBusyFailure`、`TestStorageHTTPEnrollmentUnlockAndRecovery`、`storage_live_test.go/TestZFSNativeLifecycle`；stdin 交钥、指纹/GUID、已加载密钥验证、busy/错误/取消、互斥、恢复不自动清锁 |
| `/1.0/payincus` 子路径原生路由 | `cmd/incusd/payincus_test.go/TestPayIncusAuthenticatedRoute`：实际 daemon 子路由、扩展能力和未认证拒绝；修复此前 ServeMux 缺少末尾斜线子树导致子路由 404 的问题 |
| Agent HMAC 上报 | `internal/server/payincus/panel/client_test.go/TestSecurityReportSignedContract`：规范路径/请求体签名、ACK、错误与取消；控制端报告验证/事务/去重测试 |
| TUI 原生 `/1.0`、storage-pools、networks、profiles/default、certificates、operations/*/wait | `tools/incus-tui/local_api_test.go`：真实 LTS JSON、请求/ETag、Pending 到 Success、失败/异步错误/取消/格式变化；现有 `backend_test.go` 保留不覆盖已有资源等回归 |
| TUI ZFS 安装、确认、池创建及指纹展示 | 同文件、`backend_test.go` 与 `zfs_test.go`：发行版、APT 备份、已加载/压缩模块、已有 headers、APT headers 查询、缺失依赖安装前拒绝及原始错误保留、危险请求拒绝、密钥只读展示/确认/隐藏 |

必要上游修改仅为：`cmd/incus/{main,main_aliases}.go` 的执行前观察；`cmd/incusd/daemon.go` 的认证后检查/启动前锁定/密钥请求禁用 debug body 捕获；`cmd/incusd/instances.go` 的启动任务检查；`internal/server/instance/drivers/driver_lxc.go` 的启动围栏；ZFS `driver_zfs.go` 的 Mount 和 `driver_zfs_volumes.go` 的 MountVolume 检查。各处保留 PayIncus 注释。上游 LTS 同步必须复核授权顺序、停止内部回调方法/路径、Start 签名与失败清理、池挂载重试、API JSON/ETag/异步 operation 及别名执行顺序，避免恢复时重放旧启动任务。安全模块和 TUI 独立文件承担实现；不修改上游数据库格式或依赖模块。

### 本地验证记录

- 扩展模块/连接 Agent race 测试、TUI race 与 lint、前后端类型检查及双端构建已完成；详细结果以本次最终测试记录为准。全量上游静态检查既有许可和内核限制仍按前文记录，不将其算作通过。
- 在隔离 Debian 13 中编译真实 daemon/CLI，运行 daemon 测试；实际启动 LXC fixture，验证关键 CLI 返回通用错误、容器停止、锁定期间文件 API 拒绝、报告保活、控制端 mTLS 恢复、关闭安全模式不清锁、重启保持锁定且不延迟自动启动。
- 配套控制端使用独立 PostgreSQL 测试库与真实 incusd 联通，验证指纹、策略、手动锁定、停止核验、恢复授权、密钥加密入库/领取；测试中没有连接生产。
- 在独立 QEMU TCG 虚拟机中运行 Debian 13 6.12.113 内核与 OpenZFS 2.3.9，使用预分配匿名文件 vdev，真实验证 AES 创建、已加载密钥验证、错误密钥、busy 卸载拒绝、数据卸载不可见、密钥加载与 API 恢复分离、恢复后内容完整，以及文件池 export/import-N/加载的回归。`scripts/payincus/test-zfs-vm.sh` 将该测试接入 Debian amd64 CI；仅 guest 内加载模块，不接触宿主磁盘。磁盘池、Ubuntu 内核/Secure Boot、真实服务升级仍需相应目标验收，不将本地文件 vdev 当作这些场景的证据。
- 真实 PTY 验证新增 ZFS 密钥创建页、已有加密根注册、取消与终端恢复；捕获的测试密钥仅在内存中校验，不保存终端录屏。真实 daemon debug 模式下确认拒绝的密钥请求仍返回通用错误，日志不包含提交的密钥。
- 新增脚本 ShellCheck、workflow actionlint 及改动范围 native lint 通过。原全仓静态/内核测试限制仍适用。发布、推送、CI run 和生产部署需独立记录，以对应提交的 Actions 与 Release 为准；本地验收未操作生产。

## 节点程序统一归属 Incus，移除独立 Agent 链路（2026-10-26）

- 配套 PayIncus 控制端已移除 `agent/` 源码、独立 Agent 编译/发布、binary/manifest 代理、自更新 API 与旧业务 RPC 回退；控制端镜像不再包含 Agent 构建产物。此前记录的新旧兼容过渡方案由本条替代，不新增升级迁移程序、旧配置搬运或历史协议兼容层。
- 唯一宿主连接器为本仓库 `cmd/payincus-agent`，随 incus-payincus deb 管理。删除 WebSocket 业务 RPC 执行器、局部方法转换适配和心跳独立升级指令；保留原生 API 字节转发、`payincus_node_v1` 能力握手和节点日志转发。控制端通过原生 mTLS API 执行业务操作，旧 RPC 不再可用。
- 心跳始终携带 `nodeApi=payincus_node_v1` 及能力声明，包含告警锁定时的最小报告；保留正常报告的资源能力。控制端拒绝缺失原生节点声明的报告，不触发旧节点更新。
- 控制端安装入口仅下载并执行本仓库 `scripts/payincus/install.sh latest|canary`。安装脚本职责仍仅为选择正式 release 包、校验、apt 安装和进入 TUI。Panel 地址与一次性 token 从环境交给 TUI，字段掩码显示并在进入界面时移除 token 环境变量，用户确认后才兑换；没有交互终端时由管理员稍后手工配置。服务 unit 必须匹配软件包提供的连接器；不接管、迁移或修复旧服务配置。
- 适用平台仍为 Debian 13 / Ubuntu 24.04 的 amd64、arm64。首次接入面向本集成包，不提供独立旧 Agent 的数据迁移、配置继承或协议回退；已经使用本集成包的节点可使用上文 TUI 更新功能。节点凭据接口沿用同一 HMAC 身份契约，不修改现有上游代码、数据库或依赖。
- 测试：`cmd/payincus-agent/main_test.go/TestNativeHeartbeatContractIncludesLockedNodes` 真实 Unix/HTTP 验证普通与锁定心跳、能力保留；`tunnel/control_test.go/TestNativeIncusStreamAndRejectedRPC` 真实签名 WebSocket 验证握手、字节中继、旧 RPC 不执行及断线取消；`nodeclient/client_test.go` 直接验证原生 HTTP 请求；`tools/incus-tui/main_test.go/TestInstallerTokenPrefillIsMaskedAndRemovedFromEnvironment` 验证交接不暴露 token。控制端另有分发边界、安装脚本双渠道、HMAC、超时/取消与并发控制契约测试。
- 上游 API/扩展仍为 7.0.2 LTS 原生 API 和 `payincus_node_v1`，无新增上游修改点。后续同步核对 HELLO 帧、原生请求及 TUI 初始化/凭据确认行为。移除的旧 RPC 测试由原生传输与拒绝旧 RPC 的契约测试替代；不把删除兼容代码视为旧节点迁移验收。

## 默认 10 秒心跳与即时配置下发（2026-10-26）

- 目的：降低节点状态上报延迟；实例配置写入继续走常驻反向隧道上的原生 mTLS API，不等待心跳轮询。
- `internal/server/payincus/config/config.go` 默认心跳改为 10 秒，允许范围 5–3600 秒；显式配置及 `INCUDAL_HEARTBEAT_INTERVAL_SECONDS` 仍优先，不强制重写已有节点配置。
- `report/report.go` 复用同一组默认值及范围，`service.go` 的 `GET /1.0/payincus/report?interval=` 接受 10 秒，TUI 保存新连接配置时写入 10 秒。认证、授权、签名和隧道协议不变；锁定节点仍可保持管理通信。
- 控制端新增已创建实例高级配置编辑，集中适配 Incus 7.0.2 的 `GET/PATCH /1.0/instances/{name}`，等待原生异步 operation 成功后保存实例覆盖值。涉及磁盘吞吐/IOPS、主网卡入站/出站带宽、LXC 进程数、CPU 优先级及启动配置。保留实际设备名称、存储池、网络和 MAC 等属性；继承 profile 的设备通过实例级覆盖更新，不修改共享 profile。
- 控制端独立保存管理员带宽意图与实际流量限速，超额期间保留较严格限速，恢复后重用管理员设置。详见 Rain-kl/payincus `docs/INSTANCE_ADVANCED_CONFIG.md`；这些变动不加入节点二次业务转换层。
- 接口测试：`config/config_test.go` 覆盖缺省值、10 秒及上下界；`report/report_test.go` 覆盖指标默认值及范围；`service_test.go/TestEndpointContracts` 覆盖报告接口 5/10/60 秒及非法间隔。上述包及 `cmd/payincus-agent` 的 race 测试、golangci-lint 和独立 TUI 测试通过。
- 上游基线：2026-10-26 再次核对官方正式 LTS 为 v7.0.2。本次不修改上游运行时代码，无新增上游挂钩；同步时核对原生实例 PATCH 的设备合并语义、配置热更新支持和 operation 响应契约。
- 限制：10 秒是正常工作时的采集周期，不是网络故障或慢采集下的交付时限。启动参数只用于后续启动/宿主关机；磁盘及网卡热更新取决于存储驱动、NIC 类型、内核 I/O 控制器和 HTB 支持。隔离原生 daemon 已验证停止实例配置；当前嵌套环境缺少 HTB 且 cgroup 委派不足，运行中完整热更新尚需具备这些能力的节点验证。未发布或部署本次改动。

## 套餐代理网关启动健康检查（2026-10-28）

- 测试节点上发现 systemd 返回启动成功后，代理核心仍可能尚未就绪，第一次检测失败导致创建任务立即回滚；同一网络命名空间稍后检测可成功。
- 网关在原有操作超时内重试实际 HTTPS 探测，未成功时始终保持出口阻断；记录不含节点凭据的检查错误类别。取消或超时仍返回未就绪，不允许直连回退。
- Linux 节点包测试覆盖首次失败后恢复和取消时不再检测。完整实例出口验收结果须以实际部署后的测试为准；源代码改动尚未发布。
- 套餐组在相同操作时限内等待当前 systemd InvocationID 和 Xray 控制 API 就绪，再执行 HTTPS/DNS 检测；API 超时或取消仍保持阻断。接入点为 `egress/groups.go`，沿用现有 systemd 与 Xray 控制 API，无新增配置或上游 API；`TestGroupCoreWaitsForAPIAndHonorsCancellation` 覆盖延迟就绪、超时和取消。
- 2026-10-28 在用户授权的 OrbStack `debian-test` 中备份原 daemon，临时替换为含未提交修复的 Linux arm64 `incusd` 并重启；旧版备份位于 `/var/backups/incusd-before-egress-ready-NQWNGobA`。用户端以原代理套餐创建测试实例 `e2e-egress-ready-20261028`（节点名 `u2-bfy19roq`），任务 #32 成功，两次出口绑定完成，容器运行；容器内 DNS 解析成功、HTTPS 出口地址与配置的荷兰代理节点一致。该测试不证明所有故障场景和长期稳定性；替换不是节点包发布，原套餐失败实例保留。

## Xray 控制、校验热路径与出口恢复（2026-10-28）

### 行为与边界

- 保留独立 Xray 进程及各套餐出口组的独立网络命名空间，不导入 Xray Go 内核、不在 incusd 内创建 Xray 多实例；继续使用固定正式版 v26.3.27 及现有摘要。Incus 上游 LTS 核对仍为 v7.0.2，未同步开发提交。
- 未变且服务/API 健康的出口组跳过临时配置写入、配置校验子进程、unit 写入和重启。主机级策略仅变更防火墙/实例清单时，不再重复校验或改写未变的 Xray 配置。配置变化及启动/修复仍验证后应用；验证失败保留最后有效文件。
- 二进制验证缓存以路径、文件身份/大小/时间/权限为依据；Linux 身份包含 inode/ctime，变更即重新哈希并恢复固定载荷。缓存最多 128 项、使用时最长 5 分钟重新校验，不是后台定时扫描器。拒绝符号链接和非常规文件；不声明可对抗宿主 root 篡改程序或内存。
- 使用受保护目录中的 `api.sock` 管理原生 Xray gRPC，复用本地连接，单次 RPC 上限 2 秒、响应上限 2 MiB、连接池上限 128。关闭组时释放对应连接。禁止环境代理介入本地 Unix 通信；不增加公网/网卡监听，不修改 Incus mTLS/节点 API 鉴权。
- 固定版使用 `dokodemo-door` 的 Unix 入站及 API 路由，目标端口必须非零，否则该固定版会在收到连接时 panic。真实 gRPC 回归覆盖此约束；未使用较新版本才有的 `api.listen` Unix 配置。
- `ListInbounds` 请求仅标签；该固定版 `ListOutbounds` 请求为空、响应可能包含设置，仅在有界本地缓冲中解析标签，禁止记录/上报响应或远端原始错误。故障切换通过原生 `OverrideBalancerTarget`，随后 `GetBalancerInfo` 读回目标确认；移除生产路径中的 `xray api bo` 和启动 shell 重试循环。缺少方法、异常响应和拒绝访问均不降级到 CLI 或直连。
- systemd 启动后只确认 API/黑洞初始化，不凭进程启动设置转发 ready。主机代理通过真实 HTTPS 出口及网关 TCP/UDP DNS 检测后放行；显式 direct 模式仍在 API 与服务实例确认后走管理员选择的直连策略。套餐出口组需进程当前 InvocationID、API、HTTPS、TCP/UDP DNS 均满足，重启不可沿用旧探测结果。
- 进程停止/启动前清理就绪标记；单套餐服务只清理自身 mark。API/探测故障不启用直连回退。主机级 API 连续失败三轮后只修复 Xray 服务；远端出口/DNS 失败不触发反复重启内核。失败健康状态保留，失败探测下一轮重新尝试；服务 InvocationID 改变时立即使旧探测失效，不等待长探测间隔。周期协调仍为 10 秒，外部探测有超时，不承诺故障检测零延迟。
- 主机旧式代理在已应用策略上的进程/API 故障不阻止独立套餐组的周期协调。不得将 API 在线误认成完整数据路径可用，亦不承诺所有配置无损热更新；配置/监听/DNS 等仍受控重启，长期连接是否存活依变更及原生内核行为而定。

### 接入点及上游 API 契约

| 路径 / 接口 | 作用与契约 | 验证 |
| --- | --- | --- |
| `egress/core_api.go`，HandlerService.ListInbounds/ListOutbounds | 依据 XTLS/Xray-core v26.3.27 `app/proxyman/command/command.proto`，分别请求字段 1 的 true / 空请求，响应字段 1 为 handler 列表、handler 字段 1 为 tag | `TestCoreAPIContracts`、原生 Xray 回归 |
| RoutingService.OverrideBalancerTarget/GetBalancerInfo | 依据同版本 `app/router/command/command.proto`；写入 balancerTag/target，读回 balancer.override.target，目标不匹配即失败 | 原生黑洞/指定代理切换、读回不匹配、取消/超时/异常响应 |
| `egress/core_files.go`、`embed.go` | 校验/文件快路径与有界二进制缓存，不更改共享配置 | 配置不变不启动校验、不写文件；失败保留有效文件；取消、篡改修复、热路径基准 |
| `egress/health.go`、`manager.go`、`groups.go` | 服务实例身份及 HTTP/DNS 就绪；保留网络命名空间、nftables、版本/代际检查与失败阻断 | DNS 双传输失败、systemd 状态及 InvocationID、真实 namespace/重启与包路径隔离 |
| `cmd/payincus-network-worker/main.go` | 增加 `-network-core-api`、`-network-core-failover`、`-network-dns-address`，仅节点内部启动/探测用途 | systemd 真正运行 worker 与固定 Xray，不依赖 shell API 模拟 |
| `go.mod` | 将已有 gRPC v1.84.0 从 indirect 标为直接依赖，保留原有 LTS 版本并标注 `PayIncus:` 原因；无新的 Xray runtime 依赖、无 go.sum/Go 工具链升级 | 构建、竞态及 lint；上游同步时核对依赖版本和协议适配 |

`egress/` 均指 `internal/server/payincus/egress/`。新增实现集中于定制目录；未增加或修改上游 daemon/驱动运行时挂钩。gRPC 字段采用独立小适配层，无生成代码或 Xray 源码拷入。同步上游/Xray 时须对照固定版 proto、入站 Unix 行为、方法存在性、错误/取消、服务实例身份以及内核转发链逐项回归。

### 集成验证

- 定制模块与连接器 race 测试包含 gRPC 正常请求、错误脱敏、缺失能力、畸形响应、标签校验、读回确认、取消/超时；原生 Xray 真实切换黑洞/代理并发起 HTTPS，验证无直连泄漏。
- `scripts/payincus/test-egress-systemd.sh`：可销毁容器中真实 systemd、固定 Xray、新 worker，覆盖私有/残留/占用命名空间、重新启动、DNS bind mount、排队恢复及所有代理策略 API 初始化。无宿主网络/PID/cgroup 挂载或内核模块加载；可用 `PAYINCUS_EGRESS_SYSTEMD_IMAGE` 指定已准备的本地镜像。
- `scripts/payincus/test-egress-vm.sh`：仅在隔离 QEMU 中加载内核模块，验证不同代理组及 direct 的 TCP/UDP/DNS 来源、单组故障不串流、失败阻断和 IPv6/来源伪造限制。需已准备 amd64 固定载荷与匹配 Debian 内核/模块。CI Debian amd64 在 main 与既有 ZFS VM 构建环境复用，canary 独立运行代理 VM；均不在宿主上加载模块。
- 本地基准仅衡量二进制核对热路径：完整哈希约 30–31ms、分配约 36.6MB/次，缓存命中约 8–12µs、分配约 2.5KB/次；不代表完整协调循环或代理转发吞吐提升。未进行生产部署或真实外部代理吞吐压测。
- 本地已通过：定制模块/连接器 race、固定 Xray 原生 API/HTTPS 回归、真实 systemd 全部恢复场景、隔离 QEMU 套餐 TCP/UDP/DNS 数据路径、改动范围 golangci-lint、脚本 ShellCheck、workflow actionlint、`go mod tidy -diff` 及差异空白检查。CI 已补充对应集成步骤，但尚未推送或触发远程 CI；不将本地结果视为发布验收。

- canary 合并保留既有套餐健康检查重试，并将 TCP/UDP DNS 纳入同一探测；保留该分支既有精简构建流程，仅增加本次代理模块 race、systemd、代理 VM 和新增脚本检查，不恢复已删除的其它测试步骤。合并时独立追加文档，避免覆盖已有特性记录。

## 修复干净 CI 环境的代理 VM 依赖（2026-10-28）

- GitHub Actions 的 Debian amd64 代理 VM 步骤缺少提供 `sysctl` 的 `procps`，准备 initramfs 时 `command -v sysctl` 在 `set -e` 下直接退出。此前本地镜像预装了该包，未覆盖干净依赖清单；其余三个构建矩阵通过不能替代此项验收。
- `.github/workflows/payincus.yml` 显式安装 `procps`。`scripts/payincus/test-egress-vm.sh` 在缺少待复制工具时输出依赖名称；QEMU 失败/超时时先打印 guest 日志，再保留原退出码，防止再次静默失败。不跳过真实数据路径测试，不修改生产代码或上游 API。
- 本地验证已通过：按 CI 原始 Debian 13 依赖清单建立全新镜像，复现 `Missing VM build dependency: sysctl`；仅补齐 `procps` 后完整 QEMU 数据路径返回 `PACKAGE_VM_TEST_EXIT=0`。模拟 QEMU 返回 42 时打印日志且保留退出码；ShellCheck、workflow actionlint、bash 语法及差异空白检查通过。远程集成与发布以对应提交的 Actions 为准。

## 安全模式运行状态清单与控制端自动恢复（2026-10-28）

- 统一文案、注释和测试命名为安全模式，替换此前旧称。默认仍关闭；关键 CLI 分类、通用错误响应、独立锁定和存储清钥规则保持。变更前再次核对正式 LTS 为 v7.0.2，未同步普通功能版或开发提交。
- `security.State` 增加 `resumeCaptured`、`resumeInstances`，记录项目、名称、volatile.uuid 和冻结状态，不含密钥。`cmd/incusd/payincus_security.go` 的独立定制适配器只读清点运行中的本机 LXC；启动围栏排空后，持久化恢复清单，再停止与清钥。重复锁定与 daemon 重启保留原清单及失败恢复项。
- 清单读取/落盘失败仍执行容器停止与存储处置，独立报告 `recovery_plan` 失败并保持锁定，不误报恢复完成。无法追溯此前未记录的状态，也不能自动重建已销毁实例。新功能不加入数据迁移、旧 Agent 或历史协议兼容层。
- 节点授权恢复仍先验证当前 revision、停止确认及加密存储；保留清单供控制端通过 LTS 原生实例 API 自动恢复原运行/冻结状态。原停止实例不会列入清单。新增能力 `payincus_security_resume_v1` 及 POST `/1.0/payincus/security/resume`，仅远程受信任 mTLS 管理身份可按 revision/完整实例身份逐项确认；Unix 请求、锁定期间确认、过期 revision、未知实例均拒绝，成功后增加 revision。API 保持通用内部错误，不改变上游认证和启动挂钩。
- 配套控制端在安全模式恢复流程等待原生异步启动/冻结成功并核验状态，逐项 ACK；部分失败允许重试，防止重复启动已成功实例，拒绝 UUID 改变的同名实例并遵守后续业务封停/删除/到期。面板对离线/锁定等节点投影实例错误状态，节点列表显示已锁定；CLI 审计白名单不包含心跳或状态请求。详见 Rain-kl/payincus `docs/NODE_SECURITY.md`。
- 新增 `security/resume_test.go`：停机前持久化、重复锁定/重启、失败处置、权限/格式/过期确认、完整身份、遥测不生成 CLI 事件；扩展 `service_test.go` 通用错误及真实 daemon 能力声明断言。定制模块及连接器 race、定向 lint、真实 daemon 路由测试通过；隔离 Debian daemon + mTLS + 实际 LXC + 控制端原生客户端验证运行恢复、冻结恢复、原停止保持、ACK 清空及轮询不入 CLI 审计。
- 新增逻辑集中在定制目录，无上游数据库或存储格式修改、无依赖升级。上游原代码仅替换既有 PayIncus 注释中的旧称；LTS 同步核对 LoadNodeAll、Project/LocalConfig/IsRunning/IsFrozen 只读契约及原生 state/operation 语义。未发布或部署本次功能。
- 最终全量回归暴露本地 Xray gRPC 超时状态先于 context 定时器触发的竞态：`egress/core_api.go` 显式将 gRPC DeadlineExceeded/Canceled 映射到标准 context 错误，仍不传播远端文本。`TestCoreAPIContracts` 重复竞态检查覆盖该错误类型契约，避免 CI 偶发误报控制接口错误。

## 安全管理接口保留正常失败原因（2026-10-28）

- 目的：修复安全模式未锁定时，创建加密数据集、密钥校验或管理权限等失败仍统一伪装为数据库损坏的问题。`internal/server/payincus/service.go` 删除二次错误包装，直接转发 `security.Manager.Handle` 既有状态码和固定安全文案；不输出密钥、请求体、底层 stderr 或任意异常文本。
- 边界：不修改 `cmd/incusd/daemon.go` 的认证/授权和持久锁检查，不修改 `security.PublicError`、CLI 审计、锁定白名单和存储行为。被锁定检查拒绝的请求保持现有 HTTP 500 掩盖约定；允许进入管理处理器的请求保留 400/403/404/409/423/503 等真实语义。安全模式开启但未锁定同样不掩盖普通管理错误。无新增配置、API 或扩展，无数据迁移、依赖升级或上游文件改动；仍基于当日核对的正式 v7.0.2 LTS。
- 测试位置：`internal/server/payincus/service_test.go` 将原先要求所有错误都掩盖的断言替换为明确接口契约，覆盖关闭/开启但未锁定、参数/指纹/创建/未注册密钥/权限/路径错误和敏感值不回显；真实 HTTP server 验证锁定期间恢复与解锁失败保留状态及文案且不清锁。原 `security` 包锁定持久化、白名单、创建/注册/解锁和恢复测试继续执行。
- 验证：隔离 Linux 容器中 `go test -race ./internal/server/payincus ./internal/server/payincus/security` 已通过；同范围 Linux `go vet`、增量 `golangci-lint`（0 问题）、差异空白检查和控制仓库 `make check` 均通过。Incus 全仓 `make check INCUS_OFFLINE=1` 因本地缺少 cowsql 在构建前失败，未计为通过。未部署到运行节点；现有节点须更新 daemon 后才会改变错误响应，单独替换 TUI 不会生效。上游同步核对服务 envelope 与 daemon 锁定检查顺序。

## 存储池加密流程重做（进行中，2026-10-28）

- 目标：控制端和 TUI 均默认创建加密 ZFS；控制端自动生成/托管密钥，TUI 独立创建并在成功后展示密钥；加密卷状态、密钥录入、锁盘/解锁、相关实例恢复及密钥删除跟随存储池生命周期。ZFS UI 从节点安全模式迁入存储池。完整流程与 computer-use E2E 尚未完成，不能按接口测试通过宣称交付。
- 已实现基础接口：`payincus_storage_encryption_v1` 能力；`GET /1.0/payincus/storage` 发现所有 ZFS 加密文件系统/卷（含未注册外部卷）及 encryption root、算法、key status、key format；`POST /storage/create|register|unlock` 使用现有 ZFS 原生适配和身份校验，不再要求控制端托管声明。独立创建仍需调用方提供随机密钥；自动池创建、TUI 成功页及控制端交换/托管尚待接入，不宣称已实现。旧安全接口暂保留，待调用方迁移后清理。
- 权限/日志边界：继续由真实 daemon 的 can_edit 授权接入。新 storage 子树不暴露 policy/recover；锁定时只放行加密状态读取和已注册密钥加载，不放行 create/register。存储解锁不清除节点安全锁。整个新子树进入 SensitivePath，密钥请求不进入 debug body 捕获；响应只有固定错误与非秘密存储身份。无新增密钥磁盘文件，无密码学格式或现有注册数据迁移。
- 接入点：`internal/server/payincus/service.go` 路由和能力声明；`security/http.go` 复用管理操作并移除新入口的托管前提；`security/storage.go` 的 ZFS inventory 适配；`security/policy.go` 的精确只读/解锁白名单和敏感路径。仅独立定制目录，未修改上游源码/数据库或依赖；LTS 同步复核原生 zfs list 列定义、daemon 授权/锁检查顺序和 debug 排除机制。
- 契约测试：`security/storage_http_test.go` 覆盖无控制端声明创建、stdin 密钥传输、注册身份、锁定期间创建拒绝/解锁不清锁、命名空间隔离及 debug 路径排除；inventory 覆盖外部锁定加密根与继承卷、空列表和畸形输出。`service_test.go` 通过实际 HTTP server 验证 namespace 路由/错误 envelope/能力声明。隔离 Linux race 测试、同范围 go vet、增量 golangci-lint（0 问题）及控制仓库 make check 均通过。Incus 全仓 make check 仍受本机缺少 cowsql 阻塞，不计为通过。
- 未部署，未修改测试机现有池/实例/密钥。后续仍须完成控制端存储池数据模型/API、所有 UI 入口迁移、TUI 创建与成功密钥页、删除时密钥同步清理、锁盘与实例精确恢复、导入后持久解锁，以及 computer-use 全流程验证。

### TUI 独立一步创建加密存储（已接线，尚待真机 E2E）

- `tools/incus-tui/encrypted_pool.go` 将底层文件/磁盘建池与 `/storage/create` 的 AES-256-GCM 子数据集创建/密钥校验/注册整合为一次确认。每次随机生成 32 字节密钥，仅通过本地 Unix API body 传输；无需控制端、指纹复制或托管声明。先探测 `payincus_storage_encryption_v1` 并检查节点锁状态，拒绝旧节点或锁定状态后才分配设备。现有设备无强制覆盖、测试文件独占创建等保护保留。
- 菜单合并为“创建加密 ZFS 存储池”，删除独立“创建加密数据集并显示密钥”；注册已有加密数据集及密钥加载改走 storage 命名空间，不再要求控制端指纹。保留管理员自行导入存储池的流程。
- `main.go` 的专用临时结果页在节点确认加密身份后显示密钥及实际导入源 `池名/incus`，不把密钥放入普通日志视图。离开结果页清除 UI 引用。请求/错误日志沿用脱敏；节点不保存明文密钥文件。底层池已创建后若 API 失败、取消或身份响应无效，结果页保留密钥并说明加密初始化未确认完成，不宣称成功或自动删除数据。
- 测试：`encrypted_pool_test.go` 覆盖无控制端依赖、旧版本/锁定时零存储操作、创建拒绝/身份不匹配时密钥保留、日志不泄密、结果页和离开后的 UI 清理。更新原菜单/两步托管断言为新流程的确认与取消契约。TUI `go test -race ./...`、`go vet ./...`、增量 lint（0 问题）和 Linux arm64 静态构建通过。当前只通过 fixture/模型测试，尚未部署到节点或完成 computer-use 真机创建验收。
- 仍待完成：控制端创建自动交换与托管、存储池 UI 迁移、外部加密卷导入后解锁/持久托管、删除密钥清理、手动锁盘与受影响实例恢复。早先条目中的待接入状态以本条进展为准，不据此缩小完整目标。

### 控制端自动建池与可恢复的节点初始化（进行中）

- 新增 `POST /1.0/payincus/storage/pool`：调用方提供池名、专用设备或整数 GiB 文件容量及随机密钥；节点在原动作互斥和安全锁检查内创建底层池、`池名/incus` 加密根、验证密钥并登记。物理设备必须为空块设备；文件独占创建；不使用强制覆盖。
- 新池根数据集在 `zpool create -O` 中带 `payincus:key-fingerprint` 用户属性。响应丢失后，只对相同密钥身份恢复未完成初始化；已注册加密根重试验证 GUID/密钥后返回状态，不重复建池。标记不匹配的既有池与无标记既有文件均拒绝。错误只返回固定安全说明，密钥经 stdin 进入 ZFS；新路由不在锁定白名单，仍受请求体日志排除保护。
- `security/pool_test.go` 模拟加密数据集创建中断、同密钥继续、完成请求幂等、错误密钥和已存在文件拒绝。Linux 定制模块 race 测试通过；实际 ZFS/设备路径仍须真机测试。控制端先托管密钥再调用节点，最终以真实 Incus storage-pools 身份和加密状态确认完成；对应控制仓库代码和证据见 `docs/NODE_SECURITY.md`。
- 本轮新增/修改仅在独立定制目录，无上游文件、API 扩展列表或数据库格式改动；新路由复用之前声明的 `payincus_storage_encryption_v1` 能力，尚未发布过该能力。未部署至运行节点；computer-use E2E 仍未完成。

### 删除后的登记清理（进行中）

- 新增 `POST /1.0/payincus/storage/forget`，只接受 dataset/GUID，不接收密钥。`security/storage_delete.go` 要求实际父池可枚举、目标及后代已不存在、登记 GUID 与请求一致后才移除单个目标。父/兄弟目标不变；请求幂等。该受限清理在节点锁定期间允许执行，不清除 API 锁或放宽创建权限；同样经过 daemon can_edit 认证与动作互斥。
- 父池未导入、导出、离线、枚举失败/空响应时保留登记和密钥，不把“不可见”当作“已销毁”。当前整父池销毁后仍缺少可靠删除证明，控制端会保留检查点并报告清理未完成；此项和受管文件回收尚未完成，不能据此宣称磁盘全生命周期验收通过。
- `security/storage_delete_test.go` 验证消失目标清理、重复请求、仍存在/不可枚举/错误 GUID 拒绝、其它目标及锁定状态保持。控制端有配套持久检查点和事务密钥删除测试。Linux race/静态检查结果以本轮记录为准，未做真实删除或部署。改动集中于定制模块，未修改上游驱动或数据库。

### 按加密根手动锁盘与精确恢复（2026-10-28，尚待真机 E2E）

- owner 已明确第 9 项为已加密磁盘锁盘，不是明文数据格式转换。`security/storage_lock.go` 持久化每个加密根的手动锁、完成状态及原运行实例清单；节点写入清单后才停止实例和卸载密钥。失败保留 pending 状态及原清单，重试不重新采集已停止实例。普通加载密钥/登记/池初始化重试均不能打开手动锁盘，必须通过远程控制端的明确解密操作。
- `cmd/incusd/payincus_storage.go` 用原生 LoadNodeAll(Any)、ExpandedDevices、storage.LoadByName/ToAPI 解析实际存储归属；同一加密根下的关联池均受影响，其它池不停止。保存 project/name/volatile.uuid/Frozen，不保存密钥。停止后重新核对不存在相关运行实例；没有原生适配器或身份不完整时拒绝停机。
- 新 `POST /storage/lock-pool|unlock-pool|resume` 只允许远程已认证控制端；本地 Unix 请求不能解除手动锁。unlock-pool 不清节点 API 锁。逐实例恢复 ACK 检查 dataset、revision、完整实例身份及手动/全局锁状态，只有确认恢复或控制端业务策略明确跳过后才从清单删除。状态目录、原子写入和跨进程文件锁继续复用，新增字段向后兼容。
- 必要上游接入点：现有 `driver_zfs.go` Mount 与 `driver_zfs_volumes.go` MountVolume 的检查改为同时检查全局及手动加密根锁；`driver_qemu.go` 的统一 start 入口加入与 LXC 相同的 BeginStart 围栏，覆盖 HTTP 外的 VM 启动/迁移/恢复路径。附近均有 PayIncus 注释。全局原生 snapshot/stop 也覆盖 VM，防止 VM 仍占用磁盘导致卸载密钥失败；控制端原生恢复支持 container/virtual-machine 并维持冻结状态。LTS 同步复核这些入口、池配置 source、设备展开及实例 UUID/IsFrozen/Stop 契约，不能删除启动围栏或扩大普通锁定白名单。
- `security/storage_lock_test.go` 覆盖先持久化后停机、停止失败不卸钥、重试保留清单、其它池访问不受影响、自动加载不能打开手动锁、远程明确解密、待恢复清单禁止重新锁盘覆盖、UUID/ACK 校验。Linux 定制模块 race 通过；真实 daemon TestPayIncusAuthenticatedRoute 编译及路由运行通过（隔离 build 容器已装原生 cowsql/LXC 开发依赖）。这证明真实 daemon 可编译并接入路由，不等同于实际 LXC/VM 锁盘与恢复验收。

### 加密存储联调收尾（2026-10-28，以本条更新前述进行中状态）

- TUI 一步创建已统一调用 `/storage/pool`，复用底层分配、身份标记和加密根注册。控制端存储页已接入创建、外部 hex 加密卷待导入/解锁托管、手动锁盘/恢复及删除清理；节点安全页只保留节点安全管理。对应控制仓库实现及 computer-use 证据见 `docs/NODE_SECURITY.md`。
- Boot 不再仅因 Targets 非空就开启全局锁；安全策略关闭的普通加密存储可由控制端自动加载。安全策略已开启、已有全局锁及每根手动锁仍持久保留，关闭策略不解除已有锁。新增重启策略回归测试。
- `GET /storage` 返回全局 locked 及可见数据集（包括明文项），防止未知被当作未加密。外部已存在的独立池根可登记；UnlockManaged 先检查系统挂载点和支持的加密身份，再验证密钥和接管为 prompt，不删除管理员原有 key 文件。当前只托管 AES-256-GCM/hex，不支持明文迁移或 raw/passphrase 转换。
- 锁定下只额外放行原生 GET storage-pools 列表及单池身份，卷内容、资源详情及写操作不开放。`/storage/auto-unlock` 只允许远程控制端在全局及手动锁均未锁定时调用；普通 unlock 不能清手动锁，unlock-pool 不能清全局锁。
- storage_delete.go 与 driver_zfs.go 的必要原生接入：销毁前核对登记 GUID，只有原生 destroy 成功才持久化删除证明，支持整池清理及 DB 部分失败重试；重新注册同名存储时清除旧证明。导出/离线不构成证明。身份标记匹配且只剩空父池时才回收自建池；匹配真实文件 vdev 路径并核验 inode 后才删除文件，外部池与兄弟数据保留。导入子数据集不走同名原生 loop 文件删除，避免误删仍在使用的父池 vdev。各上游接入附近有 PayIncus 注释；LTS 同步须保留成功之后的证明顺序、source 归属及外部数据保护。
- storage_http_test.go 新增锁定身份读边界、外部根及系统挂载点保护；storage_delete_test.go 新增原生整池证明、prepare 不能证明成功、GUID/幂等、外部/兄弟/不匹配文件保护；TUI 测试确认不绕过节点分配器。Linux race、真实 daemon 路由运行及 go vet 已通过；最终全量检查结果按控制仓库收尾记录标注。
- 真机仅为已授权 bad（Debian 13/arm64，10.211.55.11），有 daemon/TUI 备份。Netcatty 原生 UI 一步创建，ego-browser 管理界面创建、导入、托管、单池锁盘/恢复及全局安全锁定/逐池解锁/恢复均已运行。LXC 核验原关机保持、其它池不因单池锁盘停止及数据标记完整。删除测试池后原生池/数据集/文件及控制端池/密钥均清理。节点无 /dev/kvm，未作 VM 真机验收；未发布或推送。


### 未锁定错误真实性复核（2026-10-29）

- 当前 canary 以 security.PublicErrorFor(err) 仅对 errors.Is(err, ErrLocked) 保留既有全局锁伪装；daemon、LXC/QEMU 启动、ZFS Mount/MountVolume 不把状态读取失败或手动锁盘伪装成数据库损坏。CLI 审计协议/权限/写入失败保留实际原因；审计异常后只有再次确认持久锁存在才使用伪装，仍拒绝执行。管理 HTTP 保留已有不含密钥的错误原因。LTS 同步须保留这些条件与 fail-closed 行为，不可重新用任意 err!=nil 代替已确认锁定。
- 复核新增 storage_resize.go 发现同类原因误判：ResizePool 将 Check/Status/属性读取异常合并为节点不可用或身份改变。现透传实际原因，手动锁盘独立说明；错误时仍不执行磁盘扩容，无新增 API、配置或上游接入点。security/errors_test.go 新增损坏状态下零存储命令回归，其它错误/权限/状态码/密钥脱敏测试沿用已有覆盖。
- GPT-6.1 Terra 在 owner 要求后恢复，完成多轮只读复审；发现的遗漏均已修正，最终针对性复审无新增必须修正问题。复审不等于自行运行测试或真机验证。
- 本轮检查通过：定制模块 race、真实 daemon 路由测试、实例/存储驱动 go vet、daemon 编译、增量 golangci-lint 0 issues；控制仓库 node-security/storage-encryption/resize、类型检查及 make check 通过。测试节点 10.211.55.11 当前 Netcatty 不通，本轮新增分支未重新部署实测；无其它节点变更、无提交或推送。

- 恢复复审后的补修：storage.go 将 GUID/加密根/托管格式/卸钥后属性读取错误与实际值不符分开；pool.go 与 storage_resize.go 保留系统/执行阶段错误，不猜测 busy、身份改变或文件系统已存在。属性失败后只有成功的 zpool list -H -o name 确认池未导入才进入创建/导入；池已存在或清单失败就停止，原失败原因保留。原有相同身份重试、已确认离线池的导入及无 force-overwrite 规则不变。无新配置/API/上游接入，LTS 同步须保留先读并确认再写的顺序。
- errors_test.go 覆盖 Ready/Unlock/UnlockManaged/Lock/Resize 的读取异常、管理标记第二次查询失败、卸钥验证失败、缺少命令/缺失设备、池清单失败或池已存在时零写，以及确认缺池后的导入成功/失败；pool_test.go 的缺池 fixture 加入真实只读清单契约。控制端 wait/cancel/长镜像等待与旧解锁路由的错误状态/脱敏/取消测试详见控制仓库 docs/NODE_SECURITY.md。安全 Runner 仍丢弃 stderr，系统执行错误只含阶段和 exit/errno 等安全信息，不声称还原 ZFS 内部所有具体原因。

- 恢复复审后的验证：security race（含新增全部用例）、定制模块 race、真实 daemon TestPayIncusAuthenticatedRoute、daemon/实例/存储/security go vet、daemon 构建及 security 增量 golangci-lint 0 issues 通过；控制端相关安全/存储/异步测试、类型检查和 make check 通过。未重新运行受既有容器限制的上游全仓检查，未将其写为通过；bad 节点未连接，本轮新增分支未重新部署实测。

### 立即上报测试与锁定存储恢复身份（2026-10-29）

- 新 heartbeat-confirm-v1 能力复用已有认证 WebSocket HEARTBEAT 帧，携带有界 UUID requestId；旧空 payload 通知仍兼容。连接器将测试请求排到下一次新采集之前，并把 heartbeatRequestId 放入正常 HMAC 上报，不添加监听端口。控制端只有对应请求完成持久化且采集完整后才确认测试，不按通知发送成功确认。协议/队列接入为 tunnel/tunnel.go、cmd/payincus-agent/main.go；接口测试 control_test.go/main_test.go 覆盖相关性和旧通知兼容。
- service.go 的锁定 report 只调用 report.LockedStoragePayload，不采集实例与主机资源；沿用锁定下既有存储身份白名单。report/storage.go 保留加密身份，区分实际密钥状态与 nodeLocked，锁定的普通池也不访问受限 resources；collectionComplete 区分部分失败。service_test.go/storage_test.go 覆盖这些边界。无新配置、数据库或上游源码接入点；LTS 同步保留白名单、签名上下文和采集失败语义。控制端实现/配套升级要求及验证见控制仓库 docs/STORAGE_HEARTBEAT.md。

- 已在授权 bad（Debian 13/arm64，10.211.55.11）以 SHA256 校验后备份替换本轮 daemon/connector；两服务 active/running，原 global lock 与存储登记保留。实际新签名心跳补齐 test/incus 加密身份，管理界面点击“测试”等待新报告落库后成功，真实存储页解密入口可见；未替用户解除 API 锁或加载数据集密钥。备份及完整控制端证据见 docs/STORAGE_HEARTBEAT.md。真实全局锁定下不采集实例/主机内容，普通池也不访问被锁定的资源 API。相关 race、真实 daemon 路由、go vet、构建、增量 lint 通过；Terra 最终只读复核无新增必改项，无发版或推送。

### 默认心跳间隔（2026-10-29）

连接器默认心跳由 10 秒改为 5 秒，新 TUI 连接配置写入 5 秒；既有显式配置与 5–3600 秒限制保留。采集与请求串行，采集耗时超过周期时不会并发执行。接口及状态报告沿用现有协议，无数据库变更；config_test.go 覆盖默认、显式及边界。


## 2026-10-29：内部发行 2.0.0、正常启动恢复与共享出口维护

**上游基线**：官方 `lxc/incus` 正式 LTS `v7.0.2`，提交 `01f81e5e607bbb5818410318e4b3496ad1820824`；2026-10-29 再次核对，未使用普通功能版或未发布提交。Incus 自身版本未修改。

### 内部版本与发行

- `scripts/payincus/VERSION` 为内部版本唯一事实源，当前 `2.0.0`。`build.sh` 把内部版本及本次 SHA 同时注入 Agent 和 TUI：正式 `v2.0.0`，开发 `v2.0.0-<12SHA>`。每次修改源代码重新构建都会更新 Agent 版本。
- 资产 `incus-2.0.0[-canary.<12SHA>]-linux-<OS>-<版本>-<架构>.deb`；manifest 保留真实 upstreamTag/Commit。软件包名仍为 `payincus-incus`，不改其它发行版包。
- `payincus.yml`：canary push、PR、人工 workflow_dispatch、`v*` tag 可集成；main 普通 push 不集成。正式 tag 必须与 VERSION 一致且提交属于 main。`payincus-publish.yml` 仅发布验证成功的 canary 产物，tag `canary-2.0.0-<12SHA>`。安装器严格选择该渠道、OS 与架构，不读取旧命名兼容包。

### 正常停止与自动恢复

- 安全模式 `enabled` 不再意味着重启即 `locked`。`Boot` 保留真正锁定，正常启动设置独立 `recoveryPending`，在控制端授权前阻止容器/VM 启动。控制通道和遥测继续工作。
- `security/lifecycle.go`：正常 Shutdown 先持久化运行工作负载的 project/name/UUID/frozen 状态，再停止实例，逐根卸载并验证 ZFS keystatus。每根错误独立汇总；未导入池已无加载密钥，冷启动不会为证明卸载而导入池。失败不伪报成功，不把正常停止标为安全告警。
- 冷启动由 incusd 自身先停止崩溃后残留实例并卸载仍驻留的密钥，准备流程不依赖连接器在线；失败会重试。连接器在本机未锁且存在待恢复任务时才申请密钥；本地状态读取失败不申请。控制端已锁、证明失败粘性锁、状态未知都不提供密钥。控制端暂不可达时自动重试。
- 外层复用现有 HMAC 节点身份认证与 PostgreSQL nonce 去重，确认身份后才校验内层 AES-256-GCM 证明。内层明文 token、毫秒时间戳、随机 nonce，时间容忍正负 180 秒（含边界），并独立持久化 nonce 防窗口内重放。密钥派生为 SHA256(`payincus-startup-v1\0` + 高熵 Agent secret)，96-bit 随机 IV，128-bit tag；AAD 绑定协议、方向、Agent ID。响应也加密并绑定请求 nonce。算法采用 AES-256-GCM；用户原始名称 ARCTEL 未对应到明确算法。
- 已确认身份的内层错误证明持久化 `recoveryBlocked`；未认证请求无法锁定任意节点。控制端立即尝试通过证书指纹固定的 mTLS API 下发锁定，并在后续安全报告响应重试。Agent 收到认证后的锁定决定后写本地锁，现有异步隔离流程继续关闭实例/卸 key。管理员成功执行恢复流程后方可清除控制端此标志。控制锁消息携带单调 generation；节点持久化已确认代数，恢复审批同时确认该代数，重复或延迟的旧锁消息不会把已恢复节点再次锁定，新一代告警仍会锁定。
- 只有已注册 GUID/指纹匹配且非手动锁定的加密根会加载密钥；密钥不写磁盘、不进入日志或 State。恢复按原生工作负载身份校验，正常停止前正在运行的实例恢复运行，Frozen 状态保留；冷启动同时复用原生 boot.autostart / volatile.last_state.power。正常启动恢复失败保留未完成清单并自动重试。`startupRecovering` 单独标记本机正常恢复执行器；真正告警与管理员恢复会清除此标记，本机循环不会抢跑控制端异步恢复清单或跳过控制端的实例资格检查。
- 系统强制 SIGKILL、OOM kill 或突然断电无法执行停机用户态代码；不承诺清理已被攻击者复制的内存或密钥。进程重启时重新执行准备流程，正常 systemctl stop 会执行停机卸 key 路径；ZFS 被外部进程占用时不会强制绕过内核失败。

**上游接入点**：`cmd/incusd/daemon.go` Boot 注释更新及 Stop 内、数据库关闭前调用定制 Shutdown 并返回清理错误；`cmd/incusd/instances.go` 正常启动等待授权，不误报锁定。原有 LXC/QEMU `BeginStart` 接入复用，未改上游 API。native adapter 位于独立 `cmd/incusd/payincus_security.go`，使用 `instance.LoadNodeAll/LoadByProjectAndName`、Start/Stop/Freeze 与 volatile.uuid。同步上游时复核 Stop 信号顺序、数据库存活期、setupChan 及 boot.autostart/volatile.last_state.power 语义。

**接口扩展**：`payincus_security_startup_v1`、`payincus_xray_maintenance_v1` 在节点根端点明确公布。

**接口**：本地 UNIX socket `GET/POST /1.0/payincus/security/startup`（TLS 远程不可调用），控制端 `POST /api/agent/startup-keys`（HMAC + AEAD、no-store）。原有 `/security/recover`、`/storage/auto-unlock` 仍保留明确管理语义；控制端后台存储加载器在 recoveryPending 时不执行，防绕过启动证明。

### 共享出口与 Xray TUI

- 出口组 key 由历史版本身份改为共享策略 `p<ID>`，内容变化通过现有 host revision 确认/重试，不维护策略版本或历史快照。负载均衡、故障转移、最低延迟走统一策略执行器：多节点组保留 Xray 原生 balancer/observatory，所有 probe-N 各自固定候选节点，DNS 通过 TCP 转换及 loopback 路由进入同一 balancer，不能固定首代理。故障转移通过 RoutingService 覆盖并回读目标，活跃候选失败立即切备，首选恢复需两次成功探测。每组隔离健康状态，按策略检测间隔刷新。额外内网 CIDR 仅在对应策略组生效，不影响其它组。
- `egress/maintenance.go` 集中只读脱敏目录、分批代理检测、托管核心重启与重建；`/1.0/payincus/xray` 仅供本机 TUI。GET 返回节点 ID/protocol/address/port 和状态，POST 固定 action=test/restart/reconfigure，禁止任意脚本或命令参数。
- 重建与应用/健康检查共用 applyGate，与探测共用 probeGate。先撤销就绪位，再停止托管核心，重建专属命名空间、veth、resolver、生成配置及 systemd unit；保留策略意图、ownership、流量分类及 unreachable 保护，不清除宿主全部 nft/路由/conntrack。失败保持阻断并可重试。仅管理已验证注册表中的资源，不动其它网络。
- TUI Xray 菜单：查看节点、测试、重启、日志、重新配置；调用本机 API，不调用 Incus CLI，因此不会被 CLI 安全模式误触发。输出只含允许的非机密字段。

### 测试与验证

接口对应测试：`security/lifecycle_test.go`、`security/security_test.go`、`panel/recovery_test.go`、`egress/*test.go`、`service_test.go`、`tools/incus-tui/*test.go`，发行契约 `scripts/payincus/test-install.py`。控制端相关接口测试在 PayIncus `test-node-security-startup.ts`、`test-node-security-db.ts`、`test-package-egress.ts`、`test-egress.ts` 与前端守卫。测试结果由本次测试记录及 CI 提供，未执行项不得视为通过。


**本次已验证（2026-10-29，GPT Luna）**：前后端类型、相关安全/出口 API 与隔离 PostgreSQL 集成、界面守卫通过；Debian 13 原生集成二进制完成编译；隔离 QEMU 的真实 ZFS 测试通过正常 Shutdown 卸 key → Boot 保持未锁待授权 → Startup 加载 key 后恢复工作负载回调、验证原加密数据可读，并通过既有扩容验证。完整上游 `make check` 含宿主系统集成测试，未在共享工作机执行；后续集成矩阵与发布结果另行记录，不将本地契约测试当作完整上游套件。

**部署约束**：Incus 数据库、控制连接配置及守护进程启动必须读取的系统目录/系统卷需位于可独立启动的非托管存储。托管加密根用于实例数据；把 Incus 自身元数据放进必须通过 Incus API 才能解锁的根会造成启动依赖环，不能据此承诺自恢复。
