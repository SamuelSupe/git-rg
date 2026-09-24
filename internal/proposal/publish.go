package proposal

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/SamuelSupe/git-rg/internal/provider"
)

type Result struct {
	Branch      string                `json:"branch"`
	BaseCommit  string                `json:"base_commit"`
	Commit      string                `json:"commit,omitempty"`
	PullRequest *provider.PullRequest `json:"pull_request,omitempty"`
	Reused      bool                  `json:"reused"`
	Complete    bool                  `json:"complete"`
}

// Publish only creates a branch. Repeating an identical plan recovers a matching
// commit or PR; unrelated existing branches are never updated or deleted.
func (p *Prepared) Publish(ctx context.Context, remote provider.ChangeProvider) (Result, error) {
	plan, snapshot := p.Plan, p.Snapshot
	result := Result{Branch: plan.Branch, BaseCommit: snapshot.Commit}
	head, err := remote.BranchHead(ctx, snapshot, plan.Branch)
	if err != nil {
		return result, err
	}
	if head == "" {
		previous, err := remote.FindPullRequest(ctx, snapshot, plan.Branch, plan.BaseBranch)
		if err != nil {
			return result, err
		}
		if previous != nil {
			return result, errors.New("a PR/MR already uses this branch name; choose a new branch")
		}
		baseHead, err := remote.BranchHead(ctx, snapshot, plan.BaseBranch)
		if err != nil {
			return result, err
		}
		if baseHead != snapshot.Commit {
			return result, ErrBaseChanged
		}
		result.Commit, err = remote.CreateChange(ctx, snapshot, plan.Branch, p.Message, p.Changes)
		if err != nil {
			return result, fmt.Errorf("create commit/branch (rerun the same plan to recover): %w", err)
		}
		head = result.Commit
	} else {
		result.Commit, result.Reused = head, true
	}
	if err := p.verifyCommit(ctx, remote, head); err != nil {
		return result, err
	}
	if err := p.verifyBranch(ctx, remote, head); err != nil {
		return result, err
	}
	pr, err := remote.FindPullRequest(ctx, snapshot, plan.Branch, plan.BaseBranch)
	if err != nil {
		return result, err
	}
	if pr == nil {
		created, err := remote.CreatePullRequest(ctx, snapshot, plan.Branch, plan.BaseBranch, plan.Title, plan.pullRequestBody())
		if created.Number > 0 || created.URL != "" {
			result.PullRequest = &created
		}
		if err != nil {
			return result, fmt.Errorf("create PR/MR (rerun the same plan to recover): %w", err)
		}
		if !created.Draft {
			return result, errors.New("provider did not create a draft PR/MR")
		}
		pr = &created
	} else {
		result.Reused = true
	}
	result.PullRequest = pr
	u, err := url.Parse(pr.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || pr.Number <= 0 {
		return result, errors.New("provider returned an incomplete PR/MR response; rerun the same plan to recover")
	}
	// GitLab can leave SHA empty while a new MR is being prepared. The branch
	// verification still pins the published commit, without inventing CI status.
	if pr.Head != "" && pr.Head != head {
		return result, ErrBranchConflict
	}
	if err := p.verifyBranch(ctx, remote, head); err != nil {
		return result, err
	}
	result.Complete = true
	return result, nil
}

func (p *Prepared) verifyBranch(ctx context.Context, remote provider.ChangeProvider, commit string) error {
	head, err := remote.BranchHead(ctx, p.Snapshot, p.Plan.Branch)
	if err != nil {
		return err
	}
	if head != commit {
		return ErrBranchConflict
	}
	return nil
}

func (p *Prepared) verifyCommit(ctx context.Context, remote provider.ChangeProvider, commit string) error {
	if !validOID(commit) {
		return errors.New("provider returned an invalid commit SHA")
	}
	snapshot, err := remote.Resolve(ctx, p.Snapshot.Repository, commit)
	if err != nil {
		return err
	}
	if snapshot.Commit != commit || len(snapshot.Parents) != 1 || snapshot.Parents[0] != p.Snapshot.Commit || snapshot.CommitInfo == nil || strings.TrimRight(snapshot.CommitInfo.Message, "\n") != p.Message {
		return ErrBranchConflict
	}
	tree, err := loadTree(ctx, remote, snapshot)
	if err != nil {
		return err
	}
	if len(tree) != len(p.expected) {
		return ErrBranchConflict
	}
	for name, want := range p.expected {
		if err := ctx.Err(); err != nil {
			return err
		}
		got, ok := tree[name]
		if !ok || got.OID != want.OID || got.Mode != want.Mode {
			return ErrBranchConflict
		}
	}
	return nil
}
