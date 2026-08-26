# git-rg 支持与兼容策略

当前稳定版本为 v0.2.0。v0.x 只维护最新稳定版本：v0.2.0 发布后，v0.1.0 保留在 GitHub Releases 中供复现和回滚，但进入 EOL，不再接受普通缺陷、安全或兼容修复。支持策略的目标是让 agent 能明确判断一次搜索结果的完整性、输出契约和运行平台边界。

## 预构建兼容矩阵

以下六个组合是 Tier 1：每次 Release 都应构建、运行版本检查并通过发布验收。

| 操作系统 | 架构 | Release 资产 | GitHub Actions runner | 支持级别 |
| --- | --- | --- | --- | --- |
| Linux | amd64（x86_64） | git-rg_vX.Y.Z_linux_amd64.tar.gz | ubuntu-24.04 | Tier 1 |
| Linux | arm64 | git-rg_vX.Y.Z_linux_arm64.tar.gz | ubuntu-24.04-arm | Tier 1 |
| macOS | amd64（x86_64） | git-rg_vX.Y.Z_darwin_amd64.tar.gz | macos-15-intel | Tier 1 |
| macOS | arm64 | git-rg_vX.Y.Z_darwin_arm64.tar.gz | macos-15 | Tier 1 |
| Windows | amd64（x86_64） | git-rg_vX.Y.Z_windows_amd64.zip | windows-2025 | Tier 1 |
| Windows | arm64 | git-rg_vX.Y.Z_windows_arm64.zip | windows-11-arm | Tier 1 |

每个 Tier 1 archive 包含一个顶层版本目录，目录内只有对应平台的单个可执行文件，并由 Release checksum 保护；Release 还附带 install.sh 和 install.ps1。二进制运行不需要 Go；当前构建使用 CGO_ENABLED=0，不依赖本机 C 编译器。其他 GOOS/GOARCH 组合允许从源码构建，但属于 best effort，不承诺每个版本都有 archive、原生 runner 或专门 smoke。

## 运行时和 API 兼容性

- 源码构建和 go install 的最低 Go 版本为 1.26。最低版本只在 minor release 中提升，并会在发布说明和迁移说明中明确标记。
- GitHub.com 和 GitLab.com 是每次发布的主要 SaaS 验证目标。公开仓库和带有合适权限的私有仓库都在支持范围内，但请求仍受平台 API 配额、仓库权限、对象大小和服务可用性约束。
- GitHub Enterprise Server（GHES）和自建 GitLab 通过对应 REST API 提供 best effort 支持。v0.2.0 不承诺具体 GHES/GitLab Server 最低版本；部署方必须用目标实例的 --provider、--api-base、token 和代表性仓库做验收。
- 支持单仓库、单个 branch/tag/commit ref；不支持跨组织/跨 group 搜索、Git 历史搜索、替换、远端提交或通用 Git 服务器协议。
- 认证只使用 GITRG_TOKEN、公共 GitHub 的 GITHUB_TOKEN/GH_TOKEN 或公共 GitLab 的 GITLAB_TOKEN。token 不作为 CLI 参数，不写入日志或缓存；私有化 host 使用 GITRG_TOKEN。

## 搜索结果完整性

exact 会在指定不可变 commit 上扫描符合 glob 的普通文本文件；在可用的 archive 路径上流式读取内容，必要时完整回退到 tree/blob。正常结束时 summary.complete=true，但 --max-results、超时、请求失败、资源限制或取消都必须显式报告不完整。

auto 默认优先使用平台索引缩小候选并由本地 Go RE2 matcher 复核，但最终仍扫描指定 commit 的剩余内容；索引失败或不可用时继续 exact 路径。indexed 只搜索索引候选，summary.complete 永远为 false，适用于低延迟候选查询，不可作为全仓库未命中的证明。

默认最多返回 200 个匹配行；达到上限时 summary.truncated=true、summary.complete=false、reason="result_limit"。需要完整结果时显式使用 --max-results=0，并设置足够的 --timeout 与请求预算。搜索不 clone 目标仓库、不 checkout、不下载 Git 历史；exact 的最坏情况仍可能读取指定 commit 中全部符合条件的文本内容。

## CLI 与输出兼容性

版本使用 SemVer：

- patch：修复缺陷、安全问题和兼容问题，不改变既有 CLI 语义、退出码或结果完整性含义。
- minor：增加功能、平台或非破坏性字段；v0.x 如必须改变行为，必须在 release notes 和迁移说明中写明。
- breaking CLI 或 NDJSON 变化：提升主版本（v1.0 之后）或在 v0.x 发布说明中明确迁移；不能在 patch 中静默改变。

当前 NDJSON schema_version 为 1。schema v1 允许增加调用方可忽略的字段，但不会删除既有字段、改变字段类型或重定义 complete、truncated、reason 和退出码语义。需要不兼容的事件、类型或语义变化时，提升 schema_version 并提供迁移文档。

CLI flag 在删除前至少保留一个 minor 的弃用周期。0 匹配、1 无匹配、2 失败的退出码契约在 patch release 中保持不变。缓存是可删除的实现数据，不承诺跨版本格式兼容；内部 Go package 不构成公共 SDK。

## 生命周期

| 状态 | 含义 |
| --- | --- |
| Current | 最新稳定版本，接受普通缺陷、安全和兼容修复，并运行 CI/Release 验收 |
| EOL | 旧版本仍可下载和复现，但不接受普通修复；升级到 Current |
| Best effort | 非 Tier 1 平台、未列出的自建服务版本或源码构建组合，问题需要在目标环境复现 |

发布新稳定 minor/major 后，旧 v0.x 版本立即转为 EOL；不会维护长期稳定旧分支。安全问题可能在 release notes 中回溯说明，但不承诺为 EOL 版本提供补丁包。每个 Release 保留 checksums，便于审计和回滚。

## 反馈与安全问题

报告问题时请提供 git-rg --version、OS/架构、provider、运行模式、是否使用 --no-cache、脱敏后的 NDJSON meta/summary/error 和可复现命令。不要提交 token、私有仓库内容或包含私密路径的缓存文件。

安全问题请使用 GitHub Security Advisories 或私下联系维护者，不要在公开 issue 中粘贴凭据。修复是否进入 Current 版本、是否影响 EOL 版本，以安全公告和 release notes 为准。
