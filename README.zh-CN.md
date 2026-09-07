# git-rg

[![CI](https://github.com/SamuelSupe/git-rg/actions/workflows/ci.yml/badge.svg)](https://github.com/SamuelSupe/git-rg/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SamuelSupe/git-rg)](https://github.com/SamuelSupe/git-rg/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/SamuelSupe/git-rg/total)](https://github.com/SamuelSupe/git-rg/releases)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/doc/install)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

[English](README.md) | [简体中文](README.zh-CN.md)

> 在不可变 commit 上搜索 GitHub 和 GitLab 仓库，不 clone 目标仓库。

`git-rg` 是一个用 Go 编写的远程代码搜索命令行工具。它会先把分支、tag 或 commit 解析为固定的 commit SHA，再按所选搜索路径从 API 按需读取内容，默认输出适合 agent 消费的 NDJSON。它支持公开、私有以及自建的 GitHub/GitLab 实例；不会创建本地工作树，也不会下载 Git 历史。

<a id="whats-new-in-v030"></a>
## v0.3.0 更新

- **完整扫描 commit 内容**：exact/auto 核验完整 tree 和 archive 中的 blob ID，补读被 archive 导出规则遗漏或改写的文件。
- **更充分地复用缓存**：缩小范围的查询在应用下载门槛前利用已校验的 blob 缓存；未命中时限制探测开销。
- **减少分配与内存开销**：流式筛选 tree、按字节匹配文本、预编译 glob，并使用有界的内存结果缓冲，减少重复处理。
- **更高效地输出 NDJSON**：缓冲小块写入，同时立即输出首条匹配。

完整变更和升级说明见 [v0.3.0 发布说明](docs/releases/v0.3.0.md)。CLI 参数和 NDJSON schema v1 保持兼容。旧缓存会自动重新构建，升级后的首次查询可能重新下载内容。

## 安装并运行

Linux/macOS，安装到用户目录：

```sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh \
  | sh -s -- --version v0.3.0 --bin-dir "$HOME/.local/bin"
git-rg --version
```

Windows PowerShell：

```powershell
$installer = Join-Path $env:TEMP "git-rg-install.ps1"
Invoke-WebRequest https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.ps1 -OutFile $installer
& $installer -Version v0.3.0
git-rg --version
```

安装脚本会下载 GitHub Release archive，校验 SHA-256，检查 archive 结构，再安装对应平台的单个二进制文件。不需要 `sudo`、Git 或 Go。需要审计、升级和卸载说明时，请看[安装文档](docs/installation.md)。

## 快速开始

```sh
# 搜索公开仓库；目标仓库不会被 clone。
git-rg -F "TODO" github:OWNER/REPO

# 默认输出 NDJSON。所有 flag 放在 PATTERN 和 REPOSITORY 之前。
git-rg --mode exact --format ndjson --glob '*.go' TODO github:OWNER/REPO

# 搜索 branch、tag 或 commit，并输出前后各两行上下文。
git-rg --ref v1.2.3 --context 2 TODO https://github.com/OWNER/REPO.git

# 不 clone 仓库，列出分支头和 tag。
git-rg refs github:OWNER/REPO
```

除非使用 `-F/--fixed-strings`，`PATTERN` 会按 Go 正则表达式解释。需要完整审计时使用 `--mode exact`，详见[搜索模式与完整性](#search-modes-and-completeness)。搜索私有仓库前，请按[权限与凭证最佳实践](#permissions--credential-best-practices)设置最小权限 token。

## 目录

- [v0.3.0 更新](#whats-new-in-v030)
- [为什么不需要 clone](#why-no-clone)
- [安装](#install)
- [仓库地址](#repository-addresses)
- [搜索模式与完整性](#search-modes-and-completeness)
- [列出分支和 tag](#list-branches-and-tags)
- [CLI 参数](#cli-reference)
- [输出](#output)
- [退出码](#exit-codes)
- [权限与凭证最佳实践](#permissions--credential-best-practices)
- [缓存](#cache)
- [资源与平台边界](#resource-and-platform-boundaries)
- [开发、支持与许可证](#development-support-and-license)

<a id="why-no-clone"></a>
## 为什么不需要 clone

- 对目标仓库不会执行 clone、checkout，不创建本地工作树，不调用 Git，也不下载 Git 历史。agent 可以直接查询远程 snapshot，不必先管理一个与仓库大小相当的目录。
- 内容读取前会先把 ref 解析为不可变 commit。exact/auto 先读取完整 commit tree，按 blob ID 核验 archive 条目，缺失或被改写的内容由 blob API 补读，再用 Go RE2 引擎在本地匹配。
- `exact` 仍可能读取所选 commit 中所有符合条件的普通文本文件。“不 clone”省去工作树和历史传输，并不表示完整搜索不读取仓库内容或不需要网络。
- SSH 风格的 clone URL 只用于解析地址。`git-rg` 不使用 SSH 认证，也不使用 Git 传输。

当前 provider 只有 GitHub 和 GitLab。不提供离线模式、本地路径搜索、写操作、历史搜索或通用 Git 服务器协议。预构建二进制通过 GitHub Releases 发布。

<a id="install"></a>
## 安装

v0.3.0 是当前支持版本。此前的 v0.x 版本仍可下载用于复现或回滚，但已经 EOL；兼容性和生命周期策略见 [SUPPORT.md](SUPPORT.md)。预构建二进制运行时不需要 Go；源码构建和 `go install` 需要 Go 1.26 或更高版本。

### 预构建平台矩阵

以下六种组合属于 Tier 1，每个 Release 都会提供：

| 操作系统 | 架构 | v0.3.0 资产 | 支持级别 |
| --- | --- | --- | --- |
| Linux | amd64（x86_64） | `git-rg_v0.3.0_linux_amd64.tar.gz` | Tier 1 |
| Linux | arm64 | `git-rg_v0.3.0_linux_arm64.tar.gz` | Tier 1 |
| macOS | amd64（x86_64） | `git-rg_v0.3.0_darwin_amd64.tar.gz` | Tier 1 |
| macOS | arm64 | `git-rg_v0.3.0_darwin_arm64.tar.gz` | Tier 1 |
| Windows | amd64（x86_64） | `git-rg_v0.3.0_windows_amd64.zip` | Tier 1 |
| Windows | arm64 | `git-rg_v0.3.0_windows_arm64.zip` | Tier 1 |

每个 archive 包含一个顶层版本目录和一个可执行文件。v0.3.0 Release 有 9 个资产：6 个平台 archive、`install.sh`、`install.ps1` 和 `checksums.txt`。checksum 文件覆盖两个安装脚本和 6 个 archive。

### GitHub Release（手工下载）

从 [v0.3.0 Release](https://github.com/SamuelSupe/git-rg/releases/tag/v0.3.0) 下载匹配的资产和 `checksums.txt`，解压前先校验：

```sh
version=v0.3.0
asset="git-rg_${version}_linux_amd64.tar.gz"
base="https://github.com/SamuelSupe/git-rg/releases/download/${version}"
curl -fL -o "$asset" "$base/$asset"
curl -fL -o checksums.txt "$base/checksums.txt"
grep " $asset$" checksums.txt | sha256sum -c -
tar -xzf "$asset"
"./git-rg_${version}_linux_amd64/git-rg" --version
```

macOS 如果没有 `sha256sum`，可改用 `shasum -a 256`；按机器选择 `darwin_amd64` 或 `darwin_arm64` 资产。Windows PowerShell 可使用 `Get-FileHash` 和 `Expand-Archive`，完整示例见[安装文档](docs/installation.md)。未完成 checksum 和 archive 结构校验前，不要执行其中的二进制文件。

### `install.sh`（Linux/macOS）

脚本支持 amd64 和 arm64，默认安装到 `$HOME/.local/bin`，不请求 root 权限：

```sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh \
  | sh -s -- --version v0.3.0 --bin-dir "$HOME/.local/bin"
```

使用 `--version VERSION` 固定版本；省略时使用 latest。使用 `--bin-dir DIRECTORY` 指定安装目录。需要可审阅的安装过程时，先下载并检查脚本，再执行它。完整参数和失败处置见 [docs/installation.md](docs/installation.md)。

### `install.ps1`（Windows）

脚本支持 Windows amd64 和 arm64。默认安装到 `%LOCALAPPDATA%\Programs\git-rg\bin`，除非指定 `-NoPathUpdate`，否则会更新当前用户的 `PATH`：

```powershell
$installer = Join-Path $env:TEMP "git-rg-install.ps1"
Invoke-WebRequest https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.ps1 -OutFile $installer
& $installer -Version v0.3.0
git-rg --version
```

使用 `-InstallDir DIRECTORY` 指定目标目录，使用 `-NoPathUpdate` 保持 `PATH` 不变，使用 `-Version VERSION` 固定 Release。

### Homebrew

```sh
brew install SamuelSupe/tap/git-rg
git-rg --version

# 升级或卸载
brew upgrade SamuelSupe/tap/git-rg
brew uninstall git-rg
```

Formula 使用对应的 GitHub Release archive 和 SHA-256 值。

### Scoop

```powershell
scoop bucket add samuelsupe https://github.com/SamuelSupe/scoop-bucket
scoop install git-rg
git-rg --version

# 升级或卸载
scoop update git-rg
scoop uninstall git-rg
```

### `go install` 和源码构建

需要 Go 1.26 或更高版本：

```sh
# 固定到当前支持版本。
go install github.com/SamuelSupe/git-rg/cmd/git-rg@v0.3.0

# 或跟随最新模块版本。
go install github.com/SamuelSupe/git-rg/cmd/git-rg@latest
```

源码构建时只 clone 本工具的源码仓库，不要把之后要搜索的目标仓库 clone 到本地：

```sh
git clone https://github.com/SamuelSupe/git-rg.git
cd git-rg
go build -trimpath -o ./git-rg ./cmd/git-rg
./git-rg --version
```

其他 `GOOS/GOARCH` 组合可以用 Go 1.26 构建，但属于 best effort，不提供 Tier 1 archive 或专门的 Release smoke 保证。

<a id="repository-addresses"></a>
## 仓库地址

支持以下形式：

```text
github:OWNER/REPO
gitlab:GROUP/PROJECT
https://HOST/OWNER/REPO.git
git@HOST:GROUP/PROJECT.git
```

公开云服务示例：

```sh
git-rg TODO github:OWNER/REPO
git-rg -F "release marker" https://github.com/OWNER/REPO.git
git-rg --provider gitlab TODO gitlab:GROUP/PROJECT
git-rg --ref v1.2.3 TODO https://gitlab.com/GROUP/PROJECT.git
```

GitHub Enterprise Server 和自建 GitLab 必须显式指定 provider。默认 API 基址分别是 `https://HOST/api/v3` 和 `https://HOST/api/v4`；部署使用不同路径时用 `--api-base` 覆盖：

```sh
export GITRG_TOKEN='read-only-token'
git-rg --provider github TODO https://github.example.com/OWNER/REPO.git

export GITRG_TOKEN='read-only-token'
git-rg --provider gitlab \
  --api-base https://gitlab.example.com/api/v4 \
  TODO git@gitlab.example.com:GROUP/PROJECT.git
```

`--api-base` 必须是绝对的 `http://` 或 `https://` URL，不能含用户信息、查询串或片段。生产环境使用 HTTPS。GitHub 路径必须是 `OWNER/REPO`；GitLab 路径可以包含多级 group。

<a id="search-modes-and-completeness"></a>
## 搜索模式与完整性

命令形式是 `git-rg [FLAGS] PATTERN REPOSITORY`。所有模式都会先把 `--ref`（或 provider 的默认分支）解析为不可变 commit。`--ref` 可接 branch、tag 或 commit SHA，不会切换本地 checkout。

| 模式 | 行为 | 完整性含义 |
| --- | --- | --- |
| `exact` | 先读取不可变 commit 的完整 tree，不调用代码索引。所选 archive 文件按 Git blob ID 核验，归档中缺失或被改写的文件从 blob API 补读。若 archive 在读取首个普通文件前不可用，则使用 tree 和 blob 扫描。 | 正常结束时为 `summary.complete=true`。结果上限、超时、请求/provider 错误、取消或资源上限都会使结果不完整。这是完整审计模式。 |
| `auto`（默认） | 与 exact 使用相同的完整 tree 和经核验的 archive/blob 扫描。命中 archive 缓存时跳过索引；未命中缓存、模式有字面前缀且请求预算有余量时，最多预取 16 个索引候选，然后扫描剩余文件。 | 最终扫描是所选 commit 上的精确扫描，除非结果上限或硬错误提前终止。索引候选只是加速；索引 warning 不会把完整 archive 扫描变成不完整。 |
| `indexed` | 只搜索 provider 索引候选，校验、按 glob 过滤、去重路径，再读取所选 commit 上的候选文件。不使用 archive，索引失败时也不会回退为全量扫描。 | `summary.complete=false` 固定不变，reason 为 `indexed_mode`。覆盖范围和可用性取决于 provider 索引，未命中不能证明该 commit 没有匹配。 |

完整 tree 不可用或分页未完成时，exact/auto 不会声明结果完整。tree 分页计入 `--max-requests`，大型仓库可能需要提高请求预算。archive 的 `export-ignore`、`export-subst` 和 LFS 展开不会改变实际搜索的 commit 原始内容。

显式 glob 缩小搜索范围时，exact/auto 在应用下载门槛前利用通过校验的 blob 缓存；全部命中时，无需 archive、索引或 blob 下载。对于剩余未命中的文件，可以直接下载最多 8 个 blob：已知大小合计须不超过 8 MiB；缺失多个文件时，每个文件须有已知大小，且数量不超过 tree 的四分之一。单个大小未知的缺失文件也可使用该路径。请求预算须为重试留出余量，这些下载门槛不限制缓存命中。发现超过 8 个未命中文件后便停止探测，由原有 archive/索引策略处理剩余文件。blob 预读失败时回退 archive，已成功读取的文件不会重复输出。

`indexed` 要求 Go 能从模式中提取出非空字面前缀。GitHub 和 GitLab 的候选请求最多 10 页、每页 100 项，并受 8 MiB 候选路径预算限制；provider 的覆盖范围仍可能不同。不要假设所有 indexed search 都可用或完整。

完整审计请使用 `exact`；`indexed` 的不完整语义是固定契约；`auto` 会在结果上限或硬错误之外，最终覆盖整个 commit 做精确扫描。

### Glob 和上下文

`-g/--glob` 可以重复使用。包含 `/` 的 glob 按完整仓库路径匹配，不含 `/` 的按 basename 匹配。支持 `*`、`?`、字符组和递归段 `**`。以 `!` 开头表示排除。只要提供了正向 glob，路径就必须先命中正向规则；最后一个命中的规则决定结果。

```sh
git-rg --mode exact --glob '*.go' --glob '!vendor/**' TODO github:OWNER/REPO
```

`-C/--context NUM` 同时请求匹配前后的 NUM 行。`-B/--before-context NUM` 和 `-A/--after-context NUM` 可以分别设置两侧，并覆盖 `-C` 对应的一侧。

### 请求和结果预算

`--max-results` 默认最多输出 200 个匹配行；`0` 表示不限制。达到上限会提前停止，设置 `summary.truncated=true`、`summary.complete=false` 和 `summary.reason="result_limit"`。只要至少找到一行匹配，进程仍可能返回 `0`。`--max-requests` 默认允许 100 个远端 HTTP 请求，包含重试和重定向；`0` 表示不限制。`--timeout` 默认 5 分钟，是网络和本地扫描工作的统一截止时间。

<a id="list-branches-and-tags"></a>
## 列出分支和 tag

```text
git-rg refs [FLAGS] REPOSITORY
```

`refs` 使用 provider 的 branch 和 tag API，并完整分页。结果会校验、去重、稳定排序；因为 ref 会变化，所以不写入磁盘缓存：

v0.3.0 的接口是 `refs` 子命令（list-refs 操作），没有 `--list-refs` flag。

```sh
git-rg refs github:OWNER/REPO
git-rg refs --kind branch gitlab:GROUP/PROJECT
git-rg refs --kind tag gitlab:GROUP/PROJECT
```

参数为 `--kind all|branch|tag`、`--format ndjson|text`、`--max-requests NUM`、`--provider github|gitlab`、`--api-base URL` 和 `--timeout DURATION`。完整列表每页最多 100 项。后续页失败时返回 error，不会把部分列表标为完整。

NDJSON 正常事件顺序为 `meta`、每项一个 `ref` event、`summary`：

```ndjson
{"type":"meta","schema_version":1,"provider":"github","repository":"https://github.com/OWNER/REPO"}
{"type":"ref","kind":"branch","name":"main","commit":"0123456789abcdef","head_tags":["v1.2.0"]}
{"type":"ref","kind":"tag","name":"v1.2.0","commit":"0123456789abcdef","head_branches":["main"]}
{"type":"summary","summary":{"branches":1,"tags":1,"api_requests":2,"api_retries":0,"request_limit":100,"complete":true}}
```

`head_tags` 和 `head_branches` 只报告当前 head 指向完全相同 commit SHA 的名称，不证明历史上的分支归属或祖先关系；`git-rg` 不下载 commit 历史。使用 `--kind branch` 或 `--kind tag` 时只查询该类 ref，因此不会生成跨类型关联字段。如果搜索模式本身是 `refs`，使用 `git-rg -- refs REPOSITORY`，避免被识别为子命令。

<a id="cli-reference"></a>
## CLI 参数

所有 flag 必须放在 `PATTERN REPOSITORY` 之前。

| 参数 | 默认值 | 作用 |
| --- | --- | --- |
| `-F`、`--fixed-strings` | 关闭 | 将 `PATTERN` 当作固定字符串。 |
| `-i`、`--ignore-case` | 关闭 | 忽略大小写。 |
| `-w`、`--word-regexp` | 关闭 | 要求 Unicode 单词边界；字母、数字、标记和 `_` 是单词字符。 |
| `-g`、`--glob GLOB` | 不限制 | 包含 glob；前缀 `!` 表示排除，可重复。 |
| `-B`、`--before-context NUM` | `0` | 输出匹配前 NUM 行。 |
| `-A`、`--after-context NUM` | `0` | 输出匹配后 NUM 行。 |
| `-C`、`--context NUM` | `0` | 输出匹配前后各 NUM 行。 |
| `--ref REF` | provider 默认分支 | 使用 branch、tag 或 commit SHA。 |
| `--commit-info` | 关闭 | 输出 commit author、committer、时间和 message。 |
| `--mode MODE` | `auto` | 选择 `auto`、`exact` 或 `indexed`。 |
| `--format FORMAT` | `ndjson` | 选择 `ndjson` 或 `text`。 |
| `--max-results NUM` | `200` | 最多输出的匹配行数；`0` 表示不限制。 |
| `--max-requests NUM` | `100` | 远端请求预算，包含重试和重定向；`0` 表示不限制。 |
| `--provider NAME` | 自动判断 | `github` 或 `gitlab`；私有/自建 host 必须指定。 |
| `--api-base URL` | 自动判断 | 覆盖 provider API 基址。 |
| `--no-cache` | 关闭 | 禁用本次运行的持久化磁盘缓存读写和清理。 |
| `--timeout DURATION` | `5m` | 整个命令的截止时间，例如 `30s` 或 `2m`。 |
| `--version` | — | 输出版本并返回 `0`；不需要 pattern 或 repository。 |
| `-h`、`--help` | — | 输出帮助并返回 `0`。 |

`--commit-info` 复用解析 ref 时返回的 commit 数据，不增加 API 请求。author 是 commit 中记录的原作者，committer 是创建该 commit 对象的人。GitHub 可能提供关联的 username；GitLab commit 响应通常只有姓名和 email，没有可靠的平台 username。

<a id="output"></a>
## 输出

### NDJSON（默认）

标准输出每行一个 JSON 对象。每个 event 都有 `type`；预检失败时可能只有 `error`，没有 `meta` 或 `summary`。NDJSON 模式下，warning 和 error 还会以转义后的一行诊断写到 stderr。

| Event | 主要字段 |
| --- | --- |
| `meta` | `schema_version`（当前为 `1`）、`provider`、`repository`、`requested_ref`、`resolved_ref`、`commit`、`mode`；可选 `commit_info`。 |
| `match` | `path`、从 1 开始的 `line`、从 1 开始的字节偏移 `column`、行 `text` 和 `submatches`。 |
| `context` | `path`、从 1 开始的 `line`、行 `text`，以及值为 `before` 或 `after` 的 `context`。 |
| `warning` / `error` | 稳定的 `code` 和 `message`。 |
| `summary` | 嵌套的 `summary`，包含计数、字节/请求统计、`complete`、`truncated`、`duration_ms`，以及可选的 `reason`、`transport`、`rate_limit`。 |

搜索命令的 `duration_ms` 包含参数处理、ref 解析、缓存维护、扫描和 summary 之前的结果输出；不包含进程启动与 summary 自身的写出。

搜索命令使用 64 KiB 缓冲批量写出 NDJSON。meta、首条 match、warning、error 和 summary 会立即刷新；尚未写出的结果也会通过 100 ms 定时器刷新，方便管道消费方及时读取零散命中。

`submatches` 是该行每个完整命中的数组，不是捕获组数组。每项形如 `{ "start": 0, "end": 4, "text": "TODO" }`，start/end 是从 0 开始、end-exclusive 的字节偏移。空行的 match/context event 仍保留 `"text":""`。`transport` 可能是 `archive`、`blob_fallback` 或 `blob`。

示例（字段值仅作说明）：

```ndjson
{"type":"meta","schema_version":1,"provider":"github","repository":"https://github.com/OWNER/REPO","resolved_ref":"main","commit":"0123456789abcdef","mode":"exact"}
{"type":"match","path":"README.md","line":3,"column":1,"text":"TODO: document the API","submatches":[{"start":0,"end":4,"text":"TODO"}]}
{"type":"summary","summary":{"matched_lines":1,"matched_files":1,"scanned_files":2,"cache_hits":0,"downloaded_bytes":734,"skipped_binary":0,"api_requests":4,"api_retries":0,"request_limit":100,"transport":"archive","complete":true,"truncated":false,"duration_ms":18}}
```

### Text

使用 `--format text` 时，匹配行格式为 `path:line:column:text`，上下文行格式为 `path-line-text`。meta 和 summary 不写到 stdout，warning/error 写到 stderr。结果不完整时，stderr 输出带有原因和截断状态的 `incomplete_results`。指定 `--commit-info` 后，commit 元数据会先于匹配结果输出。

<a id="exit-codes"></a>
## 退出码

- `0`：找到至少一行匹配且命令没有失败。结果上限截断和 `complete=false` 的 indexed 结果也可能返回 `0`。
- `1`：命令完成但没有找到匹配行。对 indexed 来说，这只表示 provider 候选集中没有匹配。
- `2`：参数、pattern、glob、仓库、provider、API、输出、超时/取消、资源上限或请求预算失败；indexed pattern 没有字面前缀、或 indexed 请求失败也返回 `2`。

`index_unavailable` 等 warning 本身不会改变退出码。`SIGINT` 和 `SIGTERM` 会取消命令；除非命令此前已经因 `result_limit` 停止，否则搜索不完整并返回 `2`。`--version` 不访问 provider，直接返回 `0`。

<a id="permissions--credential-best-practices"></a>
## Permissions & credential best practices / 权限与凭证最佳实践

`git-rg` 只在 provider 要求的请求头中发送凭证。请使用能够读取目标仓库及其元数据的最小只读身份。

### GitHub

- 优先使用[细粒度 personal access token](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)，仅授权选定的目标仓库，并授予 `Contents: read`。根据 GitHub/GHES 策略启用的 endpoint，参阅[细粒度权限要求](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens)。
- classic PAT 的 `repo` scope 更宽。只有组织策略或 GHES 部署确实要求时才使用，并让该身份保持只读；同时遵循 GitHub 的[凭证安全建议](https://docs.github.com/en/rest/authentication/keeping-your-api-credentials-secure)。

### GitLab

- 优先使用 project/group access token，或受限 PAT，授予文档中的 `read_api` scope，并确保账号/机器人具备读取该项目的权限。参阅 GitLab 的[访问 token scope](https://docs.gitlab.com/security/tokens/access_token_scopes/)和[Repository Files API](https://docs.gitlab.com/api/repository_files/)。
- 不要为这个只读搜索工具授予 `api` 或 `write_repository`。不要把 `read_repository` 单独当作覆盖所有 project metadata、ref、tree、search、archive 和 raw-file API 的保证；可用 scope 还取决于 GitLab 版本和实例策略。

### Token 读取顺序与处理

读取顺序是固定且有意收窄的：

| 目标 | 环境变量顺序 |
| --- | --- |
| 任一 provider，包括自建实例 | 总是先检查 `GITRG_TOKEN`；非空时它优先。 |
| `github.com` | 在 `GITRG_TOKEN` 之后读取 `GITHUB_TOKEN`，再读取 `GH_TOKEN`。这两个云端变量不会用于自建 GitHub。 |
| `gitlab.com` | 在 `GITRG_TOKEN` 之后读取 `GITLAB_TOKEN`。该云端变量不会用于自建 GitLab。 |
| 自建 host | 使用 `GITRG_TOKEN`；没有按 host 区分的后备变量。 |

没有 token CLI 参数。仓库 URL 和 `--api-base` 会拒绝内嵌凭证，所以不要把 token 放进 URL。未设置 token 时公开 endpoint 仍可能可访问；最终由 provider 决定权限。环境变量是普通的进程输入，不要承诺同一用户的其他进程或诊断工具无法观察它。

使用短有效期 token，定期轮换并及时撤销；CI 中使用 masked/protected secret，或使用 secret manager。长期运行的 agent 应使用独立的只读身份和短期凭证。NDJSON 和 text 输出可能包含私有源码、路径、commit message 及 provider 诊断信息；请把 stdout、stderr、日志、构建产物和下游存储都按敏感数据保护。

内部 CA 环境应把 CA 安装到操作系统和 Go 标准 TLS 栈所使用的系统信任链中。`git-rg` 没有 `--insecure` 参数，不要绕过证书校验。SSH 风格 URL 只解析仓库地址，不提供 SSH 认证。

<a id="cache"></a>
## 缓存

持久化缓存保存 commit 固定的 tree、blob、archive 和 indexed raw-file 条目，每个 `.entry` 文件包含版本头、SHA-256 校验和及正文，通过一次 rename 发布完整条目；token 不参与缓存 key。旧正文与 sidecar 缓存视为未命中，仍纳入容量清理。branch/tag 列表会变化，因此每次重新查询，不写入磁盘缓存。

- 默认缓存根目录是 `os.UserCacheDir()/git-rg`。常见位置是 macOS 的 `~/Library/Caches/git-rg`、Linux 的 `$XDG_CACHE_HOME/git-rg` 或 `~/.cache/git-rg`，以及 Windows 的 `%LocalAppData%\git-rg`。实际路径取决于平台和运行时环境。
- 默认缓存容量总计为 512 MiB。命令启动/结束时检查是否需要清理：成功写入缓存后触发清理，只读查询共享一个五分钟清理时间戳。并发命令下仍为 best effort；超过 24 小时的临时文件和孤立 checksum sidecar 可能被清理。
- 程序会尝试将缓存目录设为 `0700`、缓存文件设为 `0600`。真实权限语义还取决于操作系统、文件系统、umask、ACL 和账号配置；私有缓存必须按敏感数据处理，并在目标环境检查有效权限。
- `--no-cache` 禁用本次运行的持久化缓存读写和清理，结果先使用每个 worker 最多 256 KiB 保守字节预算的内存缓冲；超出后以二进制格式写入 OS 临时目录中的 `0600` 文件，每个文件上限 32 MiB，用完会删除。`--no-cache` 不会禁用该结果 spool。高敏感或临时 agent 应使用 `--no-cache`，同时保护或隔离 OS 临时目录。
- cache/spool 超限、checksum 无效和缓存 I/O 错误都会明确报告；无效缓存不会被当作可信仓库内容。缓存目录初始化失败会发出 `cache_disabled` warning，搜索可能在不使用持久化缓存的情况下继续。

<a id="resource-and-platform-boundaries"></a>
## 资源与平台边界

以下是有意设置的边界，不是容量承诺：

| 范围 | 上限 |
| --- | --- |
| 单行文本 | 8 MiB |
| 单个扫描文本文件 | 512 MiB |
| before-context 缓冲 | 32 MiB |
| 单行完整匹配数 | 100,000 |
| 单个结果 spool | 内存预算 256 KiB，超出后最多 32 MiB 磁盘文件；搜索 worker 最多 8 个 |
| 压缩 archive 输入 | 512 MiB |
| 解压 archive 输出 | 4 GiB |
| archive header/路径元数据 | 1,000,000 个 header / 128 MiB；单个路径最多 1 MiB |
| Provider JSON 响应 | 16 MiB |
| 完整 tree 收集 | 1,000,000 个 item / 128 MiB 元数据 / 1,000 次逻辑分页或 subtree 请求 |
| Ref 列表 | 100,000 个 ref / 32 MiB 元数据 / 1,000 次逻辑分页请求 |
| Indexed 候选 | 最多 10 页、每页最多 100 项 / 8 MiB 路径元数据 |

超过本地或 provider 边界会返回明确的 `resource_limit` error。GitHub Git Blob API 路径拒绝大于 100 MiB 的对象；archive 读取使用独立的压缩、解压和本地文件上限。文件会探测 NUL 和非法 UTF-8；二进制/非法 UTF-8 文件会跳过，如果在暂存匹配之后才发现 NUL，则丢弃该文件的全部暂存 event。达到结果上限后停止匹配，不对后缀作文本/二进制检查承诺；archive 条目仍会在资源限制内读到文件末尾，验证 blob 哈希后才输出结果。

每个 HTTP 请求最多尝试 3 次。客户端尊重可用的 `Retry-After`/限流 reset 延迟，最多允许 5 次重定向，拒绝 HTTPS 降级；HTTPS 跨 host 重定向时会剥离授权请求头。`--max-requests` 统计 ref 解析、索引、tree、archive、blob、重试和重定向请求。

六个 Tier 1 二进制覆盖 Linux、macOS、Windows 的 amd64 和 arm64。Release 使用 `CGO_ENABLED=0` 构建；其他平台可以用 Go 1.26 源码构建，但属于 best effort。GitHub.com 和 GitLab.com 是主要 SaaS 目标。GHES 和自建 GitLab 通过对应 REST API best effort 支持，不承诺最低服务端版本；请使用目标实例自己的 provider、API 基址、token 和代表性仓库进行验收。

GitHub tree 行为可参阅官方 [Git Trees API](https://docs.github.com/en/rest/git/trees)。工具需要访问支持的 API，目前不提供离线模式、SSH 认证或通用 Git 服务器兼容性。

<a id="development-support-and-license"></a>
## 开发、支持与许可证

使用 Go 1.26 或更高版本构建：

```sh
go build -trimpath -o ./git-rg ./cmd/git-rg
```

CI 会运行 `go test -race ./...`、`go vet ./...` 和构建，并对六个 Tier 1 组合执行原生 smoke 检查。Release workflow 构建带 checksum 的 archive，并执行安装脚本以及公开 GitHub/GitLab smoke。反馈问题时请提供 `git-rg --version`、OS/架构、provider、mode、缓存选择、脱敏后的命令和脱敏后的 NDJSON `meta`/`summary`/`error`；不要附带 token、私有源码或私有缓存文件。依赖非 Tier 1 平台或 EOL 版本前，请阅读 [SUPPORT.md](SUPPORT.md)。

本地性能矩阵：`go test ./internal/search -run '^$' -bench BenchmarkRunner -benchmem -count 3`。它覆盖冷/热缓存、大量小文件、高匹配量，以及单文件精确 glob 的 direct-blob 路径，并记录首条结果延迟及 provider 请求数；不模拟远程网络延迟。

完整 CLI 调用基准：`go test ./internal/cli -run '^$' -bench BenchmarkCLI -benchmem -count 3`，包含本地 HTTP fixture 的 ref 解析、缓存维护和 NDJSON 文件输出，并覆盖大量缓存条目；不包含外部网络延迟与进程启动。

本项目采用 [MIT License](LICENSE) 发布。
