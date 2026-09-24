package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

func githubRepositoryEndpoint(snapshot Snapshot) string {
	owner, repo, _ := strings.Cut(snapshot.Repository.Project, "/")
	return snapshot.Repository.APIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func (g *gitHub) BranchHead(ctx context.Context, snapshot Snapshot, branch string) (string, error) {
	var response struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_, err := g.client.getJSON(ctx, githubRepositoryEndpoint(snapshot)+"/git/ref/heads/"+escapeRepositoryPath(branch), &response)
	if isNotFound(err) {
		return "", nil
	}
	if err == nil && response.Object.SHA == "" {
		err = errors.New("GitHub returned an incomplete branch ref")
	}
	return response.Object.SHA, err
}

func (g *gitHub) CreateChange(ctx context.Context, snapshot Snapshot, branch, message string, changes []FileChange) (string, error) {
	base := githubRepositoryEndpoint(snapshot)
	tree := make([]map[string]any, 0, len(changes))
	for _, change := range changes {
		entry := map[string]any{"path": change.Path, "mode": change.Mode, "type": "blob"}
		if change.Action == "delete" {
			entry["sha"] = nil
		} else {
			entry["content"] = change.Content
		}
		tree = append(tree, entry)
	}
	var treeResponse struct {
		SHA string `json:"sha"`
	}
	if err := g.client.postJSON(ctx, base+"/git/trees", map[string]any{"base_tree": snapshot.TreeOID, "tree": tree}, &treeResponse); err != nil {
		return "", err
	}
	if treeResponse.SHA == "" {
		return "", errors.New("GitHub returned an empty tree SHA")
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := g.client.postJSON(ctx, base+"/git/commits", map[string]any{
		"message": message, "tree": treeResponse.SHA, "parents": []string{snapshot.Commit},
	}, &commit); err != nil {
		return "", err
	}
	if commit.SHA == "" {
		return "", errors.New("GitHub returned an empty commit SHA")
	}
	var ref struct {
		Ref string `json:"ref"`
	}
	err := g.client.postJSON(ctx, base+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": commit.SHA}, &ref)
	return commit.SHA, err
}

type githubPullRequest struct {
	Number   int     `json:"number"`
	URL      string  `json:"html_url"`
	State    string  `json:"state"`
	Draft    bool    `json:"draft"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
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

func (p githubPullRequest) result() PullRequest {
	state := p.State
	if p.MergedAt != nil {
		state = "merged"
	}
	return PullRequest{Number: p.Number, URL: p.URL, Head: p.Head.SHA, Draft: p.Draft, State: state}
}

func (g *gitHub) FindPullRequest(ctx context.Context, snapshot Snapshot, branch, baseBranch string) (*PullRequest, error) {
	owner, _, _ := strings.Cut(snapshot.Repository.Project, "/")
	values := url.Values{"head": {owner + ":" + branch}, "base": {baseBranch}, "state": {"all"}, "per_page": {"100"}}
	var response []githubPullRequest
	_, err := g.client.getJSON(ctx, githubRepositoryEndpoint(snapshot)+"/pulls?"+values.Encode(), &response)
	if err != nil {
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

func (g *gitHub) CreatePullRequest(ctx context.Context, snapshot Snapshot, branch, baseBranch, title, body string) (PullRequest, error) {
	var response githubPullRequest
	err := g.client.postJSON(ctx, githubRepositoryEndpoint(snapshot)+"/pulls", map[string]any{
		"head": branch, "base": baseBranch, "title": title, "body": body, "draft": true,
	}, &response)
	return response.result(), err
}
