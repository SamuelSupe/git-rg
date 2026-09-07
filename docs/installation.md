# git-rg 安装、升级与卸载

本文说明如何安装 git-rg v0.3.0、校验下载内容、升级到固定版本以及卸载。git-rg 的运行时不需要 Git 或 Go；只有从源码构建和 go install 需要 Go 1.26 或更高版本。

## 选择安装方式

| 场景 | 推荐方式 | 备注 |
| --- | --- | --- |
| Linux/macOS 用户目录 | install.sh | 自动识别 OS/架构并校验 SHA-256，不使用 sudo |
| Windows 用户目录 | install.ps1 | 支持 amd64/arm64，可选择是否修改用户 PATH |
| Homebrew 用户 | brew install SamuelSupe/tap/git-rg | macOS 和 Linux |
| Scoop 用户 | scoop install git-rg | Windows |
| 已有 Go 1.26+ | go install ...@v0.3.0 | 从源码模块安装 |
| 需要审计安装过程 | 手工下载 Release | 先下载并检查 checksums.txt |

预构建版本只承诺 SUPPORT.md 中的六个 Tier 1 组合。v0.3.0 Release 共包含 9 个资产：6 个平台 archive、install.sh、install.ps1 和 checksums.txt。安装器在 Release 缺少目标 archive、checksums.txt 或匹配校验值时会拒绝安装，不会把未知内容写入目标目录。

## Linux/macOS：安装脚本

脚本支持 Linux/macOS 的 amd64（x86_64）和 arm64。默认安装到 $HOME/.local/bin，不会自动请求 root 权限：

~~~sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh \
  | sh -s -- --version v0.3.0 --bin-dir "$HOME/.local/bin"
~~~

参数：

- --version VERSION：安装指定 Release，例如 v0.3.0；省略时使用最新稳定 Release。
- --bin-dir DIR：安装目录；省略时为 $HOME/.local/bin。目录会被创建，已有同名二进制会在校验通过并下载完成后替换。
- --help：显示参数说明。

建议先下载脚本再检查后执行，适用于受控或需要留存审计记录的环境：

~~~sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh -o /tmp/git-rg-install.sh
less /tmp/git-rg-install.sh
sh /tmp/git-rg-install.sh --version v0.3.0 --bin-dir "$HOME/.local/bin"
~~~

验证安装：

~~~sh
command -v git-rg
git-rg --version
~~~

如果脚本提示目录不在 PATH，按它给出的 shell 配置命令加入 PATH，然后重新打开 shell 或执行对应的 export。例如：

~~~sh
export PATH="$HOME/.local/bin:$PATH"
~~~

不要用 sudo curl ... | sh 绕过权限提示。需要系统级安装时，先用用户可写临时目录下载、校验并审计，再由管理员将已验证的单个二进制复制到受控目录。

## Windows：PowerShell 安装脚本

脚本支持 Windows amd64 和 arm64，默认安装到 %LOCALAPPDATA%\Programs\git-rg\bin，并将该目录加入当前用户的 PATH：

~~~powershell
$script = Join-Path $env:TEMP "git-rg-install.ps1"
Invoke-WebRequest https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.ps1 -OutFile $script
& $script -Version v0.3.0
~~~

参数：

- -Version VERSION：安装指定 Release；省略时使用最新稳定 Release。
- -InstallDir DIR：安装目录；默认 %LOCALAPPDATA%\Programs\git-rg\bin。
- -NoPathUpdate：安装但不修改当前用户的 PATH。
- -Help：显示参数说明。

验证安装（新开的 PowerShell 会自动继承更新后的 PATH）：

~~~powershell
Get-Command git-rg
git-rg --version
~~~

在不允许修改 PATH 的环境中：

~~~powershell
$dir = Join-Path $env:LOCALAPPDATA "Programs\git-rg\bin"
& $script -Version v0.3.0 -InstallDir $dir -NoPathUpdate
& (Join-Path $dir "git-rg.exe") --version
~~~

## Homebrew

macOS/Linux：

~~~sh
brew install SamuelSupe/tap/git-rg
git-rg --version
brew upgrade SamuelSupe/tap/git-rg
~~~

卸载：

~~~sh
brew uninstall git-rg
brew untap SamuelSupe/tap  # 不再需要该 tap 时可选
~~~

Homebrew formula 只引用 git-rg GitHub Release 中的对应平台 archive，并校验 SHA-256。Formula 或 Release 资产不完整时，Homebrew 应拒绝安装；不要通过跳过 checksum 的方式强行安装。

## Scoop

Windows PowerShell：

~~~powershell
scoop bucket add samuelsupe https://github.com/SamuelSupe/scoop-bucket
scoop install git-rg
git-rg --version
scoop update git-rg
~~~

卸载：

~~~powershell
scoop uninstall git-rg
scoop bucket rm samuelsupe  # 不再需要该 bucket 时可选
~~~

Scoop manifest 为 amd64/arm64 分别声明 URL 和 SHA-256，并将解压后的 git-rg.exe 暴露为 git-rg。Manifest 不完整或 hash 不匹配时，更新器必须停止而不是使用旧 manifest 中的未知 URL。

## Go 安装与源码构建

需要 Go 1.26 或更高版本。固定版本安装：

~~~sh
go install github.com/SamuelSupe/git-rg/cmd/git-rg@v0.3.0
git-rg --version
~~~

跟随最新稳定版本：

~~~sh
go install github.com/SamuelSupe/git-rg/cmd/git-rg@latest
~~~

源码构建：

~~~sh
git clone https://github.com/SamuelSupe/git-rg.git
cd git-rg
go build -trimpath -o ./git-rg ./cmd/git-rg
./git-rg --version
~~~

上面的 git clone 只适用于获取 git-rg 自身源码；运行 git-rg 搜索其他目标仓库时，不会 clone、checkout 或下载目标仓库 Git 历史。若不希望获取源码，优先使用 Release、脚本或包管理器。

## 手工下载与 checksum

Release 页面：[v0.3.0](https://github.com/SamuelSupe/git-rg/releases/tag/v0.3.0)。Release 资产共 9 个，命名规则为：

~~~text
git-rg_v0.3.0_{linux|darwin}_{amd64|arm64}.tar.gz
git-rg_v0.3.0_windows_{amd64|arm64}.zip
install.sh
install.ps1
checksums.txt
~~~

每个 archive 有一个顶层版本目录，目录内只有对应平台的单个可执行文件；`checksums.txt` 列出 6 个 archive 和两个安装脚本的 SHA-256。安装器只提取并安装已校验的目标 binary。

Linux/macOS 示例（这里选择 Linux amd64；macOS 使用 shasum -a 256）：

~~~sh
version=v0.3.0
asset="git-rg_${version}_linux_amd64.tar.gz"
base="https://github.com/SamuelSupe/git-rg/releases/download/$version"
curl -fL -o "$asset" "$base/$asset"
curl -fL -o checksums.txt "$base/checksums.txt"
grep " $asset$" checksums.txt | sha256sum -c -
tar -xzf "$asset"
install -m 0755 "git-rg_${version}_linux_amd64/git-rg" "$HOME/.local/bin/git-rg"
"$HOME/.local/bin/git-rg" --version
~~~

Windows PowerShell 示例：

~~~powershell
$Version = "v0.3.0"
$Asset = "git-rg_" + $Version + "_windows_amd64.zip"
$Base = "https://github.com/SamuelSupe/git-rg/releases/download/" + $Version
Invoke-WebRequest -Uri "$Base/$Asset" -OutFile $Asset
Invoke-WebRequest -Uri "$Base/checksums.txt" -OutFile checksums.txt
$pattern = " " + [regex]::Escape($Asset) + "$"
$expected = (Select-String -Path checksums.txt -Pattern $pattern).Line.Split()[0]
$actual = (Get-FileHash -Path $Asset -Algorithm SHA256).Hash.ToLowerInvariant()
if ($expected -ne $actual) { throw "checksum mismatch" }
Expand-Archive -Path $Asset -DestinationPath .
$dir = "git-rg_" + $Version + "_windows_amd64"
& (Join-Path $dir "git-rg.exe") --version
~~~

校验失败、Release 返回 HTML、archive 无法解压或版本输出不符合预期时，删除临时下载文件并从官方 Release 重新下载；不要执行未校验的文件。校验命令只保护 Release archive 的完整性，不能替代对来源、权限和操作系统安全策略的审查。

## 升级、降级和卸载

升级时重复运行同一种安装方式即可；安装脚本会先完整下载并校验，再替换目标文件：

~~~sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh \
  | sh -s -- --version v0.3.0
~~~

需要回到旧版本时显式指定已发布版本，例如 --version v0.1.0 或 -Version v0.1.0。旧版本仅保留下载，不再接受普通修复；详见 SUPPORT.md。

卸载命令取决于安装方式：

~~~sh
# 脚本/手工安装（默认目录）
rm -f "$HOME/.local/bin/git-rg"

# Homebrew
brew uninstall git-rg
~~~

~~~powershell
# PowerShell 脚本默认目录
Remove-Item (Join-Path $env:LOCALAPPDATA "Programs\git-rg\bin\git-rg.exe") -Force

# Scoop
scoop uninstall git-rg
~~~

通过 go install 安装的二进制位于 go env GOBIN，或 go env GOPATH 的第一个路径下的 bin 目录；可用以下命令定位并删除：

~~~sh
go_bin="$(go env GOBIN)"
if [ -z "$go_bin" ]; then go_bin="$(go env GOPATH)/bin"; fi
printf 'Removing %s\n' "$go_bin/git-rg"
rm -f "$go_bin/git-rg"
~~~

卸载二进制不会自动删除搜索缓存。缓存只是可丢弃的加速数据，默认位于 os.UserCacheDir()/git-rg；确认不再需要私有仓库缓存后再删除该目录。--no-cache 可防止后续运行写入磁盘缓存。

## 常见问题

- unsupported platform：当前预构建包只支持 Linux/macOS/Windows 的 amd64、arm64；其他平台请用 Go 1.26+ 源码构建，并按 best effort 使用。
- checksum mismatch：停止执行，重新下载同一版本的 checksums.txt 和 archive，确认没有代理替换或下载到错误架构。
- git-rg: command not found：检查安装目录是否在 PATH；脚本不会替用户修改 shell 配置，PowerShell 可用 -NoPathUpdate 显式关闭 PATH 更新。
- release asset not found：目标版本未提供当前 OS/架构资产，不能改用其他架构；改用支持的资产或源码构建。
- go install 失败：确认 Go 版本为 1.26+，并使用完整模块路径 github.com/SamuelSupe/git-rg/cmd/git-rg@VERSION。
