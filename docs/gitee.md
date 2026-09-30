# Gitee development preview / Gitee 开发预览

Gitee support exists in unpublished local development changes and is not available from the published `main` branch or a release. Build a source tree containing the adapter changes to use it; v0.7.0 installers, package managers, and `go install ...@latest` do not include it. The adapter targets [Gitee Open API v5](https://gitee.com/api/v5/swagger), including its batch-commit and draft-PR APIs.

## Commands

Use `gitee:OWNER/REPO`, `https://gitee.com/OWNER/REPO.git`, or `git@gitee.com:OWNER/REPO.git`. For a private installation exposing the same API, pass `--provider gitee`; the default API base is `https://HOST/api/v5`, overridable with `--api-base`. Private installations remain best effort and need validation against their API version.

```sh
# Search the immutable snapshot and list branch/tag heads.
git-rg --mode exact --max-results 0 -F TODO gitee:OWNER/REPO
git-rg refs gitee:OWNER/REPO

# Read a complete file and validate an agent-generated change plan.
git-rg read --ref main gitee:OWNER/REPO path/to/file.go
git-rg propose --dry-run --changes changes.json gitee:OWNER/REPO

# Requires GITRG_WRITE_TOKEN and explicit write enablement.
git-rg propose --enable-write --changes changes.json \
  --agent-name "Review Agent" --agent-model "model-name" --agent-run-id run-123 gitee:OWNER/REPO

# Preview locally, then create an issue when explicitly enabled.
git-rg issue create --dry-run --title "Unexpected result" --body-file issue.md gitee:OWNER/REPO
git-rg issue create --enable-write --title "Unexpected result" --body-file issue.md \
  --agent-name "Review Agent" --agent-run-id run-123 gitee:OWNER/REPO
```

All flags precede positional arguments. The [change-plan format and recovery rules](agent-changes.md) and [issue input limits and attribution](issues.md) apply to Gitee too. Agent metadata is self-reported; the authenticated account remains the platform author.

## Credentials and writes

Read credential priority on Gitee.com is `GITRG_TOKEN` → `GITEE_TOKEN` → `GITRG_WRITE_TOKEN`. Private/custom API hosts use `GITRG_TOKEN` → `GITRG_WRITE_TOKEN`; a cloud `GITEE_TOKEN` is selected only when the Gitee API hostname is `gitee.com`. Both `--auth auto` and `--auth env` follow this order. Without a token, public reads are anonymous; Gitee never reuses `gh` or `glab` credentials.

Writes remain **off by default** and always require `--enable-write` plus `GITRG_WRITE_TOKEN`. `GITEE_TOKEN` and other read credentials are never substituted for a missing write token. The token must have repository access and the applicable Gitee scopes: `projects` for repository operations, `pull_requests` for PRs, and `issues` for issue creation. Repository permissions and branch rules still apply. Tokens are sent in the Authorization header, not URL query parameters.

Providing only `GITRG_WRITE_TOKEN` is sufficient for authenticated reads, previews, and explicitly enabled writes, as long as it has all required permissions. `--dry-run` takes precedence over `--enable-write`. Proposal previews make read requests; issue previews are entirely local.

Publication submits the file actions as one commit from the pinned base SHA on a new source branch, preserves executable modes, and then creates a draft PR. It verifies the resulting commit parent, message, full tree, branch head, and PR before reporting `complete: true`. A matching plan can recover a lost response; unrelated existing branches are rejected. Writes are not automatically replayed.

## Search and output differences

- `auto` and `exact` search the pinned tree/archive/blob content using the local matcher. Gitee's public API does not provide a code-search index, so `indexed` returns `indexed_search_unsupported` with exit code 2. `auto` skips index acceleration.
- Archive URLs use a `ref` query parameter pinned to the commit SHA. Blob responses contain base64 JSON; decoded bytes are checked against the immutable blob identity by the read/search workflow. The shared response, request, and repository limits still apply. An unavailable archive falls back to blob requests within the remaining budget.
- Gitee issue identifiers are alphanumeric strings such as `IABC42`. Their result uses the additive `identifier` field and omits `number`; GitHub/GitLab keep their existing numeric `number` field. NDJSON remains schema v1.

```json
{"type":"issue","schema_version":1,"repository":"https://gitee.com/OWNER/REPO","result":{"identifier":"IABC42","url":"https://gitee.com/OWNER/REPO/issues/IABC42","state":"open","complete":true}}
```

Issue creation does not deduplicate repeated invocations. On `issue_creation_uncertain`, inspect the remote issue list before retrying; `--agent-run-id` is attribution, not an idempotency key.

Public repository access does not guarantee anonymous archive downloads: an archive endpoint can require authentication even when tree/blob reads succeed. In that case, search falls back to individual blobs, which can exhaust the default 100-request budget or the provider's quota on larger repositories. Configure a token with repository access, narrow the search with `--glob`, or explicitly adjust the request budget as appropriate. A budget-limited scan returns `complete: false` and exit code 2; partial results are not evidence of a complete search.

## Validation boundary

The local development adapter has been checked against public Gitee repository reads, refs, searches, and proposal previews. Its development HTTP fixtures cover authenticated requests, batch-commit and draft-PR publication, issue creation, conflicts, lost replies, attribution, and write gating. Authenticated live writes and private Gitee installations have not been validated; no real PR or issue was created during these checks.

## 中文速查

本地开发改动已接入 Gitee，尚未发布到远端 `main` 或 Release。使用时须构建包含这些改动的源码；v0.7.0 安装包、包管理器和 `go install ...@latest` 不包含 Gitee。支持搜索、分支/tag 列表、完整文件读取、变更预览、批量提交及草稿 PR、issue 创建和 Agent 身份参数。

使用 `gitee:OWNER/REPO` 即可；读取凭证优先级为 `GITRG_TOKEN` → `GITEE_TOKEN` → `GITRG_WRITE_TOKEN`。只配置一个具备相应权限的 `GITRG_WRITE_TOKEN` 也能完成整个流程。写入默认关闭，每次都需要 `--enable-write`；dry-run 不会创建远端内容。

Gitee 使用 `auto` 或 `exact` 搜索，不支持 `indexed`。issue 结果的字符串编号放在 `result.identifier` 中，既有 GitHub/GitLab 的 `result.number` 类型不变。创建 issue 的响应不确定时应先检查远端；提案则使用相同计划和身份参数恢复。自建 Gitee 必须支持对应 Open API v5 接口，尤其是批量提交和草稿 PR。

公开仓库的归档下载也可能需要认证；匿名访问会回退到逐文件 blob 读取，可能耗尽请求预算或平台配额。可配置有仓库读取权限的 token，或使用 `--glob` 缩小范围；预算不足会明确返回不完整结果。
