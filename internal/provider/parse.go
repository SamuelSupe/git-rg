package provider

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

func ParseRepository(raw, explicitProvider, apiBase string) (Repository, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Repository{}, errors.New("repository is empty")
	}

	providerName := strings.ToLower(strings.TrimSpace(explicitProvider))
	var host, project string

	switch {
	case strings.HasPrefix(raw, "github:"):
		if providerName != "" && providerName != "github" {
			return Repository{}, errors.New("repository shorthand conflicts with --provider")
		}
		providerName, host, project = "github", "github.com", strings.TrimPrefix(raw, "github:")
	case strings.HasPrefix(raw, "gitlab:"):
		if providerName != "" && providerName != "gitlab" {
			return Repository{}, errors.New("repository shorthand conflicts with --provider")
		}
		providerName, host, project = "gitlab", "gitlab.com", strings.TrimPrefix(raw, "gitlab:")
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return Repository{}, fmt.Errorf("parse repository URL: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ssh" {
			return Repository{}, fmt.Errorf("unsupported repository URL scheme %q", u.Scheme)
		}
		hasPassword := false
		if u.User != nil {
			_, hasPassword = u.User.Password()
		}
		if u.Hostname() == "" || u.User != nil && u.Scheme != "ssh" || hasPassword || u.RawQuery != "" || u.Fragment != "" {
			return Repository{}, errors.New("repository URL must have a host and must not contain credentials")
		}
		host, project = u.Host, strings.TrimPrefix(u.Path, "/")
	default:
		// SCP-like SSH clone URL: git@example.com:group/project.git.
		at := strings.LastIndex(raw, "@")
		colon := strings.Index(raw, ":")
		if colon <= at+1 || colon == len(raw)-1 {
			return Repository{}, errors.New("repository must be a github:/gitlab: shorthand or clone URL")
		}
		host, project = raw[at+1:colon], raw[colon+1:]
	}

	host = strings.ToLower(strings.TrimSpace(host))
	project = strings.Trim(strings.TrimSuffix(strings.TrimSpace(project), ".git"), "/")
	if host == "" || project == "" {
		return Repository{}, errors.New("repository host or project path is invalid")
	}
	if providerName == "" {
		switch strings.Split(host, ":")[0] {
		case "github.com":
			providerName = "github"
		case "gitlab.com":
			providerName = "gitlab"
		default:
			return Repository{}, errors.New("--provider is required for a private GitHub or GitLab host")
		}
	}
	if providerName != "github" && providerName != "gitlab" {
		return Repository{}, fmt.Errorf("unsupported provider %q", providerName)
	}

	parts := strings.Split(project, "/")
	if len(parts) < 2 {
		return Repository{}, errors.New("repository project path must include an owner or group")
	}
	if providerName == "github" && len(parts) != 2 {
		return Repository{}, errors.New("GitHub repository path must be owner/repository")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return Repository{}, errors.New("repository project path is invalid")
		}
	}

	base, err := resolveAPIBase(providerName, host, apiBase)
	if err != nil {
		return Repository{}, err
	}
	webScheme := "https"
	if strings.HasPrefix(raw, "http://") {
		webScheme = "http"
	}

	return Repository{
		Provider: providerName,
		Host:     host,
		Project:  path.Clean(project),
		WebURL:   webScheme + "://" + host + "/" + project,
		APIBase:  base,
	}, nil
}

func resolveAPIBase(providerName, host, override string) (string, error) {
	if override != "" {
		u, err := url.Parse(strings.TrimRight(override, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", errors.New("--api-base must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		return strings.TrimRight(u.String(), "/"), nil
	}
	if providerName == "github" {
		if host == "github.com" || host == "github.com:443" {
			return "https://api.github.com", nil
		}
		return "https://" + host + "/api/v3", nil
	}
	return "https://" + host + "/api/v4", nil
}
