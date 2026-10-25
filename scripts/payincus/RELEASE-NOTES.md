基于正式 Incus 7.0.2 LTS 的 PayIncus 定制构建，稳定/集成渠道以 release 的 prerelease 标记及随包 manifest 为准。

- 包含定制 incusd、CLI、官方 VM Guest Agent、受限用户服务、精简宿主连接 Agent、网络恢复工具和 Bubble Tea `incus-tui`。
- 新增 `scripts/payincus/install.sh latest|canary`，按发行版/架构下载 GitHub deb，校验后安装并进入 TUI。
- 支持 Debian 13、Ubuntu 24.04 的 amd64/arm64 原生 deb、归档、源码/上游基线清单及 SHA256。
- TUI 提供首次初始化、Panel 凭据/token 配置、mTLS 公钥证书导入、本地隧道监听、服务管理和实时日志；不覆盖已有资源。
- 节点扩展通过 /1.0/payincus 提供，沿用 Incus 认证授权。控制端支持新旧节点协议。
- 软件包不自动启动或重启服务；升级现有节点前应安排维护窗口并检查网络保护。
- 集成预发布不等于生产验收。平台限制、操作说明及测试记录见 FEATURES.md、scripts/payincus/INSTALL.md。
