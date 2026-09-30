# git-rg 文档导航 / Documentation guide

当前稳定版为 **v0.7.0**。另有尚未发布的本地 **Gitee 适配器开发改动**，不包含在远端 `main` 中；使用它需要构建包含该改动的源码。Release 安装器、包管理器、`go install ...@v0.7.0` 和 `@latest` 不包含这些本地改动。

The stable release is **v0.7.0**. An **unreleased Gitee adapter** exists in local development changes and is not included in the published `main` branch. It requires a build containing those changes; release installers, package managers, and `go install ...@latest` do not provide unpublished local changes.

## 按任务查阅 / Guides by task

| 任务 / Task | 文档 / Guide |
| --- | --- |
| 快速开始、搜索模式、refs、输出、认证与缓存 / Search, refs, output, authentication, cache | [中文 README](../README.zh-CN.md) / [English README](../README.md) |
| 安装、升级、校验与卸载 / Install, upgrade, verify, uninstall | [Installation](installation.md) |
| 读取完整文件、JSON 变更计划、预览、草稿 PR/MR 与失败恢复 / Read, preview, publish, recover | [Agent changes](agent-changes.md) |
| issue 正文、Agent 身份、权限与不确定响应 / Create issues and handle uncertain responses | [Issue creation](issues.md) |
| Gitee 地址、凭证、索引限制与 API 差异 / Gitee usage and API differences | [Gitee](gitee.md) |
| 平台、API、schema 与版本生命周期 / Compatibility and lifecycle | [Support policy](../SUPPORT.md) |
| Agent 工具选型与工作流背景 / Agent workflow background | [中文技术文章](why-agents-need-remote-git-rg.zh-CN.md) · [PDF](../output/pdf/why-agents-need-remote-git-rg.zh-CN.pdf) |

## 功能与版本 / Availability

| 能力 / Capability | GitHub、GitLab | Gitee |
| --- | --- | --- |
| `auto` / `exact` 搜索，`refs` | v0.7.0 | 本地开发预览，尚未发布 / Local development preview |
| `indexed` 搜索 | 取决于平台索引与权限 / Subject to index availability | 不支持，返回 `indexed_search_unsupported` / Unsupported |
| `read`、`propose --dry-run` | 自 v0.5.0 起 / Since v0.5.0 | 本地开发预览，尚未发布 / Local development preview |
| `propose --enable-write`：同仓库新分支、commit、草稿 PR/MR | 自 v0.5.0 起 / Since v0.5.0 | 本地开发预览，尚未发布 / Local development preview |
| 仅配置 `GITRG_WRITE_TOKEN` 的读取回退 / Single-token read fallback | 自 v0.6.0 起 / Since v0.6.0 | 本地开发预览，尚未发布 / Local development preview |
| `issue create`、本地 issue 预览 / Issue creation and local preview | 自 v0.7.0 起 / Since v0.7.0 | 本地开发预览，尚未发布 / Local development preview |

写入每次都要求 `--enable-write` 和具备对应权限的 `GITRG_WRITE_TOKEN`；`--dry-run` 优先。提案预览会读取远端，issue 预览完全在本地。搜索检查 `summary.complete`；发布检查 `result.complete`，后者不表示编译、测试或 CI 通过。

Writes require `--enable-write` and a suitably permitted `GITRG_WRITE_TOKEN` on every invocation; `--dry-run` takes precedence. Proposal previews read remote files; issue previews are entirely local. Search completeness uses `summary.complete`; publication uses `result.complete`, which does not certify builds, tests, or CI.

## 发布记录 / Release history

[v0.7.0](releases/v0.7.0.md) · [v0.6.0](releases/v0.6.0.md) · [v0.5.0](releases/v0.5.0.md) · [v0.4.1](releases/v0.4.1.md) · [v0.4.0](releases/v0.4.0.md) · [v0.3.0](releases/v0.3.0.md)

发布说明记录对应版本的行为、迁移和当时的验证，旧版不作为当前使用手册；最新稳定版之外的 v0.x 已 EOL。需要复现旧版时，使用发布说明中固定 tag 的文档链接。v0.1.0 和 v0.2.0 的记录见 [GitHub Releases](https://github.com/SamuelSupe/git-rg/releases)。

Release notes preserve version-specific behavior, migration, and validation history. Earlier v0.x releases are EOL. Use their tag-pinned documentation links when reproducing an older release; use the guides above for current behavior.
