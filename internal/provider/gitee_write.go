package provider

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
)

func (g *gitee) BranchHead(ctx context.Context, snapshot Snapshot, branch string) (string, error) {
	var response struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	_, err := g.client.getJSON(ctx, giteeRepositoryEndpoint(snapshot.Repository)+"/branches/"+url.PathEscape(branch), &response)
	if isNotFound(err) {
		return "", nil
	}
	if err == nil && response.Commit.SHA == "" {
		err = errors.New("Gitee returned an incomplete branch")
	}
	return response.Commit.SHA, err
}

func (g *gitee) CreateChange(ctx context.Context, snapshot Snapshot, branch, baseBranch, message string, changes []FileChange) (string, error) {
	changes = append([]FileChange(nil), changes...)
	sort.SliceStable(changes, func(i, j int) bool {
		return changes[i].Action == "delete" && changes[j].Action != "delete"
	})
	actions := make([]map[string]any, 0, len(changes))
	for _, change := range changes {
		action := map[string]any{"action": change.Action, "path": change.Path}
		if change.Action != "delete" {
			action["content"] = change.Content
			action["encoding"] = "text"
			action["execute_filemode"] = change.Mode == "100755"
		}
		if change.Action != "create" {
			action["last_commit_id"] = snapshot.Commit
		}
		actions = append(actions, action)
	}
	var response struct {
		SHA string `json:"sha"`
	}
	err := g.client.postJSON(ctx, giteeRepositoryEndpoint(snapshot.Repository)+"/commits", map[string]any{
		"branch": branch, "start_branch": baseBranch, "message": message, "actions": actions,
	}, &response)
	if err == nil && response.SHA == "" {
		err = errors.New("Gitee returned an empty commit SHA")
	}
	return response.SHA, err
}

type giteePullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p giteePullRequest) result() PullRequest {
	return PullRequest{Number: p.Number, URL: p.URL, Head: p.Head.SHA, Draft: p.Draft, State: p.State}
}

func (g *gitee) FindPullRequest(ctx context.Context, snapshot Snapshot, branch, baseBranch string) (*PullRequest, error) {
	values := url.Values{"head": {branch}, "base": {baseBranch}, "state": {"all"}, "per_page": {"100"}}
	var response []giteePullRequest
	if _, err := g.client.getJSON(ctx, giteeRepositoryEndpoint(snapshot.Repository)+"/pulls?"+values.Encode(), &response); err != nil {
		return nil, err
	}
	if len(response) >= 100 {
		return nil, errors.New("too many matching pull requests")
	}
	var found *PullRequest
	for _, pr := range response {
		if pr.Head.Ref != branch || pr.Base.Ref != baseBranch || !strings.EqualFold(pr.Head.Repo.FullName, snapshot.Repository.Project) {
			continue
		}
		if found != nil {
			return nil, errors.New("multiple pull requests use this branch; choose a new branch")
		}
		result := pr.result()
		found = &result
	}
	return found, nil
}

func (g *gitee) CreatePullRequest(ctx context.Context, snapshot Snapshot, branch, baseBranch, title, body string) (PullRequest, error) {
	var response giteePullRequest
	err := g.client.postJSON(ctx, giteeRepositoryEndpoint(snapshot.Repository)+"/pulls", map[string]any{
		"head": branch, "base": baseBranch, "title": title, "body": body, "draft": true,
	}, &response)
	return response.result(), err
}
