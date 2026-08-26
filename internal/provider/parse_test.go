package provider

import "testing"

func TestParseRepositoryForms(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		provider string
		apiBase  string
		want     Repository
	}{
		{
			name: "github shorthand",
			raw:  "github:octocat/Hello-World",
			want: Repository{Provider: "github", Host: "github.com", Project: "octocat/Hello-World", WebURL: "https://github.com/octocat/Hello-World", APIBase: "https://api.github.com"},
		},
		{
			name: "gitlab shorthand",
			raw:  "gitlab:gitlab-org/gitlab-test",
			want: Repository{Provider: "gitlab", Host: "gitlab.com", Project: "gitlab-org/gitlab-test", WebURL: "https://gitlab.com/gitlab-org/gitlab-test", APIBase: "https://gitlab.com/api/v4"},
		},
		{
			name: "https clone URL",
			raw:  "https://github.com/octocat/Hello-World.git",
			want: Repository{Provider: "github", Host: "github.com", Project: "octocat/Hello-World", WebURL: "https://github.com/octocat/Hello-World", APIBase: "https://api.github.com"},
		},
		{
			name: "ssh clone URL",
			raw:  "git@gitlab.com:gitlab-org/gitlab-test.git",
			want: Repository{Provider: "gitlab", Host: "gitlab.com", Project: "gitlab-org/gitlab-test", WebURL: "https://gitlab.com/gitlab-org/gitlab-test", APIBase: "https://gitlab.com/api/v4"},
		},
		{
			name:     "private host override",
			raw:      "https://git.example.test/team/project.git",
			provider: "github",
			apiBase:  "http://api.example.test/v3/",
			want:     Repository{Provider: "github", Host: "git.example.test", Project: "team/project", WebURL: "https://git.example.test/team/project", APIBase: "http://api.example.test/v3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRepository(tt.raw, tt.provider, tt.apiBase)
			if err != nil {
				t.Fatalf("ParseRepository() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseRepository() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseRepositoryRejectsUnsafeOrAmbiguousInputs(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		provider string
		apiBase  string
	}{
		{name: "empty", raw: ""},
		{name: "missing project owner", raw: "github:octocat"},
		{name: "github nested project", raw: "github:org/team/repo"},
		{name: "shorthand provider conflict", raw: "github:octocat/repo", provider: "gitlab"},
		{name: "unsupported URL scheme", raw: "ftp://github.com/octocat/repo"},
		{name: "URL credentials", raw: "https://user:pass@github.com/octocat/repo"},
		{name: "URL query", raw: "https://github.com/octocat/repo?ref=main"},
		{name: "URL fragment", raw: "https://github.com/octocat/repo#main"},
		{name: "path traversal", raw: "gitlab:group/../repo"},
		{name: "private host without provider", raw: "https://git.example.test/group/repo"},
		{name: "invalid API base scheme", raw: "https://github.com/octocat/repo", apiBase: "ftp://api.example.test"},
		{name: "API base credentials", raw: "https://github.com/octocat/repo", apiBase: "https://user:pass@api.example.test"},
		{name: "API base query", raw: "https://github.com/octocat/repo", apiBase: "https://api.example.test?v=1"},
		{name: "unsupported provider", raw: "https://github.com/octocat/repo", provider: "bitbucket"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseRepository(tt.raw, tt.provider, tt.apiBase); err == nil {
				t.Fatal("ParseRepository() unexpectedly succeeded")
			}
		})
	}
}

func TestResolveAPIBase(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		host     string
		override string
		want     string
	}{
		{name: "github cloud", provider: "github", host: "github.com", want: "https://api.github.com"},
		{name: "github enterprise", provider: "github", host: "ghe.example.test", want: "https://ghe.example.test/api/v3"},
		{name: "gitlab self hosted", provider: "gitlab", host: "git.example.test", want: "https://git.example.test/api/v4"},
		{name: "override trims slash", provider: "gitlab", host: "git.example.test", override: "http://api.example.test/root///", want: "http://api.example.test/root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAPIBase(tt.provider, tt.host, tt.override)
			if err != nil {
				t.Fatalf("resolveAPIBase() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveAPIBase() = %q, want %q", got, tt.want)
			}
		})
	}
}
