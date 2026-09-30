# Creating issues

Available since **v0.7.0**. See the [installation guide](installation.md) and [release notes](releases/v0.7.0.md). Run `git-rg issue --help` or `git-rg issue create --help` for flags and examples.

Unpublished local development changes also support [Gitee](gitee.md). It requires a build containing the adapter changes, which are not yet in a release or the published `main` branch.

`git-rg issue create` creates one issue through the GitHub, GitLab, or Gitee API. It does not clone the repository or resolve a commit, so an empty repository can accept issues if the platform permits them. Remote writes are **off by default**.

## Preview and create

```sh
# Local validation and preview; no credentials or network access.
git-rg issue create --dry-run --title "Unexpected search result" --body-file issue.md github:OWNER/REPO

# Set GITRG_WRITE_TOKEN through your environment or secret manager first.
git-rg issue create --enable-write --title "Unexpected search result" --body-file issue.md github:OWNER/REPO

# A short inline description, or Markdown from stdin.
git-rg issue create --dry-run --title "Missing documentation" --body "Steps to reproduce..." gitlab:GROUP/PROJECT
cat issue.md | git-rg issue create --enable-write --title "Unexpected result" --body-file - gitlab:GROUP/PROJECT
```

Put flags before the repository. Use the same repository addresses, `--provider`, and `--api-base` conventions as the other commands. `--timeout` defaults to 5 minutes and includes reading a file or waiting for stdin to close. `--max-requests` defaults to 100; 0 means unlimited. Creation sends one POST and never automatically retries or follows redirects.

Without `--dry-run` or an explicit true `--enable-write`, the command exits with `write_disabled` before reading a body file or acquiring credentials. Setting a token alone does not enable writing. `--dry-run` takes precedence even alongside `--enable-write`: it validates the input and returns the final body locally, without checking repository existence, issue settings, or permissions. There is no `--auth` flag because creation always uses the write token and preview needs no authentication.

## Input and Agent identity

- `--title` is required: 1–200 UTF-8 bytes, non-blank and without control characters.
- The body is optional. Use either `--body` or `--body-file`, including `--body-file -` for stdin. The final UTF-8 Markdown body, including Agent attribution, is limited to 64 KiB and cannot contain NUL bytes.
- Optional `--agent-name`, `--agent-model`, and `--agent-run-id` use the same validation and self-reported JSON attribution as PR/MR proposals. A supplied identity requires a name. The authenticated platform author remains the token's account.

```sh
git-rg issue create --enable-write --title "Investigate regression" --body-file issue.md \
  --agent-name "Review Agent" --agent-model "model-name" --agent-run-id "run-123" github:OWNER/REPO
```

The command creates ordinary issues with a title and body. It does not manage labels, assignees, milestones, comments, edits, or closing issues. Creation is not idempotent: repeating a successful invocation creates another issue. `--agent-run-id` is attribution, not a deduplication key.

## Credentials

Creation requires `GITRG_WRITE_TOKEN`; read tokens and `gh`/`glab` logins are never substituted for it. The token needs permission for the requested issue operation, independently of code or PR/MR permissions:

- GitHub fine-grained tokens need **Issues: write** for the target repository. Code and pull-request write permissions alone are insufficient. See [Create an issue](https://docs.github.com/en/rest/issues/issues#create-an-issue).
- GitLab tokens need API access and a user/bot role allowed to create an issue in the project. Issues must be enabled. See [Create an issue](https://docs.gitlab.com/api/issues/#create-an-issue) and [token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/).
- Gitee tokens need the `issues` scope and permission to create repository issues. Supply the token as `GITRG_WRITE_TOKEN`; `GITEE_TOKEN` alone does not enable creation. See [Gitee Open API](https://gitee.com/api/v5/swagger).

The body and Agent metadata are submitted as issue content, so review the preview before publishing them.

## Output and uncertain responses

Output is NDJSON with `schema_version: 1`. Success exits 0; invalid input, disabled writes, API failure, or an unconfirmed result exits 2.

Dry-run returns one `preview` event containing `repository`, `title`, the final `body` when non-empty, and optional `agent`. Successful creation returns one `issue` event:

```json
{"type":"issue","schema_version":1,"repository":"https://github.com/OWNER/REPO","result":{"number":42,"url":"https://github.com/OWNER/REPO/issues/42","state":"open","complete":true}}
```

`number` is the repository-local GitHub number or GitLab IID. GitLab's `opened` state is normalized to `open`. `complete: true` requires a valid issue number, web URL, and state from the API response; preview never reports creation as complete.

Gitee source builds return the alphanumeric issue number in `result.identifier` (for example `"IABC42"`) and omit `number`. GitHub/GitLab numeric `number` values remain unchanged. A Gitee result must include a valid identifier, web URL, and state to report completion.

API rejections such as missing permission, disabled issues, validation failures, or rate limiting return `issue_create_failed`. Lost connections, request timeouts, server errors, blocked redirects, malformed responses, or incomplete success results can leave creation uncertain and return `issue_creation_uncertain`. These requests are not replayed. Check the repository's issue list before retrying, since an issue may already exist. If stdout fails after confirmed creation, the stderr diagnostic includes the created URL.
