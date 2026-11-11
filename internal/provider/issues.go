package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

type Issue struct {
	Number     int    `json:"number,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	URL        string `json:"url"`
	State      string `json:"state"`
}

// IssueCreator does not require a commit or a populated Git repository.
// An error after sending the request can leave creation unconfirmed; do not
// automatically retry, since the APIs do not provide a shared idempotency key.
type IssueCreator interface {
	CreateIssue(context.Context, Repository, string, string) (Issue, error)
}

func (i Issue) validate() error {
	u, err := url.Parse(i.URL)
	if (i.Number <= 0) == (i.Identifier == "") || i.Identifier != "" && !validIssueIdentifier(i.Identifier) || err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || (i.State != "open" && i.State != "closed") {
		return errors.New("remote API returned an incomplete or invalid issue result")
	}
	return nil
}

func validIssueIdentifier(value string) bool {
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

func (g *gitee) CreateIssue(ctx context.Context, repo Repository, title, body string) (Issue, error) {
	owner, name, _ := strings.Cut(repo.Project, "/")
	var response struct {
		Number string `json:"number"`
		URL    string `json:"html_url"`
		State  string `json:"state"`
	}
	err := g.client.postJSON(ctx, repo.APIBase+"/repos/"+url.PathEscape(owner)+"/issues", map[string]string{
		"repo": name, "title": title, "body": body,
	}, &response)
	issue := Issue{Identifier: response.Number, URL: response.URL, State: response.State}
	if err == nil {
		err = issue.validate()
	}
	return issue, err
}

func (g *gitHub) CreateIssue(ctx context.Context, repo Repository, title, body string) (Issue, error) {
	var response struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
		State  string `json:"state"`
	}
	err := g.client.postJSON(ctx, githubRepositoryEndpoint(repo)+"/issues", map[string]string{
		"title": title, "body": body,
	}, &response)
	issue := Issue{Number: response.Number, URL: response.URL, State: response.State}
	if err == nil {
		err = issue.validate()
	}
	return issue, err
}

func (g *gitLab) CreateIssue(ctx context.Context, repo Repository, title, body string) (Issue, error) {
	var response struct {
		IID   int    `json:"iid"`
		URL   string `json:"web_url"`
		State string `json:"state"`
	}
	err := g.client.postJSON(ctx, repo.APIBase+"/projects/"+url.PathEscape(repo.Project)+"/issues", map[string]string{
		"title": title, "description": body,
	}, &response)
	if response.State == "opened" {
		response.State = "open"
	}
	issue := Issue{Number: response.IID, URL: response.URL, State: response.State}
	if err == nil {
		err = issue.validate()
	}
	return issue, err
}
