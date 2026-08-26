package provider

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

func New(repository Repository, timeout time.Duration) (Provider, error) {
	return NewWithOptions(repository, Options{Timeout: timeout})
}

func NewWithOptions(repository Repository, options Options) (Provider, error) {
	token := tokenFor(repository)
	switch repository.Provider {
	case "github":
		return newGitHubWithOptions(repository, token, options), nil
	case "gitlab":
		return newGitLabWithOptions(repository, token, options), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", repository.Provider)
	}
}

func tokenFor(repository Repository) string {
	if token := os.Getenv("GITRG_TOKEN"); token != "" {
		return token
	}
	apiURL, _ := url.Parse(repository.APIBase)
	apiHost := apiURL.Hostname()
	if repository.Provider == "github" && apiHost == "api.github.com" {
		if token := os.Getenv("GITHUB_TOKEN"); token != "" {
			return token
		}
		return os.Getenv("GH_TOKEN")
	}
	if repository.Provider == "gitlab" && apiHost == "gitlab.com" {
		return os.Getenv("GITLAB_TOKEN")
	}
	return ""
}

func setGitHubHeaders(token string) func(*http.Request) {
	return func(req *http.Request) {
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("User-Agent", "git-rg/1")
	}
}

func setGitLabHeaders(token string) func(*http.Request) {
	return func(req *http.Request) {
		if token != "" {
			req.Header.Set("PRIVATE-TOKEN", token)
		}
		req.Header.Set("User-Agent", "git-rg/1")
	}
}
