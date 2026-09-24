# Agent changes and draft PRs/MRs

Available since **v0.5.0** in the release binaries and source builds. See the [installation guide](installation.md) and [v0.5.0 release notes](releases/v0.5.0.md).

An external agent supplies the edited text. `git-rg` reads immutable files, validates a JSON plan, previews a diff, and publishes a single commit on a new branch followed by a draft GitHub PR or GitLab MR. It uses the provider API without cloning a target repository, executing its code, or invoking Git. Writes are **off by default**.

## Read and prepare

```sh
git-rg read --ref main github:OWNER/REPO path/to/file.go
git-rg read --ref main gitlab:GROUP/PROJECT path/to/file.go
```

The `file` NDJSON event contains `repository`, `commit`, `path`, `blob`, `mode`, and the full UTF-8 `content`, including its original line endings. A successful read verifies the downloaded bytes against the blob SHA. Use the returned `commit` to pin subsequent reads with `--ref`, so all edits use one snapshot. Files are read through the API; these commands do not use the search cache.

Create `changes.json`. Replace the two SHA placeholders below with the full lowercase values returned by `read`; `content` is the entire modified file, not a patch or a search result fragment:

```json
{
  "schema_version": 1,
  "base_commit": "COMMIT_SHA_FROM_READ",
  "base_branch": "main",
  "branch": "agent/fix-timeout",
  "title": "Fix timeout handling",
  "body": "Describe the problem, the resulting behavior, and actual validation.",
  "commit_message": "Fix timeout handling",
  "changes": [
    {
      "action": "update",
      "path": "path/to/file.go",
      "expected_blob": "BLOB_SHA_FROM_READ",
      "content": "complete replacement text\n"
    },
    {
      "action": "create",
      "path": "notes/fix.md",
      "content": "Explanation of the change.\n"
    }
  ]
}
```

| Action | Contract |
| --- | --- |
| `create` | Path must be absent; omit `expected_blob`; supply `content`. New files use mode `100644`. |
| `update` | Path must be a regular file with the supplied `expected_blob`; supply `content`. Preserve the original executable mode. |
| `delete` | Path must be a regular file with the supplied `expected_blob`; omit `content`. |

An empty `content` string creates or writes an empty file; omitting it is an error for create/update. Duplicate paths, unchanged updates, unknown fields, malformed paths, and file/directory collisions are rejected. `base_branch` names the PR/MR target; `branch` names a new source branch and must differ from the target and repository default branch.

## Preview and publish

```sh
# GET requests only. No write identity is used or needed.
git-rg propose --dry-run --changes changes.json github:OWNER/REPO

# Requires GITRG_WRITE_TOKEN supplied by your secret manager or environment.
git-rg propose --enable-write --changes changes.json github:OWNER/REPO
```

Replace the repository with `gitlab:GROUP/PROJECT` for GitLab. Put all flags before positional arguments. `--changes -` reads the plan from stdin. Both commands also accept `--provider`, `--api-base`, `--auth auto|env`, `--timeout` (default 5 minutes), and `--max-requests` (default 100, including retries; 0 is unlimited). Private hosts use the same repository and API-base conventions as search. Output is NDJSON only.

The timeout includes reading the plan. A pipe that remains open after sending JSON still times out with a `cancelled` error; the command does not wait indefinitely for EOF.

Without either `--dry-run` or an explicit true `--enable-write`, `propose` exits with `write_disabled` before reading the plan, acquiring credentials, or sending requests. There is no persistent setting or environment variable that enables writing. `--enable-write=false` keeps it disabled. `--dry-run` takes precedence over `--enable-write` and uses read credentials only.

Preview validates the immutable source files and emits a `preview` event with a unified `diff`. It does not certify write permissions, branch availability, current target-branch position, PR availability, or CI. Publishing emits the same preview, checks the current branches, creates the commit and source branch, verifies the commit's parent and complete tree, and creates a draft PR/MR. Ordinary searches and reads never publish anything.

## Agent identity

Optionally identify the agent that prepared a change:

```sh
git-rg propose --enable-write --changes changes.json \
  --agent-name "Codex" --agent-model "your-model-id" \
  --agent-run-id "run-20260924-001" github:OWNER/REPO
```

Alternatively, include this optional top-level field in the JSON plan:

```json
"agent": {
  "name": "Codex",
  "model": "your-model-id",
  "run_id": "run-20260924-001"
}
```

`name` is required when identity is supplied; `model` and `run_id` are optional. Each field allows up to 200 UTF-8 bytes without control characters. Explicit CLI flags override the corresponding JSON fields; omitted flags preserve them. Use the actual model identifier and a stable run ID from your agent, rather than generating a different ID on each retry. Nothing is inferred from the environment.

The PR/MR description retains the supplied `body` and appends an **Agent (self-reported)** section containing the identity as JSON. The final description, including that section, must fit within 64 KiB. The `preview` event also includes the resolved `agent` object, so `--dry-run` can inspect it without writing. Omit both the JSON field and the flags to retain the original description.

This attribution does not change the authenticated platform author or Git commit author; those still use the `GITRG_WRITE_TOKEN` identity. Identity is included in the plan fingerprint: recovery must use the same identity values as the original attempt. A different identity cannot silently reuse an existing proposal branch.

## Credentials

`propose --enable-write` requires a separate `GITRG_WRITE_TOKEN`. It does not fall back to `GITRG_TOKEN`, `GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN`, or a `gh`/`glab` login. The dedicated token authenticates both validation reads and publication writes in that invocation. Merely setting it does not enable writing, and search/read/preview commands never use it.

- **GitHub:** a repository-scoped fine-grained token or GitHub App identity with Contents write and Pull requests write permissions. Repository rules can impose additional requirements, such as signed commits or restrictions on workflow-file changes. See [Git trees](https://docs.github.com/en/rest/git/trees#create-a-tree) and [PR creation](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request).
- **GitLab:** an identity allowed to create source branches and merge requests, with API access (typically the `api` scope for a PAT or project access token). `write_repository` alone is not REST API write permission. Instance policies and branch protections still apply. See [token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/), [commits](https://docs.gitlab.com/api/commits/#create-a-commit), and [merge requests](https://docs.gitlab.com/api/merge_requests/#create-a-merge-request).

Use a repository-specific identity and keep the token out of plan files, command-line arguments, and source control. The token is sent in the Authorization header. Plan files, previews, and outputs contain source code and should receive the same access controls as the repository.

## Results and recovery

Each event has `schema_version: 1`. `read` returns a `file` event; dry-run returns `preview`; publication returns `preview` followed by `proposal`. Diagnostics are `warning` or `error` events. Success exits 0; a failed or incomplete operation exits 2.

The `proposal.result` object contains `branch`, `base_commit`, any known `commit`, `pull_request` (number, URL, head SHA, draft flag, state), `reused`, and `complete`. `complete: true` means the verified commit is on the source branch and a corresponding PR/MR exists. It does **not** mean tests passed, CI finished, or the PR was merged. GitLab can temporarily return an empty PR head SHA while preparing an MR; `result.commit` still identifies the verified branch commit.

- New publication requires the target branch to still point at `base_commit`. Otherwise it returns `base_changed`; read the new snapshot and regenerate the plan.
- An original file SHA mismatch returns `blob_conflict` before writes. An unrelated existing source branch or unexpected published tree returns `branch_conflict` and is never overwritten.
- The commit includes a `Git-Rg-Change` fingerprint of the plan and repository. Repeating the same plan verifies this marker, its single parent, and every tree entry before reusing that commit. This also checks unchanged files, symlinks, submodules, and executable modes.
- Before sending a GitLab commit, `git-rg` orders deletions before other actions, so replacing a directory with a file deletes its children first. This execution order does not change the canonical plan or recovery fingerprint.
- Failed or uncertain POST requests are not automatically retried, and write redirects are refused. A timeout may occur after the server accepted a commit, branch, or PR. Rerun the **same plan against the same repository/API base**: it inspects remote state and resumes without another commit or duplicate PR. A recovered commit can still be used if the target has since advanced; its original base SHA remains unchanged.
- Existing PRs/MRs are returned with their actual state, including closed or merged. They are not reopened or converted back to drafts. If a previous PR exists but its source branch was deleted, use a new branch name.
- On failure, inspect `result.complete: false`, any returned commit/URL, and the subsequent error. Remote objects are not rolled back or deleted. A GitHub commit may exist without a branch if ref creation failed; such an unreferenced object is harmless and a retry can create another object before publishing the branch.

This first version supports same-repository new branches and regular UTF-8 text files: at most 100 changed files, 1 MiB per file, 4 MiB each for total original and replacement contents, and 8 MiB for the JSON plan. It preserves executable modes when updating existing files. Editing binary files, Git LFS pointers, symlinks, submodules, fork PRs, existing branch updates, automatic merging, and running build/test commands are outside this command's scope. GitLab can apply repository LFS rules to newly created files; any resulting tree mismatch is reported before a MR is created. Verify builds and tests in CI or an agent-managed execution environment.
