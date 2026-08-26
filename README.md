# git-rg

[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/doc/install)
[![CI](https://github.com/SamuelSupe/git-rg/actions/workflows/ci.yml/badge.svg)](https://github.com/SamuelSupe/git-rg/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SamuelSupe/git-rg)](https://github.com/SamuelSupe/git-rg/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/SamuelSupe/git-rg/total)](https://github.com/SamuelSupe/git-rg/releases)

> 一句话：不建立目标仓库工作树，固定到不可变 commit 后按需读取 API 内容，为 agent 提供低磁盘、可脚本化的远程搜索入口。

`git-rg` 是一个只通过 GitHub/GitLab API 搜索远程仓库的命令行工具：先解析某个 ref 对应的不可变 commit，再按模式以 tar.gz archive 流、候选 blob 或完整 tree+blob 读取内容并执行 Go 正则匹配。默认输出 NDJSON，适合脚本消费；也可输出简洁文本。

除搜索外，`git-rg refs REPOSITORY` 可以通过 API 完整分页列出远端分支头和 tag，并按相同 commit SHA 标记它们的精确对应关系，同样不需要 clone。

## 为什么不需要 clone

- 省去 clone、checkout 和历史下载的准备步骤，通常能让 agent 的首次查询更快；实际延迟仍取决于 API、网络和仓库规模。
- 不落地目标仓库工作树或历史，磁盘主要只使用可清理的缓存和进程生命周期临时文件；私有大仓库也可以按需读取指定 commit 的内容。
- `exact` 的最坏情况是流式读取指定 commit 中所有符合 glob 的普通文件并逐文件判定文本；archive 不可用时才改为完整 tree+blob 读取。它不下载提交历史，也不把“无需 clone”误解为“无需读取内容”。

## 边界

- 不需要 clone，也不会调用 `git`、建立本地工作树或读取本地仓库；SSH 风格的地址只用于解析主机和项目路径，不会启动 SSH 传输。
- 当前实现的 provider 只有 `github` 和 `gitlab`。完整 tree+blob 路径只纳入模式为 `100644`/`100755` 的 blob；`indexed` 路径使用代码索引给出的路径，不证明真实 mode 或 symlink 属性；archive 流只处理 tar regular file，非 regular entry 会跳过。
- 必须能访问对应 API。当前没有内置离线模式、通用 Git 服务器协议或本地路径搜索；预构建二进制通过 GitHub Releases 提供。

## 安装

v0.2.0 提供六个 Tier 1 预构建组合：Linux、macOS、Windows 的 `amd64`（x86_64）和 `arm64`。二进制用户不需要安装 Go；脚本和包管理器都会在安装前校验 Release 的 SHA-256。v0.2.0 是当前支持版本，v0.1.0 仅保留下载并已进入 EOL；完整策略见 [`SUPPORT.md`](SUPPORT.md)。完整的安装、升级、卸载和故障排查说明见 [`docs/installation.md`](docs/installation.md)。

### 推荐方式

Linux/macOS 可以安装到用户目录（不会自动使用 sudo）：

```sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh \
  | sh -s -- --version v0.2.0 --bin-dir "$HOME/.local/bin"
git-rg --version
```

Windows PowerShell：

```powershell
$script = Join-Path $env:TEMP "git-rg-install.ps1"
Invoke-WebRequest https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.ps1 -OutFile $script
& $script -Version v0.2.0
git-rg --version
```

若环境已有 Go `1.26` 或更高版本，也可以直接安装源码中的命令：

```sh
go install github.com/SamuelSupe/git-rg/cmd/git-rg@v0.2.0
# 持续跟随最新稳定版本：
go install github.com/SamuelSupe/git-rg/cmd/git-rg@latest
```

### 包管理器

```sh
# macOS/Linux（需要 Homebrew）
brew install SamuelSupe/tap/git-rg
brew upgrade SamuelSupe/tap/git-rg

# 卸载
brew uninstall git-rg
```

```powershell
# Windows（需要 Scoop）
scoop bucket add samuelsupe https://github.com/SamuelSupe/scoop-bucket
scoop install git-rg
scoop update git-rg

# 卸载
scoop uninstall git-rg
```

### Release 手工下载

固定版本的 Release 页面：[v0.2.0](https://github.com/SamuelSupe/git-rg/releases/tag/v0.2.0)。Release 共包含 9 个资产：6 个平台 archive、`install.sh`、`install.ps1` 和 `checksums.txt`。每个 archive 有一个顶层版本目录，目录内包含对应平台的单个可执行文件；资产名称如下：

| 平台 | 资产 |
| --- | --- |
| Linux x86_64 | `git-rg_v0.2.0_linux_amd64.tar.gz` |
| Linux arm64 | `git-rg_v0.2.0_linux_arm64.tar.gz` |
| macOS x86_64 | `git-rg_v0.2.0_darwin_amd64.tar.gz` |
| macOS arm64 | `git-rg_v0.2.0_darwin_arm64.tar.gz` |
| Windows x86_64 | `git-rg_v0.2.0_windows_amd64.zip` |
| Windows arm64 | `git-rg_v0.2.0_windows_arm64.zip` |

两个安装脚本和 `checksums.txt` 也直接附在 Release 页面中；脚本只选择当前 OS/架构的 archive，并在写入安装目录前校验其 checksum。

Linux/macOS 手工下载示例（macOS 可将 `sha256sum` 换为 `shasum -a 256`）：

```sh
version=v0.2.0
asset="git-rg_${version}_linux_amd64.tar.gz"
base="https://github.com/SamuelSupe/git-rg/releases/download/${version}"
curl -fL -o "$asset" "$base/$asset"
curl -fL -o checksums.txt "$base/checksums.txt"
grep " $asset$" checksums.txt | sha256sum -c -
tar -xzf "$asset"
"./git-rg_${version}_linux_amd64/git-rg" --version
```

Windows PowerShell 手工下载示例（以 x86_64 为例）：

```powershell
$Version = "v0.2.0"
$Asset = "git-rg_${Version}_windows_amd64.zip"
$Base = "https://github.com/SamuelSupe/git-rg/releases/download/$Version"
Invoke-WebRequest -Uri "$Base/$Asset" -OutFile $Asset
Invoke-WebRequest -Uri "$Base/checksums.txt" -OutFile checksums.txt
$expected = (Select-String -Path checksums.txt -Pattern " $([regex]::Escape($Asset))$").Line.Split()[0]
$actual = (Get-FileHash -Path $Asset -Algorithm SHA256).Hash.ToLowerInvariant()
if ($expected -ne $actual) { throw "checksum mismatch" }
Expand-Archive -Path $Asset -DestinationPath .
& ".\git-rg_${Version}_windows_amd64\git-rg.exe" --version
```

脚本、包管理器和手工下载都是同一份 GitHub Release 资产；没有 Release 资产的版本不会被安装器接受。安装后可执行文件只需在 `PATH` 中，搜索本身仍只访问远程 API，不会 clone 目标仓库。

仓库内的 `.github/workflows/ci.yml` 在 push/pull request 上执行 `go test -race ./...`、`go vet ./...` 和构建。`.github/workflows/release.yml` 在 `v*` tag 上用六个平台原生 runner 构建 archive，创建包含 6 个 archive、两个安装脚本和 `checksums.txt` 的 GitHub Release，并在发布后运行对应平台安装和 GitHub/GitLab smoke。

## 5 分钟上手

需要 Go `1.26`（仅源码构建或 `go install` 需要）以及访问 GitHub/GitLab API 的网络连接：

```sh
# 1. 查看版本和帮助
git-rg --version
git-rg --help

# 2. 搜索公共仓库（不 clone 目标仓库）
git-rg -F "TODO" github:OWNER/REPO

# 3. agent 通常消费 NDJSON；所有 flags 放在 PATTERN/REPOSITORY 之前
git-rg --mode exact --format ndjson -g '*.go' TODO github:OWNER/REPO
```

安装脚本的完整参数、PATH 处理、校验失败处置以及固定版本升级方式见 [`docs/installation.md`](docs/installation.md)。

## 仓库地址与示例

支持以下仓库参数形式：

```text
github:OWNER/REPO
gitlab:GROUP/PROJECT
https://HOST/OWNER/REPO.git
git@HOST:GROUP/PROJECT.git
```

公共 GitHub/GitLab：

```sh
git-rg TODO github:OWNER/REPO
git-rg -F "release marker" https://github.com/OWNER/REPO.git
git-rg --provider gitlab TODO gitlab:GROUP/PROJECT
git-rg --ref v1.2.3 TODO https://gitlab.com/GROUP/PROJECT.git

# 列出分支头、tag 及指向同一 commit 的对应关系
git-rg refs github:OWNER/REPO
git-rg refs --kind tag gitlab:GROUP/PROJECT
```

私有 GitHub Enterprise 和私有 GitLab 必须显式指定 `--provider`；默认 API 基址分别是 `https://HOST/api/v3` 和 `https://HOST/api/v4`，也可以用 `--api-base` 覆盖：

```sh
export GITRG_TOKEN='...'
git-rg --provider github TODO \
  https://github.example.com/OWNER/REPO.git

export GITRG_TOKEN='...'
git-rg --provider gitlab \
  --api-base https://gitlab.example.com/api/v4 \
  TODO \
  git@gitlab.example.com:GROUP/PROJECT.git
```

`--api-base` 必须是绝对 `http://` 或 `https://` URL，且不能包含凭据、查询串或片段；生产环境应使用 HTTPS。公共主机可由地址自动判断 provider，私有主机不能自动判断。

### 认证环境变量

程序只从环境变量读取 token，不把 token 放入仓库地址：

| provider | 读取优先级 | 请求头 |
| --- | --- | --- |
| 两者 | `GITRG_TOKEN` | 按 provider 使用下方请求头；适用于私有主机 |
| GitHub（仅 `github.com`） | `GITHUB_TOKEN`，然后 `GH_TOKEN` | `Authorization: Bearer <token>` |
| GitLab（仅 `gitlab.com`） | `GITLAB_TOKEN` | `PRIVATE-TOKEN: <token>` |

`GITHUB_TOKEN`/`GH_TOKEN` 不会发送给自建 GitHub 主机，`GITLAB_TOKEN` 不会发送给自建 GitLab 主机；私有主机请使用 `GITRG_TOKEN`。未设置 token 时仍会发起请求，是否能访问由远端权限决定。不要把 token 写入 shell 历史、脚本或日志。

## 命令行参数

用法：`git-rg [FLAGS] PATTERN REPOSITORY`。PATTERN 默认按 Go 正则表达式解释。

| 参数 | 默认值 | 作用 |
| --- | --- | --- |
| `-F`, `--fixed-strings` | 关闭 | 将 PATTERN 当作固定字符串 |
| `-i`, `--ignore-case` | 关闭 | 忽略大小写 |
| `-w`, `--word-regexp` | 关闭 | 要求 Unicode 单词边界（字母、数字、标记和 `_` 视为单词字符） |
| `-g`, `--glob GLOB` | 无限制 | 包含 glob；以 `!` 开头表示排除，可重复 |
| `-B`, `--before-context NUM` | `0` | 每个匹配前输出 NUM 行 |
| `-A`, `--after-context NUM` | `0` | 每个匹配后输出 NUM 行 |
| `-C`, `--context NUM` | `0` | 同时设置前后文；若同时给出 `-B`/`-A`，后者分别覆盖对应值 |
| `--ref REF` | 默认分支 | 指定 branch、tag 或 commit |
| `--commit-info` | 关闭 | 在 `meta` 中包含目标 commit 的 author、committer、时间和 message；text 格式下在匹配结果前输出这些信息 |
| `--mode MODE` | `auto` | `auto`、`exact` 或 `indexed` |
| `--format FORMAT` | `ndjson` | `ndjson` 或 `text` |
| `--max-results NUM` | `200` | 最多输出的匹配行数；`0` 表示不限制 |
| `--max-requests NUM` | `100` | 最多使用的远端 HTTP 请求数（包含重试）；`0` 表示不限制 |
| `--provider NAME` | 自动判断 | `github` 或 `gitlab`；私有主机必填 |
| `--api-base URL` | 按主机推导 | 覆盖 provider API 基址 |
| `--no-cache` | 关闭 | 禁用本次命令的磁盘缓存 |
| `--timeout DURATION` | `5m` | 整个命令的超时，例如 `30s`、`2m` |
| `--version` | — | 打印版本并返回 `0`，不搜索仓库 |
| `-h`, `--help` | — | 显示帮助并返回退出码 `0` |

`--ref` 可接分支名、tag 名或 commit SHA；程序先解析为不可变 commit，后续内容请求都以该 SHA 读取，不会下载历史。需要提交元数据时加 `--commit-info`；它复用解析 ref 时的响应，不增加远端请求。`refs` 子命令则完整分页列出分支/tag 头及同一 commit 的对应关系，详见下节。

`-g` 使用路径 glob：包含 `/` 时按完整路径匹配，不含 `/` 时按 basename 匹配；支持 `*`、`?`、字符组和递归段 `**`。`**` 使用迭代动态规划匹配，不对远端深层路径执行递归回溯。有任意正向模式时，默认只允许正向匹配；否则默认允许全部，按参数顺序最后命中的规则决定结果。例如：

```sh
git-rg -g '*.go' -g '!vendor/**' TODO github:OWNER/REPO
```

`--max-requests` 是整个命令的远端请求预算，覆盖 ref/tree/index/archive/blob 请求以及重试和重定向；预算耗尽会产生 `request_budget_exceeded` error。设置为 `0` 才是不限制。`auto` 在解析 ref 后剩余的有限预算不足 39 次请求时会跳过可选索引加速；启用加速时，候选 blob 预取也按最多 3 次尝试计算，并保留 6 次请求给 archive 及 GitHub archive 重定向，避免可选加速抢占完整扫描预算。

## 列出分支和 tag

用法：`git-rg refs [FLAGS] REPOSITORY`。

`refs` 是保留的首个子命令名；如果搜索 PATTERN 本身正好是 `refs`，使用 `git-rg -- refs REPOSITORY`，其中 `--` 明确结束 flag 解析并进入普通搜索。

| 参数 | 默认值 | 作用 |
| --- | --- | --- |
| `--kind KIND` | `all` | `all`、`branch` 或 `tag` |
| `--format FORMAT` | `ndjson` | `ndjson` 或 `text` |
| `--max-requests NUM` | `100` | 完整分页及重试共享的远端请求预算；`0` 表示不限制 |
| `--provider NAME` | 自动判断 | `github` 或 `gitlab`；私有主机必填 |
| `--api-base URL` | 按主机推导 | 覆盖 provider API 基址 |
| `--timeout DURATION` | `5m` | 整个 refs 命令的超时 |

默认 NDJSON 依次输出 `meta`、稳定排序的 `ref` 和 `summary`：

```ndjson
{"type":"meta","schema_version":1,"provider":"github","repository":"https://github.com/OWNER/REPO"}
{"type":"ref","kind":"branch","name":"main","commit":"0123456789abcdef","head_tags":["v1.2.0"]}
{"type":"ref","kind":"tag","name":"v1.2.0","commit":"0123456789abcdef","head_branches":["main"]}
{"type":"summary","summary":{"branches":1,"tags":1,"api_requests":2,"api_retries":0,"request_limit":100,"complete":true}}
```

`head_tags` 只表示 tag 当前与该分支头指向完全相同的 commit；`head_branches` 是反向关系。它不表示某个历史 tag 一定“属于”哪条分支，因为完整的祖先/可达性判断需要 commit 历史，而 `git-rg` 不下载历史。使用 `--kind branch` 或 `--kind tag` 时不会请求另一类 ref，因此也不会输出对应关系字段。

GitHub 和 GitLab 都按每页 100 项读取到结束；远端单页返回超过 100 项会被拒绝。结果会校验 ref 名称和 commit、去重并按 branch/tag 及名称排序。一次命令最多接受 100,000 个 ref、`32 MiB` ref 元数据和 1,000 次逻辑分页请求；branch/tag 同 commit 关系最多展开 1,000,000 对、`128 MiB` 名称数据，超限在输出 meta/ref 前返回 `resource_limit`。任何分页、权限、限流、超时或请求预算错误都会返回退出码 `2`，不会把结果列表标为完整。成功时即使仓库没有所选类型的 ref 也返回 `0`。

text 格式每行是 tab 分隔的 `kind`、`name`、`commit`；存在同 commit 对应项时继续追加列，branch 行追加 tag 名称，tag 行追加 branch 名称。provider 返回的 ref 名会按 Git ref 的控制字符和保留字符边界校验，因此 tab/newline 不会注入额外文本列或行。`--timeout` 同时覆盖远端分页、本地归并、关系预算计算和逐项输出之间的取消检查；单次不可中断的 writer 调用仍可能短暂越过截止时间。

## 搜索模式与完整性

| 模式 | 行为 | `summary.complete` |
| --- | --- | --- |
| `exact` | 不调用代码索引、不枚举 tree；对 glob 过滤后的全部普通文件使用不可变 commit archive 的 tar.gz 流匹配。 | 通常为 `true`；archive 不可用时才完整枚举 tree 并逐 blob 回退，超时、错误或达到 `--max-results` 时为 `false`。 |
| `auto`（默认） | 没有非空 literal prefix 时不枚举 tree，直接使用 archive。存在 literal prefix 时调用 provider 代码索引；仅以 best-effort、最多 16 个候选 blob 做预取（仍受 `--max-requests` 预算约束），其余文件使用 commit archive 流。 | 候选索引或候选 manifest 可能不完整，只用于有限预取；archive 扫描整个 commit 仍保证完整性。候选 blob 失败会保留给 archive 重试；archive 不可用才输出 `archive_unavailable` 并完整枚举 tree、逐 blob 回退。正常完成时为 `true`，`meta.mode` 仍为 `auto`。 |
| `indexed` | 不完整枚举 tree。代码索引候选先做 glob、安全路径校验、去重并按路径排序；GitHub 使用 Contents raw、GitLab 使用 repository files raw，并以不可变 commit SHA 作 ref，再由本地 RE2 matcher 复核；不使用 archive。 | 始终为 `false`，`reason` 为 `indexed_mode`；它是候选集搜索，不是完整性证明，也不能确认真实 mode 或 symlink。 |

`indexed` 要求 PATTERN 能被 Go 正则提取出非空字面前缀；否则返回 `indexed_pattern_unsupported`。索引请求失败返回 `indexed_search_failed`，不会静默改为全量扫描。GitHub 和 GitLab 的索引请求都最多读取 10 页、每页 100 项，并执行累计 `8 MiB` 候选路径预算；索引覆盖范围仍由 provider 决定。

exact 和没有可靠 literal prefix 的 auto 路径不会先获取 tree。literal auto 的 tree 请求只为把有限索引候选映射到 blob；GitHub 截断响应、GitLab 单页响应等 partial manifest 会发出 warning，不能改变 archive 的完整扫描。auto 的候选优先部分按路径排序；其余文件以及 exact 的全部文件都按 immutable provider archive entry 的实际顺序逐项流式处理，合法乱序不会报错。这样可以保留早停，不必全量缓冲后续文件，也不必为排序而枚举完整 tree。

这里的 archive 回退包括 archive endpoint 不可用，或在首个普通文件前无法解析 archive 流；请求预算耗尽、限流或命令取消会保持 `read_archive` error，不会继续发起完整 blob 回退。

无论模式如何，默认 `--max-results 200` 可能截断结果：此时 `summary.truncated=true`、`summary.reason="result_limit"`，且 `complete=false`。如果此时正在下载新的 archive，缓存事务会中止，不会提交不完整 archive。退出码是否为成功仍按是否找到匹配行决定，见下文。

GitLab archive 请求显式排除 LFS 对象（`include_lfs_blobs=false`）：搜索仓库中的 LFS pointer 内容，不下载 LFS 对象本体。

## 输出

### NDJSON（默认）

stdout 每行一个 JSON event；`type` 总是存在，其他字段按 event 类型可选。事件通常依次包含 `meta`、warning/match/context、可选 `error`、`summary`；参数、仓库解析或 ref 解析在搜索开始前失败时，可能只有 `error` 而没有 `meta`/`summary`。默认 NDJSON 下，参数解析、参数校验、正则和仓库校验等 CLI 错误都会输出稳定的 `{"type":"error","code":"...","message":"..."}` event 到 stdout（参数类 code 包括 `invalid_arguments`、`invalid_format`、`invalid_mode`、`invalid_pattern`、`invalid_repository`）；CLI 会在任何远端请求前预检 glob 和 `indexed` 的 literal prefix，因此 `invalid_glob` 或 `indexed_pattern_unsupported` 可能只有 error、没有 `meta`/`summary`。诊断写到 stderr，并会转义换行、tab、ESC 等控制字符，避免远端错误文本注入额外日志行或终端控制序列；NDJSON `message` 字段仍由 JSON 编码器按原始文本安全编码。使用 `--format text` 时 error/warning 仍只写 stderr。

主要 schema：

| `type` | 字段 |
| --- | --- |
| `meta` | `schema_version`（当前为 `1`）、`provider`、`repository`、`requested_ref`、`resolved_ref`、`commit`、`mode`；使用 `--commit-info` 时额外包含 `commit_info` |
| `match` | `path`、`line`（1-based）、`column`（行内 1-based 字节偏移）、`text`、`submatches` |
| `context` | `path`、`line`、`text`、`context`（`before` 或 `after`） |
| `warning` / `error` | `code`、`message` |
| `summary` | 嵌套的 `summary` 对象，包含 `matched_lines`、`matched_files`、`scanned_files`、`cache_hits`、`downloaded_bytes`、`skipped_binary`、`api_requests`、`api_retries`、`request_limit`、`complete`、`truncated`、`duration_ms`，以及可选 `reason`、`transport`、`rate_limit` |

`submatches` 是该行每个完整命中的数组（当前不是捕获组数组）；每项为 `{ "start": 0, "end": 4, "text": "TODO" }`，其中 start/end 是 0-based、end-exclusive 的字节偏移。`text` 不含行尾换行符。

`match`/`context` event 即使匹配或上下文行为空，也会保留 `"text":""` 字段。`api_requests`、`api_retries`、`request_limit` 是整数，`request_limit=0` 表示不限制；`rate_limit`（若远端返回限流头）是对象 `{ "limit", "remaining", "reset", "resource", "name" }`；`transport` 当前可能为 `archive`、`blob_fallback` 或 `blob`。

`--commit-info` 复用解析 ref 时已有的 commit API 响应，不增加远端请求。`commit_info.author` 和 `commit_info.committer` 包含 `name`、`email`，GitHub 能关联平台账号时还包含 `username`；时间字段为 `authored_at`、`committed_at`，提交说明为 `message`。GitLab commit API 不提供可可靠映射的平台用户名，因此通常没有 `username`。author 是提交声明的原作者，committer 是实际创建该 commit 对象的人，两者可能不同。

每个文件产生的 match/context 事件先写入权限为 `0600` 的进程生命周期临时 spool 文件；正常读完该文件、确认未发现 NUL 且 UTF-8 合法后才输出，随后立即删除。若 `result_limit` 先触发则会提前停止，未读取的后缀不作检查承诺。`--no-cache` 只禁用持久化缓存，不禁用这个临时 spool 文件。单文件 spool 上限为 `32 MiB`，worker 硬上限为 8，因此并发待输出 spool 不会无限增长；`--max-results 0` 的 stdout 总量仍可能无限增长。单行超过 `8 MiB`、单行超过 100,000 个 submatch、单文本文件超过 `512 MiB`、before-context 窗口超过 `32 MiB`、单个仓库路径超过 `1 MiB`，或 archive 超过 1,000,000 个 header、累计 `128 MiB` 路径元数据等资源预算超限，都会以 `resource_limit` 明确失败，`exact` 不会静默跳过。非法 UTF-8 按二进制跳过。

示例（字段值仅作示例）：

```ndjson
{"type":"meta","schema_version":1,"provider":"github","repository":"https://github.com/OWNER/REPO","resolved_ref":"main","commit":"0123456789abcdef","mode":"exact"}
{"type":"match","path":"README.md","line":3,"column":1,"text":"TODO: document the API","submatches":[{"start":0,"end":4,"text":"TODO"}]}
{"type":"summary","summary":{"matched_lines":1,"matched_files":1,"scanned_files":2,"cache_hits":0,"downloaded_bytes":734,"skipped_binary":0,"api_requests":4,"api_retries":0,"request_limit":100,"transport":"archive","complete":true,"truncated":false,"duration_ms":18}}
```

### 文本格式

使用 `--format text` 时，匹配行写为 `path:line:column:text`，上下文行写为 `path-line-text`；meta/summary 默认不写到 stdout，warning/error 写到 stderr。若 summary 为 `complete:false`，stderr 固定输出 `incomplete_results` 诊断并包含 `reason` 与 `truncated`，因此 `result_limit` 和 `indexed_mode` 不会伪装成完整文本结果。指定 `--commit-info` 后，text 输出会在匹配结果前写入 `commit`、`Author`、`AuthorDate`、`Committer`、`CommitDate` 和单行转义的 `Message`。

## 退出码

- `0`：搜索完成且至少找到一行匹配。
- `1`：搜索完成但没有匹配行。
- `2`：参数/正则/glob/仓库或 provider 初始化失败、远端解析/列树/archive/blob 读取失败、输出失败、超时/中断、资源预算耗尽（`resource_limit`）、远端请求预算耗尽（`request_budget_exceeded`），或 `indexed` 模式不满足前缀/索引请求失败。

warning（例如默认 `auto` 的 `index_unavailable`）本身不改变退出码。`indexed` 即使 `complete=false`，只要找到匹配仍返回 `0`；没有匹配返回 `1`。

`SIGINT`/`SIGTERM` 会取消整个命令上下文；进入搜索后若未先因 `result_limit` 停止，summary 会标为 `complete=false`、`reason="cancelled"` 并返回 `2`，错误 code 取决于取消发生阶段（例如 `search_cancelled`、`read_archive` 或 `read_blob`）。`--version` 是不需要 PATTERN/REPOSITORY 的快速路径，直接向 stdout 写 `git-rg <version>` 并返回 `0`。

## 缓存

缓存保存提交对应的 immutable tree、blob、archive（以及 indexed 的 commit-pinned raw file）条目；每个条目最多 `512 MiB`，并以 SHA-256 `.sha256` sidecar 记录和校验内容，读取时先验证。缓存启动和结束时 prune，清理超过总容量的旧条目以及超过 `24h` 的临时/孤儿 sidecar；并发命令下总容量控制是 best-effort，可能短暂超限。目录/文件权限分别尽力设为 `0700`/`0600`。逐 blob 仅在完整读到 EOF 后提交原子临时缓存；命中 `result_limit`、二进制、读/写失败都会中止事务，不会留下 partial blob cache。正在扫描时达到 `result_limit` 的 archive 也不会提交到缓存。

- cache checksum 校验、删除或 prune 失败会以 warning 报告（例如 `cache_read_failed`、`cache_remove_failed`、`cache_prune_failed`）；无效条目会在能删除时清除，不会当作可信内容继续使用。

- 命中缓存的 archive 出现缓存 checksum、底层 archive/provider 读取，或 tar/gzip/path/duplicate 等完整性校验失败时，程序都会关闭并删除该缓存。gzip 开头损坏会发出 `cache_read_failed` warning，并在本次运行中重新请求 archive；远端返回 HTTP 200 但不是 gzip，或在处理首个普通文件前出现 tar header/unsafe path 错误，可视为 archive 不可用并安全地完整回退到 tree+blob。首个普通文件前的缓存深层错误不会承诺同次刷新 archive，缓存会在后续运行重建。处理过首个普通文件后，完整性错误会显式返回 `read_archive` error、清除缓存，不会回退或伪装为完整结果；单行 `8 MiB`、before-context `32 MiB` 或临时 spool 写失败等本地资源策略错误同样显式失败，但保留结构有效的 archive 缓存。

- tree manifest 的缓存和远端 tree 结果都会做完整性校验：JSON 必须完整且无尾随内容，路径必须安全且规范化，OID 非空，mode 只能是 `100644`/`100755`，size 非负，路径不可重复。无效的 tree 缓存会删除并刷新远端；只缓存完整且编码上界不超过 `64 MiB` 的 tree，partial 或过大的 manifest 不入缓存；有效的 tree cache hit 会计入 `summary.cache_hits`。

- 默认目录由 Go 的 `os.UserCacheDir()` 决定，再追加 `git-rg`：macOS 通常为 `~/Library/Caches/git-rg`，Linux 通常为 `$XDG_CACHE_HOME/git-rg` 或 `~/.cache/git-rg`，Windows 通常为 `%LocalAppData%\\git-rg`。
- 程序会创建缓存目录并尝试将其设为 `0700`、缓存文件设为 `0600`（权限语义以平台文件系统为准）。私有仓库内容可能进入缓存，请保证当前用户的缓存目录安全。
- `--no-cache` 禁用本次运行的缓存读写和清理。缓存目录创建/权限设置失败时，程序会发出 `cache_disabled` warning 并继续尝试搜索。

缓存只是可丢弃的加速数据，不是仓库真源；删除后下次运行会重新从 API 获取。

## 平台限制与安全说明

- 代码使用 Go 标准库；v0.2.0 预构建 Linux/macOS/Windows 的 `amd64` 和 `arm64`，其他 Go `1.26` 支持的平台可以自行构建并按 best effort 使用。运行依赖可访问的 GitHub/GitLab API，当前不承诺离线、代理配置、SSH 认证或其他 Git 服务兼容性。
- 远端响应使用默认 Go HTTP transport；单个请求最多尝试 3 次，针对 429、5xx、已耗尽限流的 403，以及带正 `Retry-After` 的 403 secondary rate limit 尊重 `Retry-After`/reset。最多允许 5 次重定向，拒绝 HTTPS 降级；GitHub archive 可能临时跨 host 重定向，此时只在目标仍为 HTTPS 时跟随，并剥离 `Authorization`、`PRIVATE-TOKEN` 和 Cookie。
- GitHub Git Blob OID 路径会拒绝大于 100 MiB 的对象；archive 路径不受这个 provider blob API 上限约束，但本地仍执行单文本文件 `512 MiB`、archive 压缩输入 `512 MiB`、解压输出 `4 GiB`、1,000,000 个 header、单路径 `1 MiB` 和累计 `128 MiB` 路径元数据的硬预算，NUL 路径会作为无效 archive 拒绝。API JSON 解码限制为 `16 MiB`；provider 收集中的单个远端字段也限制为 `1 MiB`，完整 tree 收集最多 1,000,000 个远端 item、`128 MiB` 元数据和 1,000 次逻辑分页/子树请求。文件先探测前 8 KiB；实际读取链路中一旦发现 NUL（包括超过首 8 KiB），会丢弃该文件已暂存的全部匹配结果并计入 `skipped_binary`。达到 `result_limit` 会先停止，未读取的后缀不作检查承诺。
- token 只通过请求头发送；仓库 HTTP(S) 地址和 `--api-base` 不接受 URL 凭据。不要把私有 token 或私有仓库缓存目录暴露给其他用户。
- `--timeout` 是整个命令的截止时间；网络、索引、cache checksum 校验、本地 tree/archive 解析、blob/matcher 读取和结果 spool 回放都检查同一个上下文。个别不可中断的短系统调用或有界计算（例如文件 `fsync`、单行 RE2 匹配）仍可能在返回前短暂越过截止时间。请将远端返回的错误、匹配内容和缓存视为不可信输入，不要直接执行匹配结果中的命令。
