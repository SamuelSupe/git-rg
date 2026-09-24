package provider

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func (g *gitLab) BranchHead(ctx context.Context, snapshot Snapshot, branch string) (string, error) {
	var response struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	_, err := g.client.getJSON(ctx, g.projectEndpoint(snapshot)+"/repository/branches/"+url.PathEscape(branch), &response)
	if isNotFound(err) {
		return "", nil
	}
	if err == nil && response.Commit.ID == "" {
		err = errors.New("GitLab returned an incomplete branch")
	}
	return response.Commit.ID, err
}

func (g *gitLab) CreateChange(ctx context.Context, snapshot Snapshot, branch, message string, changes []FileChange) (string, error) {
	// GitLab applies actions in order; delete children before replacing a directory
	// with a file. Keep the plan's canonical order unchanged for recovery fingerprints.
	changes = append([]FileChange(nil), changes...)
	sort.SliceStable(changes, func(i, j int) bool {
		return changes[i].Action == "delete" && changes[j].Action != "delete"
	})
	actions := make([]map[string]any, 0, len(changes))
	for _, change := range changes {
		action := map[string]any{"action": change.Action, "file_path": change.Path}
		if change.Action != "delete" {
			action["content"] = change.Content
		}
		if change.Action != "create" {
			action["last_commit_id"] = snapshot.Commit
		}
		actions = append(actions, action)
	}
	var commit struct {
		ID string `json:"id"`
	}
	err := g.client.postJSON(ctx, g.projectEndpoint(snapshot)+"/repository/commits", map[string]any{
		"branch": branch, "start_sha": snapshot.Commit, "commit_message": message, "actions": actions, "force": false,
	}, &commit)
	if err == nil && commit.ID == "" {
		err = errors.New("GitLab returned an empty commit ID")
	}
	return commit.ID, err
}

type gitlabMergeRequest struct {
	IID           int    `json:"iid"`
	URL           string `json:"web_url"`
	SHA           string `json:"sha"`
	Draft         bool   `json:"draft"`
	State         string `json:"state"`
	SourceBranch  string `json:"source_branch"`
	TargetBranch  string `json:"target_branch"`
	SourceProject int    `json:"source_project_id"`
	TargetProject int    `json:"target_project_id"`
}

func (m gitlabMergeRequest) result() PullRequest {
	return PullRequest{Number: m.IID, URL: m.URL, Head: m.SHA, Draft: m.Draft, State: m.State}
}

func (g *gitLab) FindPullRequest(ctx context.Context, snapshot Snapshot, branch, baseBranch string) (*PullRequest, error) {
	values := url.Values{"source_branch": {branch}, "target_branch": {baseBranch}, "state": {"all"}, "scope": {"all"}, "per_page": {"100"}}
	var response []gitlabMergeRequest
	headers, err := g.client.getJSON(ctx, g.projectEndpoint(snapshot)+"/merge_requests?"+values.Encode(), &response)
	if err != nil {
		return nil, err
	}
	if len(response) >= 100 || headers.Get("X-Next-Page") != "" {
		return nil, errors.New("too many matching merge requests")
	}
	var found *PullRequest
	for _, mr := range response {
		if mr.SourceBranch != branch || mr.TargetBranch != baseBranch || strconv.Itoa(mr.SourceProject) != snapshot.RemoteID || mr.SourceProject != mr.TargetProject {
			continue
		}
		if found != nil {
			return nil, errors.New("multiple merge requests use this branch; choose a new branch")
		}
		result := mr.result()
		found = &result
	}
	return found, nil
}

func (g *gitLab) CreatePullRequest(ctx context.Context, snapshot Snapshot, branch, baseBranch, title, body string) (PullRequest, error) {
	if !strings.HasPrefix(strings.ToLower(title), "draft:") {
		title = "Draft: " + title
	}
	var response gitlabMergeRequest
	err := g.client.postJSON(ctx, g.projectEndpoint(snapshot)+"/merge_requests", map[string]any{
		"source_branch": branch, "target_branch": baseBranch, "title": title, "description": body,
	}, &response)
	return response.result(), err
}
