<!-- PayIncus customization: repository rules; keep the upstream instructions below intact. -->
# PayIncus 定制规则

## 默认开发分支与开工同步

- 默认在 `canary` 分支开发；用户没有明确指定时，不得以 `main` 或旧的本地分支作为开发起点。
- 开始修改前先检查工作区和分支，执行 `git fetch origin`，切换到 `canary` 并使用 `git merge --ff-only origin/canary` 同步最新代码，再进行开发。已有未提交改动必须保留；存在分叉时先核对提交，不得强制重置、覆盖或盲目合并。
- `main` 的同步和发布遵循用户明确指令；要求推送两条分支时，先核对差异并保留各分支已有改动，禁止强推或推送到 `work`。
- 本条约定的是 fork 的默认开发分支；官方上游基线仍仅允许下述已正式发布的 LTS tag。

## 最高工程原则：最小化上游代码改动

- 在满足需求、正确性和兼容性的前提下，最少化修改上游代码；优先复用官方 API、扩展机制和已有生命周期入口。
- 定制实现应集中在独立模块，通过尽可能少的接入点连接上游。禁止无关重构、批量格式化、改名、搬迁文件或修改生成文件来扩大差异。
- 必须修改上游代码时，在每个独立改动点附近留下 `PayIncus:` 注释，说明修改原因、定制接入点及需要保留的行为；复杂原因可链接 FEATURES.md 对应条目。注释使用文件支持的语法，通常保持一行。
- 不支持注释的文件及自动生成文件，不得加入破坏格式的注释；在生成源或最近的可注释接入点标记，并在 FEATURES.md 精确记录文件路径、原因与生成方式。
- 下述规则是本 fork 的定制要求；原有上游法律、格式和验证要求保留。接口测试是必要覆盖，不属于应避免的重复或过量测试。

## 功能文档

- 所有功能行为改动、新增特性及修复必须在同一变更中更新根目录 FEATURES.md，不能只写提交说明。
- 每个条目记录：目的、行为与限制、配置和兼容性、代码接入点、涉及的上游 API/扩展、测试位置与验证结果；不适用项明确说明。
- 上游源码改动还必须记录原因及后续上游同步时的核对事项；移除或替代功能时同步更新条目。
- 规划必须标为未实现，不得把设计、未运行测试或未部署功能写成已完成。

## LTS 正式发布版本基线与同步

- 上游始终以官方 lxc/incus 已正式发布的 LTS 版本为准，只同步 LTS release tag 对应提交。普通功能版、上游 main、开发分支、预发布版本及未发版提交均不得作为运行、开发或交付的上游基线。
- 在 FEATURES.md 记录上游仓库、版本系列、release tag、完整提交 SHA 和核对日期；定制提交可建立在该正式版本之上。
- 开始开发或执行发布前检查官方 LTS release。当前 LTS 系列每次发布维护版本后进行同步、验证并更新记录；普通功能版不纳入同步。新 LTS 系列正式发布后，以其为升级目标，完成兼容性、数据迁移和定制功能验证后再升级；未完成验证前继续维护原 LTS 基线，不得切换到普通功能版或静默降级。
- 同步时以已记录的旧 tag 和目标 LTS release tag 比较差异，在隔离分支上合并目标 tag，再逐项核对定制接入点、API 契约、启动顺序、数据格式和配置兼容性。不得合并普通功能版、上游 main 或用未发版提交解决冲突。
- 遇到冲突不能机械地选择 ours/theirs；优先让上游文件恢复原有结构，把定制代码移到独立模块或稳定扩展入口，缩小后续冲突面。必要冲突解决需记录原因、回归用例及降低后续冲突的措施。
- 同步完成后更新 FEATURES.md 的基线和同步记录，并运行相关接口测试、回归检查及上游要求的检查。无法消除的风险必须如实记录。
- 本规则要求持续同步维护，不代表已配置自动任务；如需自动检查或同步，另行实现并记录其触发方式与失败处理。

## 上游 API 接线与接口测试

- 开发定制功能前核对所选正式 LTS 版本的 API、客户端定义及 API extensions，明确调用方、参数、认证/授权、返回值、异步 operation 与错误语义。
- 调用上游接口集中经过适配层，避免在业务模块散落版本判断或直接依赖内部数据库和不稳定实现；必需接入内部接口时记录签名、生命周期和同步检查点。
- 对可选 API 能力使用 API extension 探测或等价明确能力检查；能力缺失时给出可诊断错误或经过测试的降级，不能仅凭版本字符串猜测兼容性。
- 每个被定制功能调用、封装或新增/修改的接口必须有对应的单元测试。覆盖正常响应、参数与请求构造、错误传播，以及该接口适用的权限失败、超时/取消、能力缺失和响应格式变化。
- 异步接口覆盖 operation 成功、失败及中断处理；流式接口覆盖断线与资源清理；会重试的写接口覆盖幂等与状态核对，防止重复创建或重复执行。
- 测试 mock/fixture 必须依据正式 LTS 版本的真实契约，避免仅复制实现假设；涉及真实接线的改动还应通过适当集成测试验证路由、认证和生命周期。
- 上游同步后逐一复核接口测试及真实契约；不得为通过测试而删除断言、忽略错误或盲目更新 fixture。FEATURES.md 应维护接口与测试的对应关系。

---

<!-- PayIncus customization boundary: original upstream AGENTS.md follows. -->

# Legal

 - All contributions to this repository must be compatible with the Apache 2.0 license.
 - Specifically (but not limited to), contributions cannot include code licensed under the terms of the GPL, AGPL or LGPL licenses.
 - Only human beings are allowed to sign the Developer Certificate of Ownership (DCO / Signed-off-by).
 - Only human beings can ever be credited within commit messages.

# Formatting

 - Code comments should be no longer than one line, unless they are required to cover complex unintuitive logic.
 - Commit messages should similarly be kept as short and to the point as possible, no need to summarize the whole issue.
 - We don't use the define and test one line `if` syntax, instead splitting defintion and testing across two lines.

# Testing / validation

 - The commit structure described in `CONTRIBUTING.md` should generally be followed.
 - All branches are expected to pass `make static-analysis` and `go test -v ./...`.
 - Excessive unit tests are generally discouraged.
 - When possible, existing system tests should be extended to cover new features.
 - A full local system test run isn't required prior to contribution, all tests get run in our CI.
