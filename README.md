# git-rg

[![CI](https://github.com/SamuelSupe/git-rg/actions/workflows/ci.yml/badge.svg)](https://github.com/SamuelSupe/git-rg/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SamuelSupe/git-rg)](https://github.com/SamuelSupe/git-rg/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/SamuelSupe/git-rg/total)](https://github.com/SamuelSupe/git-rg/releases)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/doc/install)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

[English](README.md) | [简体中文](README.zh-CN.md)

> Search GitHub and GitLab repositories at an immutable commit—without cloning the target repository.

`git-rg` is a small Go command-line tool for remote code search. It resolves a branch, tag, or commit to a fixed commit SHA, reads only the API content needed for the selected search path, and emits agent-friendly NDJSON by default. It supports public, private, and self-managed GitHub/GitLab instances; it does not create a local checkout or download Git history.

## What's new in v0.4.1

- **Fix glab credential padding:** leading and trailing ASCII spaces or tabs around a stored PAT or OAuth access token are removed before validation, so a valid login can authenticate search and `refs`.
- **Keep credential validation:** embedded whitespace, control bytes, invalid UTF-8, empty credentials, CI job tokens, and conflicting helper fields remain rejected.

See the [v0.4.1 release notes](docs/releases/v0.4.1.md). This patch preserves the automatic authentication behavior introduced in [v0.4.0](docs/releases/v0.4.0.md), environment-token priority, and NDJSON schema v1. Thanks to [@coanor](https://github.com/coanor) for [PR #1](https://github.com/SamuelSupe/git-rg/pull/1).

## Install and run

Linux/macOS, installed to a user-owned directory:

```sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh \
  | sh -s -- --version v0.4.1 --bin-dir "$HOME/.local/bin"
git-rg --version
```

Windows PowerShell:

```powershell
$installer = Join-Path $env:TEMP "git-rg-install.ps1"
Invoke-WebRequest https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.ps1 -OutFile $installer
& $installer -Version v0.4.1
git-rg --version
```

The installers download a GitHub Release archive, verify its SHA-256 checksum, validate the archive layout, and then install the one platform binary. They do not need `sudo`, Git, or Go. See [Installation](docs/installation.md) for audited/manual installation, upgrades, and uninstall instructions.

## Quick start

```sh
# Search a public repository; the target repository is never cloned.
git-rg -F "TODO" github:OWNER/REPO

# Default output is NDJSON. Put flags before PATTERN and REPOSITORY.
git-rg --mode exact --format ndjson --glob '*.go' TODO github:OWNER/REPO

# Search a branch, tag, or commit and include a two-line context window.
git-rg --ref v1.2.3 --context 2 TODO https://github.com/OWNER/REPO.git

# List branch heads and tags without cloning.
git-rg refs github:OWNER/REPO
```

`PATTERN` is a Go regular expression unless `-F/--fixed-strings` is used. Use `--mode exact` when a complete audit matters; see [Search modes and completeness](#search-modes-and-completeness). For a private repository, reuse an existing `gh`/`glab` login or set a least-privilege token as described in [Permissions & credential best practices](#permissions--credential-best-practices).

## Contents

- [What's new in v0.4.1](#whats-new-in-v041)
- [Why no clone](#why-no-clone)
- [Install](#install)
- [Repository addresses](#repository-addresses)
- [Search modes and completeness](#search-modes-and-completeness)
- [List branches and tags](#list-branches-and-tags)
- [CLI reference](#cli-reference)
- [Output](#output)
- [Exit codes](#exit-codes)
- [Permissions & credential best practices](#permissions--credential-best-practices)
- [Cache](#cache)
- [Resource and platform boundaries](#resource-and-platform-boundaries)
- [Development, support, and license](#development-support-and-license)

## Why no clone

- There is no clone, checkout, local worktree, Git executable, or Git-history download for the target repository. An agent can ask about a remote snapshot without first managing a repository-sized directory.
- The ref is resolved to an immutable commit before content is read. Exact and auto searches load the complete commit tree, verify archive entries against blob IDs, and fetch omitted or changed files through the blob API. Matching runs locally with Go's RE2 engine.
- `exact` can still read every matching ordinary text file in the selected commit. “Without cloning” removes the checkout and history transfer; it does not mean that a full search reads no repository content or uses no network.
- SSH-style clone URLs are accepted for address parsing only. `git-rg` does not use SSH authentication or the Git transport.

The current providers are GitHub and GitLab. There is no offline mode, local-path search, write operation, history search, or generic Git-server protocol. Pre-built binaries are published through GitHub Releases.

## Install

v0.4.1 is the current supported release. Earlier v0.x releases remain downloadable for reproduction or rollback but are EOL; see [SUPPORT.md](SUPPORT.md) for the compatibility and lifecycle policy. A pre-built binary does not need Go at runtime. Source builds and `go install` require Go 1.26 or newer.

### Pre-built platform matrix

The six combinations below are Tier 1 and are shipped for every release:

| Operating system | Architecture | v0.4.1 asset | Support |
| --- | --- | --- | --- |
| Linux | amd64 (x86_64) | `git-rg_v0.4.1_linux_amd64.tar.gz` | Tier 1 |
| Linux | arm64 | `git-rg_v0.4.1_linux_arm64.tar.gz` | Tier 1 |
| macOS | amd64 (x86_64) | `git-rg_v0.4.1_darwin_amd64.tar.gz` | Tier 1 |
| macOS | arm64 | `git-rg_v0.4.1_darwin_arm64.tar.gz` | Tier 1 |
| Windows | amd64 (x86_64) | `git-rg_v0.4.1_windows_amd64.zip` | Tier 1 |
| Windows | arm64 | `git-rg_v0.4.1_windows_arm64.zip` | Tier 1 |

Each archive contains one top-level version directory and one executable. The v0.4.1 Release has nine assets: six platform archives, `install.sh`, `install.ps1`, and `checksums.txt`. The checksum file covers both installers and all six archives.

### GitHub Release (manual)

Download the matching asset from the [v0.4.1 Release](https://github.com/SamuelSupe/git-rg/releases/tag/v0.4.1), download `checksums.txt`, and verify before extracting:

```sh
version=v0.4.1
asset="git-rg_${version}_linux_amd64.tar.gz"
base="https://github.com/SamuelSupe/git-rg/releases/download/${version}"
curl -fL -o "$asset" "$base/$asset"
curl -fL -o checksums.txt "$base/checksums.txt"
grep " $asset$" checksums.txt | sha256sum -c -
tar -xzf "$asset"
"./git-rg_${version}_linux_amd64/git-rg" --version
```

On macOS, use `shasum -a 256` if `sha256sum` is unavailable; choose the `darwin_amd64` or `darwin_arm64` asset as appropriate. Windows PowerShell users can use `Get-FileHash` and `Expand-Archive`; the full example is in [docs/installation.md](docs/installation.md). Do not execute an archive or binary until its checksum and expected layout have been checked.

### `install.sh` (Linux/macOS)

The script supports amd64 and arm64, defaults to `$HOME/.local/bin`, and does not request root access:

```sh
curl -fsSL https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.sh \
  | sh -s -- --version v0.4.1 --bin-dir "$HOME/.local/bin"
```

Use `--version VERSION` for a fixed release or omit it for the latest release. Use `--bin-dir DIRECTORY` to select the destination. For a reviewable installation, download the script first, inspect it, and then run it. The complete option list and failure handling are in [docs/installation.md](docs/installation.md).

### `install.ps1` (Windows)

The script supports Windows amd64 and arm64. It defaults to `%LOCALAPPDATA%\Programs\git-rg\bin` and updates the current user's `PATH` unless `-NoPathUpdate` is supplied:

```powershell
$installer = Join-Path $env:TEMP "git-rg-install.ps1"
Invoke-WebRequest https://raw.githubusercontent.com/SamuelSupe/git-rg/main/install.ps1 -OutFile $installer
& $installer -Version v0.4.1
git-rg --version
```

Use `-InstallDir DIRECTORY` to choose a destination, `-NoPathUpdate` to leave `PATH` unchanged, or `-Version VERSION` to pin a release.

### Homebrew

```sh
brew install SamuelSupe/tap/git-rg
git-rg --version

# Upgrade or uninstall
brew upgrade SamuelSupe/tap/git-rg
brew uninstall git-rg
```

The formula uses the corresponding GitHub Release archive and its SHA-256 value.

### Scoop

```powershell
scoop bucket add samuelsupe https://github.com/SamuelSupe/scoop-bucket
scoop install git-rg
git-rg --version

# Upgrade or uninstall
scoop update git-rg
scoop uninstall git-rg
```

### `go install` and source builds

With Go 1.26 or newer:

```sh
# Pin the supported release.
go install github.com/SamuelSupe/git-rg/cmd/git-rg@v0.4.1

# Or follow the latest module version.
go install github.com/SamuelSupe/git-rg/cmd/git-rg@latest
```

To build from source, only clone this tool's source repository—not the repository you later search:

```sh
git clone https://github.com/SamuelSupe/git-rg.git
cd git-rg
go build -trimpath -o ./git-rg ./cmd/git-rg
./git-rg --version
```

Other `GOOS/GOARCH` combinations may be built with Go 1.26, but are best effort and do not receive a Tier 1 archive or dedicated release smoke guarantee.

## Repository addresses

Supported forms are:

```text
github:OWNER/REPO
gitlab:GROUP/PROJECT
https://HOST/OWNER/REPO.git
git@HOST:GROUP/PROJECT.git
```

Public cloud examples:

```sh
git-rg TODO github:OWNER/REPO
git-rg -F "release marker" https://github.com/OWNER/REPO.git
git-rg --provider gitlab TODO gitlab:GROUP/PROJECT
git-rg --ref v1.2.3 TODO https://gitlab.com/GROUP/PROJECT.git
```

GitHub Enterprise Server and self-managed GitLab require an explicit provider. By default their API bases are `https://HOST/api/v3` and `https://HOST/api/v4`; override them with `--api-base` when the deployment uses a different path:

```sh
export GITRG_TOKEN='read-only-token'
git-rg --provider github TODO https://github.example.com/OWNER/REPO.git

export GITRG_TOKEN='read-only-token'
git-rg --provider gitlab \
  --api-base https://gitlab.example.com/api/v4 \
  TODO git@gitlab.example.com:GROUP/PROJECT.git
```

`--api-base` must be an absolute `http://` or `https://` URL without user information, a query string, or a fragment. Use HTTPS in production. GitHub paths are exactly `OWNER/REPO`; GitLab paths may contain nested groups.

## Search modes and completeness

The command is `git-rg [FLAGS] PATTERN REPOSITORY`. Every mode first resolves `--ref` (or the provider's default branch) to an immutable commit. `--ref` accepts a branch name, tag name, or commit SHA; it does not switch a local checkout.

| Mode | What it does | Completeness meaning |
| --- | --- | --- |
| `exact` | Loads the complete immutable commit tree without calling the code index. It verifies selected archive entries against their Git blob IDs and reads omitted or rewritten entries from the blob API. If the archive is unavailable before the first ordinary file, it uses the tree and blobs. | A normal end is `summary.complete=true`. A result limit, timeout, request/provider error, cancellation, or resource limit makes the result incomplete. This is the mode for a full audit. |
| `auto` (default) | Uses the same complete tree and verified archive/blob scan as exact. A cached archive skips the index. On a cache miss, a literal prefix and enough request budget allow prefetching up to 16 indexed candidates before scanning the remaining files. | The final scan is exact for the selected commit, except when a result limit or hard error stops it. Index candidates are acceleration only; an index warning does not turn a complete archive scan into an incomplete result. |
| `indexed` | Searches provider index candidates only, validates/glob-filters/deduplicates their paths, and reads each candidate at the selected commit. It does not use the archive and does not fall back to a full scan when the index fails. | Always `summary.complete=false` with reason `indexed_mode`. Coverage and availability depend on the provider index, so a miss is not proof that the commit has no match. |

An incomplete or unavailable tree cannot establish a complete exact/auto result. Tree pagination counts toward `--max-requests`; large repositories may need a larger budget. Archive `export-ignore`, `export-subst`, and LFS expansion do not change the selected commit content that is searched.

When explicit globs narrow the tree, exact/auto uses validated blob cache hits before applying download thresholds. If all selected files are cached, no archive, index, or blob download is needed. For the remaining files, up to eight blobs can be downloaded directly: known sizes must total at most 8 MiB; multiple missing files must have known sizes and cover at most a quarter of the tree. A single missing file with an unknown size is also eligible. The request budget must leave room for retries. These download thresholds do not restrict cache hits. After more than eight cache misses, probing stops and the usual archive/index strategy handles the remaining files. Failed blob prefetches fall back to the archive, without repeating successful files.

`indexed` requires a non-empty literal prefix that Go can extract from the pattern. GitHub and GitLab candidate requests are bounded to at most 10 pages of 100 items and an 8 MiB candidate-path budget; provider coverage can still vary. Do not assume every indexed search is available or complete.

### Globs and context

`-g/--glob` is repeatable. A glob containing `/` matches the complete repository path; one without `/` matches the basename. `*`, `?`, character classes, and the recursive segment `**` are supported. A leading `!` excludes a path. If any positive glob is supplied, paths must match a positive rule; the last matching rule wins.

```sh
git-rg --mode exact --glob '*.go' --glob '!vendor/**' TODO github:OWNER/REPO
```

`-C/--context NUM` requests lines before and after each match. `-B/--before-context NUM` and `-A/--after-context NUM` set the two sides independently and override the corresponding side of `-C`.

### Request and result budgets

`--max-results` defaults to 200 matching lines; `0` means unlimited. Reaching the limit stops the scan early, sets `summary.truncated=true`, `summary.complete=false`, and `summary.reason="result_limit"`. The process can still exit `0` if at least one line matched. `--max-requests` defaults to 100 remote HTTP requests, including retries and redirects; `0` means unlimited. `--timeout` defaults to five minutes and is the deadline for network and local scanning work.

## List branches and tags

```text
git-rg refs [FLAGS] REPOSITORY
```

`refs` uses the provider's branch and tag APIs with full pagination. Results are validated, deduplicated, stably sorted, and not disk-cached because refs are mutable:

The interface is the `refs` subcommand (the list-refs operation); there is no `--list-refs` flag.

```sh
git-rg refs github:OWNER/REPO
git-rg refs --kind branch gitlab:GROUP/PROJECT
git-rg refs --kind tag gitlab:GROUP/PROJECT
```

Flags are `--kind all|branch|tag`, `--format ndjson|text`, `--max-requests NUM`, `--provider github|gitlab`, `--api-base URL`, `--auth auto|env`, and `--timeout DURATION`. A full listing uses pages of up to 100 items. A later-page error returns an error and never labels a partial list complete.

In NDJSON, the normal event sequence is `meta`, one `ref` event per item, and `summary`:

```ndjson
{"type":"meta","schema_version":1,"provider":"github","repository":"https://github.com/OWNER/REPO"}
{"type":"ref","kind":"branch","name":"main","commit":"0123456789abcdef","head_tags":["v1.2.0"]}
{"type":"ref","kind":"tag","name":"v1.2.0","commit":"0123456789abcdef","head_branches":["main"]}
{"type":"summary","summary":{"branches":1,"tags":1,"api_requests":2,"api_retries":0,"request_limit":100,"complete":true}}
```

`head_tags` and `head_branches` report only names whose heads point to exactly the same commit SHA. They do not prove historical branch membership or ancestry; `git-rg` does not download commit history. `--kind branch` and `--kind tag` query only that ref type and therefore do not produce cross-type association fields. If the search pattern itself is `refs`, use `git-rg -- refs REPOSITORY` so it is not interpreted as the subcommand.

## CLI reference

All flags must appear before `PATTERN REPOSITORY`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-F`, `--fixed-strings` | off | Treat `PATTERN` as a literal string. |
| `-i`, `--ignore-case` | off | Match without case sensitivity. |
| `-w`, `--word-regexp` | off | Require Unicode word boundaries; letters, numbers, marks, and `_` are word characters. |
| `-g`, `--glob GLOB` | unrestricted | Include a glob; prefix with `!` to exclude. Repeatable. |
| `-B`, `--before-context NUM` | `0` | Include NUM lines before each match. |
| `-A`, `--after-context NUM` | `0` | Include NUM lines after each match. |
| `-C`, `--context NUM` | `0` | Include NUM lines before and after each match. |
| `--ref REF` | provider default branch | Use a branch, tag, or commit SHA. |
| `--commit-info` | off | Add commit author, committer, dates, and message to output. |
| `--mode MODE` | `auto` | Select `auto`, `exact`, or `indexed`. |
| `--format FORMAT` | `ndjson` | Select `ndjson` or `text`. |
| `--max-results NUM` | `200` | Maximum matching lines; `0` means unlimited. |
| `--max-requests NUM` | `100` | Remote request budget, including retries and redirects; `0` means unlimited. |
| `--provider NAME` | inferred | `github` or `gitlab`; required for private/self-managed hosts. |
| `--api-base URL` | inferred | Override the provider API base URL. |
| `--auth MODE` | `auto` | Use environment credentials, then the target host's `gh`/`glab` login; `env` disables CLI credential lookup. |
| `--no-cache` | off | Disable this run's persistent disk-cache reads, writes, and pruning. |
| `--timeout DURATION` | `5m` | Overall command deadline, such as `30s` or `2m`. |
| `--version` | — | Print the version and exit `0`; no pattern or repository is required. |
| `-h`, `--help` | — | Print help and exit `0`. |

`--commit-info` reuses commit data returned while resolving the ref and does not add an API request. The author is the person recorded as the original author; the committer is the person who created the commit object. GitHub may include a linked username; GitLab commit responses normally provide names and email addresses but no reliable platform username.

## Output

### NDJSON (default)

Standard output contains one JSON object per line. Each event has a `type`; preflight failures may produce only an `error` without `meta` or `summary`. In NDJSON mode, warnings and errors are also rendered as escaped one-line diagnostics on stderr.

| Event | Main fields |
| --- | --- |
| `meta` | `schema_version` (currently `1`), `provider`, `repository`, `requested_ref`, `resolved_ref`, `commit`, `mode`; optional `commit_info`. |
| `match` | `path`, 1-based `line`, 1-based byte-offset `column`, line `text`, and `submatches`. |
| `context` | `path`, 1-based `line`, line `text`, and `context` equal to `before` or `after`. |
| `warning` / `error` | Stable `code` and `message`. |
| `summary` | Nested `summary` with counts, byte/request statistics, `complete`, `truncated`, `duration_ms`, and optional `reason`, `transport`, or `rate_limit`. |

For search commands, `duration_ms` covers argument processing, ref resolution, cache maintenance, scanning, and result writes up to the summary. Process startup and writing the summary itself are outside that measurement.

Search output batches NDJSON writes in a 64 KiB buffer. Metadata, the first match, warnings, errors, and the summary flush immediately; pending results also flush on a 100 ms timer so sparse matches remain visible to pipe consumers.

`submatches` contains every full match on the line, not capture groups. Each item is `{ "start": 0, "end": 4, "text": "TODO" }`, with zero-based, end-exclusive byte offsets. Match and context events retain `"text":""` for an empty line. `transport` may be `archive`, `blob_fallback`, or `blob`.

Example (values are illustrative):

```ndjson
{"type":"meta","schema_version":1,"provider":"github","repository":"https://github.com/OWNER/REPO","resolved_ref":"main","commit":"0123456789abcdef","mode":"exact"}
{"type":"match","path":"README.md","line":3,"column":1,"text":"TODO: document the API","submatches":[{"start":0,"end":4,"text":"TODO"}]}
{"type":"summary","summary":{"matched_lines":1,"matched_files":1,"scanned_files":2,"cache_hits":0,"downloaded_bytes":734,"skipped_binary":0,"api_requests":4,"api_retries":0,"request_limit":100,"transport":"archive","complete":true,"truncated":false,"duration_ms":18}}
```

### Text

With `--format text`, match lines are `path:line:column:text`; context lines are `path-line-text`. Metadata and summaries are omitted from stdout, while warnings and errors go to stderr. If the result is incomplete, stderr prints `incomplete_results` with its reason and truncation state. `--commit-info` writes commit metadata before match output.

## Exit codes

- `0`: the command found at least one matching line and did not fail. This includes a result-limit truncation and an `indexed` result with `complete=false`.
- `1`: the command completed without a matching line. For `indexed`, this is only “no match among the provider's candidates.”
- `2`: argument, pattern, glob, repository, provider, API, output, timeout/cancellation, resource-limit, or request-budget failure; an indexed pattern without a literal prefix and an indexed request failure also use `2`.

Warnings such as `index_unavailable` do not change the exit code. `SIGINT` and `SIGTERM` cancel the command; unless it had already stopped for `result_limit`, the search is incomplete and exits `2`. `--version` exits `0` without contacting a provider.

<a id="permissions--credential-best-practices"></a>
## Permissions & credential best practices

`git-rg` sends credentials in the `Authorization: Bearer` request header. Use the narrowest read-only identity that can read the target repository and its metadata.

### Automatic CLI credentials

Since v0.4.0, `git-rg` defaults to `--auth auto` for both search and `refs`. Unlike the earlier environment-only behavior, an existing login can now authenticate a run without exporting a token:

```sh
# Sign in once with the corresponding CLI, if necessary.
gh auth login --hostname github.com
git-rg TODO github:OWNER/REPO

glab auth login --hostname gitlab.example.com
git-rg refs --provider gitlab https://gitlab.example.com/GROUP/PROJECT.git

# Keep the previous environment-only behavior, including in CI.
git-rg --auth env TODO github:OWNER/REPO
git-rg refs --auth env github:OWNER/REPO
```

A non-empty environment token always wins, with the order below unchanged. Otherwise, `git-rg` invokes `gh auth token --hostname HOST`, or checks `glab auth status --hostname HOST` before using `glab auth git-credential get`. It uses the current account for that host and keeps the access token only in memory. Reused credentials retain their existing permissions. GitLab PAT and OAuth credentials are supported; CI job tokens returned by the helper are not. The hidden glab helper must be available and work with your version's credential store and OAuth refresh support. An incompatible helper produces a warning; `git-rg` does not parse credential files or implement token refresh. glab may refresh and save its own OAuth credentials during lookup.

Automatic lookup requires HTTPS repository/API URLs with matching origins (including GitHub's `github.com` → `api.github.com` mapping). Self-managed hosts and custom API paths on the expected origin are supported; a different API host or port skips lookup with a warning. Use `GITRG_TOKEN` explicitly for those deployments. Helpers run without a shell or interactive stdin in a temporary working directory, with credential/host overrides and CI auto-login disabled; user configuration directories, proxy settings, and system credential stores remain available. System credential-store access may still require OS authorization.

A missing CLI silently leaves the run anonymous. Failed, unavailable, timed-out, or malformed credentials produce one `auth_unavailable` warning and continue anonymously. Warnings appear on stderr and, for NDJSON, as a `warning` event that can precede `meta`. API authentication failures do not trigger another identity or anonymous retry. The entire helper phase has a 10-second deadline within `--timeout`; an overall timeout or cancellation stops the command. Each helper invocation's combined stdout/stderr is capped at 64 KiB, and raw helper output is never forwarded. `--max-requests` counts only `git-rg` API requests, not glab's status check or OAuth refresh requests.

### GitHub

- Prefer a [fine-grained personal access token](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens) limited to the selected target repository, with `Contents: read`. Review the [fine-grained permission requirements](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens) for the endpoints enabled by your GitHub/GHES policy.
- A classic PAT's `repo` scope is substantially broader. Use it only when an organization policy or GHES deployment genuinely requires it, and keep the identity otherwise read-only. GitHub's [credential security guidance](https://docs.github.com/en/rest/authentication/keeping-your-api-credentials-secure) is relevant to storage and handling.

### GitLab

- Prefer a project or group access token, or a restricted PAT, with the documented `read_api` scope and an account/bot that has permission to read the project. See GitLab's [access-token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/) and [Repository Files API](https://docs.gitlab.com/api/repository_files/).
- Do not grant `api` or `write_repository` to this read-only search tool. Do not assume `read_repository` alone covers every project-metadata, ref, tree, search, archive, and raw-file API used by a run; scope availability also depends on the GitLab edition and instance policy.

### Token lookup and handling

Environment lookup runs first in both authentication modes:

| Target | Environment variables, in order |
| --- | --- |
| Any provider, including self-managed | `GITRG_TOKEN` is always checked first and wins when non-empty. |
| `github.com` | After `GITRG_TOKEN`, `GITHUB_TOKEN`, then `GH_TOKEN`. These cloud variables are not used for self-managed GitHub. |
| `gitlab.com` | After `GITRG_TOKEN`, `GITLAB_TOKEN`. This cloud variable is not used for self-managed GitLab. |
| Self-managed host | Use `GITRG_TOKEN`; no other environment fallback. `auto` then tries the matching CLI login. |

There is no token CLI flag. Repository URLs and `--api-base` reject embedded credentials, so never put a token in a URL. An unset token may still work for public endpoints; access is decided by the provider. Environment variables are ordinary process inputs—do not promise that a same-user process or diagnostic tool cannot observe them.

Use short-lived tokens, rotate and revoke them, and keep CI values masked/protected or in a secret manager. A long-running agent should use an independent read-only identity and short-lived credentials. NDJSON and text output can contain private source, paths, commit messages, and provider diagnostics; protect stdout, stderr, logs, artifacts, and downstream storage as sensitive data.

Internal CA deployments should install the CA into the system trust chain used by the operating system and Go's standard TLS stack. `git-rg` has no `--insecure` flag; do not bypass certificate verification. SSH-style URLs only parse a repository address and never provide SSH authentication.

## Cache

The persistent cache stores commit-pinned tree, blob, archive, and indexed raw-file entries. Each `.entry` file contains a versioned header, a SHA-256 checksum, and its payload, published with one rename; tokens are not a cache input. Legacy payload/sidecar pairs are treated as cache misses and remain eligible for capacity pruning. Mutable branch/tag listings are queried fresh rather than disk-cached.

- The default cache root is `os.UserCacheDir()/git-rg`. Typical locations are `~/Library/Caches/git-rg` on macOS, `$XDG_CACHE_HOME/git-rg` or `~/.cache/git-rg` on Linux, and `%LocalAppData%\git-rg` on Windows. The actual path is platform/runtime dependent.
- The default cache capacity is 512 MiB total. Commands check whether pruning is due at startup/end: a successful cache write triggers cleanup, while read-only runs share a five-minute cleanup timestamp. Pruning remains best effort under concurrent commands; stale temporary files and orphan checksum sidecars older than 24 hours may be removed.
- The program attempts to use `0700` cache directories and `0600` cache files. Actual permission semantics depend on the OS, filesystem, umask, ACLs, and account setup; treat private cache contents as sensitive and verify the effective permissions in your environment.
- `--no-cache` disables persistent cache reads, writes, and pruning for that run. Results first use a memory buffer with a conservative 256 KiB byte budget per worker. Larger results use a binary spool in an OS temporary file with mode `0600`, up to 32 MiB per file, and are removed after use; `--no-cache` does not disable this spool. A high-sensitivity or temporary agent should use `--no-cache` and also protect/isolate the OS temporary directory.
- Cache or spool limits, invalid checksums, and cache I/O failures are reported explicitly; an invalid cache entry is not trusted as repository content. A cache-directory setup failure emits `cache_disabled` and the search may continue without persistent caching.

## Resource and platform boundaries

The limits below are deliberate bounds, not capacity promises:

| Area | Bound |
| --- | --- |
| One text line | 8 MiB |
| One text file scanned | 512 MiB |
| Before-context buffer | 32 MiB |
| Full matches on one line | 100,000 |
| One result spool | 256 KiB memory budget, then up to 32 MiB on disk; at most 8 workers |
| Compressed archive input | 512 MiB |
| Expanded archive output | 4 GiB |
| Archive headers/path metadata | 1,000,000 headers / 128 MiB; one path is at most 1 MiB |
| Provider JSON response | 16 MiB |
| Complete tree collection | 1,000,000 items / 128 MiB metadata / 1,000 logical pagination or subtree requests |
| Ref listing | 100,000 refs / 32 MiB metadata / 1,000 logical pagination requests |
| Indexed candidates | 10 pages of up to 100 items / 8 MiB path metadata |

Exceeding a local or provider bound returns an explicit `resource_limit` error. GitHub's Git Blob API path rejects objects larger than 100 MiB; archive reads have their own compressed, expanded, and local-file limits. Files are probed for NUL bytes and invalid UTF-8; binary/invalid-UTF-8 files are skipped, and a NUL discovered after provisional matches discards that file's staged events. A result limit stops matching before the remaining suffix, so no text/binary inspection claim is made for that suffix. Archive entries are still read to their end, within resource limits, to verify the blob hash before results are emitted.

Each HTTP request gets at most three attempts. The client respects usable `Retry-After`/rate-limit reset delays, allows at most five redirects, refuses HTTPS downgrade, and strips authorization headers when following an HTTPS cross-host redirect. `--max-requests` counts requests made for ref resolution, indexes, trees, archives, blobs, retries, and redirects.

The six Tier 1 binaries are Linux, macOS, and Windows on amd64 and arm64. Release builds use `CGO_ENABLED=0`; other platforms can be built from source with Go 1.26 on a best-effort basis. GitHub.com and GitLab.com are the primary SaaS targets. GHES and self-managed GitLab are supported on a best-effort basis through their REST APIs, without a promised minimum server version; validate the target instance with its own provider, API base, token, and representative repositories.

For GitHub tree behavior, see the official [Git Trees API](https://docs.github.com/en/rest/git/trees). The tool requires network access to a supported API and currently provides no offline mode, SSH authentication, or generic Git-server compatibility.

## Development, support, and license

Build the command with Go 1.26 or newer:

```sh
go build -trimpath -o ./git-rg ./cmd/git-rg
```

CI runs `go test -race ./...`, `go vet ./...`, and a build, followed by native smoke checks for the six Tier 1 combinations. The release workflow builds checksummed archives and exercises installer and public GitHub/GitLab smoke paths. For support boundaries, report `git-rg --version`, OS/architecture, provider, mode, cache choice, a redacted command, and redacted NDJSON `meta`/`summary`/`error`; never include tokens, private source, or private cache files. Read [SUPPORT.md](SUPPORT.md) before relying on a non-Tier-1 platform or an EOL release.

Run the local performance matrix with `go test ./internal/search -run '^$' -bench BenchmarkRunner -benchmem -count 3`. It measures cold/warm cache, many small files, dense matches, and a single-file exact glob direct-blob path, including first-match latency and provider request counts; it does not model remote network latency.

Use `go test ./internal/cli -run '^$' -bench BenchmarkCLI -benchmem -count 3` for the complete CLI call, including ref resolution through a local HTTP fixture, cache maintenance, and NDJSON file output. It also covers large cache directories; external network latency and process startup are excluded.

Released under the [MIT License](LICENSE).
