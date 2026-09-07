package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type credentialRunCall struct {
	executable string
	args       []string
	stdin      string
	workingDir string
	env        []string
}

type credentialRunResponse struct {
	output string
	err    error
}

type credentialRunner struct {
	mu        sync.Mutex
	calls     []credentialRunCall
	responses []credentialRunResponse
}

func (r *credentialRunner) run(ctx context.Context, executable string, args []string, stdin, workingDir string, env []string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, credentialRunCall{
		executable: executable,
		args:       append([]string(nil), args...),
		stdin:      stdin,
		workingDir: workingDir,
		env:        append([]string(nil), env...),
	})
	var response credentialRunResponse
	if len(r.responses) > 0 {
		response = r.responses[0]
		r.responses = r.responses[1:]
	}
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	return response.output, response.err
}

func credentialHelperLookup(dir string) func(string) (string, error) {
	return func(name string) (string, error) {
		return filepath.Join(dir, name), nil
	}
}

func clearCredentialEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"GITRG_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
		"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN", "GITLAB_CI", "GITLAB_USER_ID",
	} {
		t.Setenv(name, "")
	}
}

func credentialRepository(providerName, host, apiBase, project string) Repository {
	return Repository{
		Provider: providerName,
		Host:     host,
		APIBase:  apiBase,
		Project:  project,
		WebURL:   "https://" + host + "/" + project,
	}
}

func TestResolveCredentialsEnvironmentTokenSkipsHelper(t *testing.T) {
	clearCredentialEnvironment(t)
	t.Setenv("GITRG_TOKEN", "generic-token")
	t.Setenv("GITHUB_TOKEN", "github-token")
	t.Setenv("GH_TOKEN", "gh-token")
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	runner := &credentialRunner{}
	token, warning, err := resolveCredentials(context.Background(), repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v", err)
	}
	if token != "generic-token" || warning != "" {
		t.Fatalf("token/warning = %q/%q, want generic-token and empty warning", token, warning)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("helper call count = %d, want none when an environment token is set", len(runner.calls))
	}
}

func TestResolveCredentialsEnvModeNeverRunsHelper(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	runner := &credentialRunner{}
	token, warning, err := resolveCredentials(context.Background(), repo, "env", credentialHelperLookup(t.TempDir()), runner.run)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v", err)
	}
	if token != "" || warning != "" {
		t.Fatalf("token/warning = %q/%q, want empty/empty", token, warning)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("helper call count = %d, want none for --auth env", len(runner.calls))
	}
}

func TestResolveCredentialsGitHubCallsGhForMatchingHTTPSOrigin(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	runner := &credentialRunner{responses: []credentialRunResponse{{output: "gh-token\n"}}}
	token, warning, err := resolveCredentials(context.Background(), repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v", err)
	}
	if token != "gh-token" || warning != "" {
		t.Fatalf("token/warning = %q/%q, want gh-token/empty", token, warning)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("helper call count = %d, want one gh call", len(runner.calls))
	}
	call := runner.calls[0]
	if filepath.Base(call.executable) != "gh" || strings.Join(call.args, " ") != "auth token --hostname github.com" {
		t.Fatalf("gh helper executable/args = %q/%q, want gh/auth token --hostname github.com", filepath.Base(call.executable), strings.Join(call.args, " "))
	}
	assertCredentialEnvironmentScrubbed(t, call.env)
}

func TestResolveCredentialsGitLabStatusThenCredentialHelper(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("gitlab", "gitlab.example.test", "https://gitlab.example.test/api/v4", "group/subgroup/project")
	runner := &credentialRunner{responses: []credentialRunResponse{
		{output: ""},
		{output: "protocol=https\nhost=gitlab.example.test\nusername=oauth2\npassword=oauth-token\noauth_refresh_token=refresh-token\n\n"},
	}}
	token, warning, err := resolveCredentials(context.Background(), repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v", err)
	}
	if token != "oauth-token" || warning != "" {
		t.Fatalf("token/warning = %q/%q, want oauth-token/empty", token, warning)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("helper call count = %d, want status and credential calls", len(runner.calls))
	}
	status, helper := runner.calls[0], runner.calls[1]
	if filepath.Base(status.executable) != "glab" || strings.Join(status.args, " ") != "auth status --hostname gitlab.example.test" || status.stdin != "" {
		t.Fatalf("glab status executable/args/stdin = %q/%q/%q", filepath.Base(status.executable), strings.Join(status.args, " "), status.stdin)
	}
	if filepath.Base(helper.executable) != "glab" || strings.Join(helper.args, " ") != "auth git-credential get" {
		t.Fatalf("glab helper executable/args = %q/%q", filepath.Base(helper.executable), strings.Join(helper.args, " "))
	}
	for _, want := range []string{"protocol=https", "host=gitlab.example.test", "path=group/subgroup/project"} {
		if !strings.Contains(helper.stdin, want) {
			t.Errorf("glab helper stdin %q does not contain %q", helper.stdin, want)
		}
	}
	assertCredentialEnvironmentScrubbed(t, status.env)
	assertCredentialEnvironmentScrubbed(t, helper.env)
}

func TestResolveCredentialsRejectsGitLabJobToken(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("gitlab", "gitlab.com", "https://gitlab.com/api/v4", "group/project")
	runner := &credentialRunner{responses: []credentialRunResponse{
		{output: ""},
		{output: "protocol=https\nhost=gitlab.com\nusername=gitlab-ci-token\npassword=ci-secret\n\n"},
	}}
	token, warning, err := resolveCredentials(context.Background(), repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v", err)
	}
	if token != "" || warning == "" {
		t.Fatalf("token/warning = %q/%q, want empty token and an auth warning", token, warning)
	}
	if strings.Contains(warning, "ci-secret") {
		t.Fatalf("warning leaked job token: %q", warning)
	}
}

func TestResolveCredentialsMissingHelperIsSilent(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	lookup := func(string) (string, error) { return "", exec.ErrNotFound }
	token, warning, err := resolveCredentials(context.Background(), repo, "auto", lookup, func(context.Context, string, []string, string, string, []string) (string, error) {
		t.Fatal("credential runner called when helper is missing")
		return "", nil
	})
	if err != nil || token != "" || warning != "" {
		t.Fatalf("token/warning/error = %q/%q/%v, want empty token/warning and nil error", token, warning, err)
	}
}

func TestResolveCredentialsDoesNotLeakHelperErrors(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	runner := &credentialRunner{responses: []credentialRunResponse{{err: errors.New("helper failed: secret-token")}}}
	token, warning, err := resolveCredentials(context.Background(), repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v", err)
	}
	if token != "" || warning == "" {
		t.Fatalf("token/warning = %q/%q, want empty token and warning", token, warning)
	}
	if strings.Contains(warning, "secret-token") {
		t.Fatalf("warning leaked helper error: %q", warning)
	}
}

func TestResolveCredentialsRejectsMalformedHelperOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "multiple gh tokens", output: "first\nsecond\n"},
		{name: "control character", output: "token\twith-tab\n"},
		{name: "duplicate password", output: "protocol=https\nhost=gitlab.com\nusername=oauth2\npassword=one\npassword=two\n\n"},
		{name: "refresh token only", output: "protocol=https\nhost=gitlab.com\nusername=oauth2\nrefresh_token=refresh-secret\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearCredentialEnvironment(t)
			runner := &credentialRunner{}
			repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
			if strings.Contains(tt.name, "password") || strings.Contains(tt.name, "refresh") {
				repo = credentialRepository("gitlab", "gitlab.com", "https://gitlab.com/api/v4", "group/project")
				runner.responses = []credentialRunResponse{{output: ""}, {output: tt.output}}
			} else {
				runner.responses = []credentialRunResponse{{output: tt.output}}
			}
			token, warning, err := resolveCredentials(context.Background(), repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
			if err != nil {
				t.Fatalf("resolveCredentials() error = %v", err)
			}
			if token != "" || warning == "" {
				t.Fatalf("token/warning = %q/%q, want empty token and warning", token, warning)
			}
			if strings.Contains(warning, "refresh-secret") || strings.Contains(warning, "first") || strings.Contains(warning, "second") {
				t.Fatalf("warning leaked helper output: %q", warning)
			}
		})
	}
}

func TestResolveCredentialsSkipsUnsafeOrCrossOriginAPIs(t *testing.T) {
	tests := []struct {
		name string
		repo Repository
	}{
		{
			name: "insecure API",
			repo: credentialRepository("github", "github.com", "http://github.com/api/v3", "octocat/Hello-World"),
		},
		{
			name: "cross origin API",
			repo: credentialRepository("github", "github.com", "https://api.other.example/v3", "octocat/Hello-World"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearCredentialEnvironment(t)
			runner := &credentialRunner{}
			token, warning, err := resolveCredentials(context.Background(), tt.repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
			if err != nil {
				t.Fatalf("resolveCredentials() error = %v", err)
			}
			if token != "" || warning == "" {
				t.Fatalf("token/warning = %q/%q, want empty token and explanatory warning", token, warning)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("helper call count = %d, want none for unsafe/cross-origin API", len(runner.calls))
			}
		})
	}
}

func TestResolveCredentialsPreservesMatchingCustomOriginPath(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("github", "ghe.example.test", "https://ghe.example.test/api/v3", "octocat/Hello-World")
	runner := &credentialRunner{responses: []credentialRunResponse{{output: "enterprise-token\n"}}}
	token, warning, err := resolveCredentials(context.Background(), repo, "auto", credentialHelperLookup(t.TempDir()), runner.run)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v", err)
	}
	if token != "enterprise-token" || warning != "" || len(runner.calls) != 1 {
		t.Fatalf("token/warning/calls = %q/%q/%d, want enterprise-token/empty/1", token, warning, len(runner.calls))
	}
	if got := strings.Join(runner.calls[0].args, " "); got != "auth token --hostname ghe.example.test" {
		t.Fatalf("gh args = %q", got)
	}
}

func TestCredentialHostNormalizesHTTPSPortsAndRejectsMismatches(t *testing.T) {
	tests := []struct {
		name string
		repo Repository
		want string
		ok   bool
	}{
		{
			name: "default port omitted",
			repo: credentialRepository("github", "ghe.example.test", "https://ghe.example.test/api/v3", "octocat/Hello-World"),
			want: "ghe.example.test",
			ok:   true,
		},
		{
			name: "default port normalized",
			repo: Repository{
				Provider: "github", Host: "ghe.example.test:443", Project: "octocat/Hello-World",
				WebURL: "https://ghe.example.test:443/octocat/Hello-World", APIBase: "https://ghe.example.test:443/api/v3",
			},
			want: "ghe.example.test",
			ok:   true,
		},
		{
			name: "non-default port matches",
			repo: Repository{
				Provider: "gitlab", Host: "gitlab.example.test:8443", Project: "group/project",
				WebURL: "https://gitlab.example.test:8443/group/project", APIBase: "https://gitlab.example.test:8443/api/v4",
			},
			want: "gitlab.example.test:8443",
			ok:   true,
		},
		{
			name: "non-default port mismatch",
			repo: Repository{
				Provider: "gitlab", Host: "gitlab.example.test:8443", Project: "group/project",
				WebURL: "https://gitlab.example.test:8443/group/project", APIBase: "https://gitlab.example.test:9443/api/v4",
			},
			ok: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := credentialHost(tt.repo)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("credentialHost() = %q/%t, want %q/%t", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCredentialEnvironmentRemovesCredentialAndHostOverrides(t *testing.T) {
	dir := t.TempDir()
	inherited := []string{
		"PATH=/usr/bin",
		"HTTPS_PROXY=http://proxy.invalid:8080",
		"GITRG_TOKEN=generic-token",
		"GITHUB_TOKEN=github-token",
		"GH_TOKEN=gh-token",
		"GH_ENTERPRISE_TOKEN=enterprise-token",
		"GITHUB_ENTERPRISE_TOKEN=enterprise-token-2",
		"GH_HOST=evil.example",
		"GH_REPO=evil/repo",
		"GITLAB_TOKEN=gitlab-token",
		"GITLAB_ACCESS_TOKEN=gitlab-access-token",
		"GITLAB_HOST=evil.example",
		"GITLAB_URI=https://evil.example",
		"GL_HOST=evil.example",
		"GITLAB_API_HOST=evil.example",
		"GITLAB_SUBFOLDER=/evil",
		"OAUTH_TOKEN=oauth-token",
		"GLAB_IS_OAUTH2=true",
		"GLAB_USER=evil-user",
		"GITLAB_CLIENT_ID=client-id",
		"CI_JOB_TOKEN=job-token",
		"GIT_CONFIG_COUNT=1",
		"GIT_TRACE=1",
	}
	filtered := credentialEnvironment(inherited, dir)
	values := make(map[string]string, len(filtered))
	for _, entry := range filtered {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	for _, name := range []string{
		"GITRG_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
		"GH_HOST", "GH_REPO", "GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "GITLAB_HOST", "GITLAB_URI",
		"GL_HOST", "GITLAB_API_HOST", "GITLAB_SUBFOLDER", "OAUTH_TOKEN", "GLAB_IS_OAUTH2", "GLAB_USER",
		"GITLAB_CLIENT_ID", "CI_JOB_TOKEN", "GIT_CONFIG_COUNT", "GIT_TRACE",
	} {
		if value := values[name]; value != "" {
			t.Errorf("credential environment retained %s=%q", name, value)
		}
	}
	if values["PATH"] != "/usr/bin" || values["HTTPS_PROXY"] != "http://proxy.invalid:8080" {
		t.Fatalf("preserved environment = PATH %q/HTTPS_PROXY %q, want inherited values", values["PATH"], values["HTTPS_PROXY"])
	}
	if values["GIT_CEILING_DIRECTORIES"] != filepath.Dir(dir) {
		t.Fatalf("GIT_CEILING_DIRECTORIES = %q, want %q", values["GIT_CEILING_DIRECTORIES"], filepath.Dir(dir))
	}
}

func TestResolveCredentialsParentCancellationIsFatal(t *testing.T) {
	clearCredentialEnvironment(t)
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	started := make(chan struct{})
	run := func(ctx context.Context, executable string, args []string, stdin, workingDir string, env []string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		token, warning string
		err            error
	}
	results := make(chan result, 1)
	go func() {
		token, warning, err := resolveCredentials(ctx, repo, "auto", credentialHelperLookup(t.TempDir()), run)
		results <- result{token: token, warning: warning, err: err}
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("credential helper was not started")
	}
	select {
	case got := <-results:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", got.err)
		}
		if got.token != "" || got.warning != "" {
			t.Fatalf("token/warning = %q/%q, want empty values on fatal cancellation", got.token, got.warning)
		}
	case <-time.After(time.Second):
		t.Fatal("resolveCredentials did not return after parent cancellation")
	}
}

func TestResolveCredentialsUsesActualChildProcess(t *testing.T) {
	clearCredentialEnvironment(t)
	helperDir := installCredentialHelper(t, "gh")
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+oldPath)
	t.Setenv("GITRG_TEST_HELPER", "1")
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	token, warning, err := ResolveCredentials(context.Background(), repo, "auto")
	if err != nil {
		t.Fatalf("ResolveCredentials() error = %v", err)
	}
	if token != "subprocess-token" || warning != "" {
		t.Fatalf("token/warning = %q/%q, want subprocess-token/empty", token, warning)
	}
}

func TestResolveCredentialsHelperTimeoutWarnsAndContinues(t *testing.T) {
	clearCredentialEnvironment(t)
	t.Setenv("GITRG_TEST_HELPER", "1")
	t.Setenv("GITRG_TEST_HELPER_MODE", "block")
	helperDir := installCredentialHelper(t, "gh")
	repo := credentialRepository("github", "github.com", "https://api.github.com", "octocat/Hello-World")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	token, warning, err := resolveCredentials(ctx, repo, "auto", credentialHelperLookup(helperDir), runCredentialCommand)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("resolveCredentials() error = %v, want warning-only helper timeout", err)
	}
	if token != "" || warning == "" {
		t.Fatalf("token/warning = %q/%q, want empty token and warning", token, warning)
	}
	if ctx.Err() != nil {
		t.Fatalf("parent context error = %v, want parent context still valid", ctx.Err())
	}
	if elapsed < 9*time.Second || elapsed > 15*time.Second {
		t.Fatalf("helper timeout elapsed = %s, want approximately 10 seconds", elapsed)
	}
}

func TestRunCredentialCommandCapsStdoutAndStderr(t *testing.T) {
	helperDir := installCredentialHelper(t, "gh")
	helperPath := credentialHelperPath(helperDir, "gh")
	for _, mode := range []string{"oversize_stdout", "oversize_stderr"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			env := credentialChildEnvironment(dir, mode)
			output, err := runCredentialCommand(context.Background(), helperPath, []string{"auth", "token", "--hostname", "github.com"}, "", dir, env)
			if !errors.Is(err, errCredentialOutputLimit) {
				t.Fatalf("runCredentialCommand() error = %v, want output limit error", err)
			}
			if output != "" {
				t.Fatalf("runCredentialCommand() returned %d captured bytes after output limit", len(output))
			}
		})
	}
}

func TestRunCredentialCommandCancelsActualChildProcess(t *testing.T) {
	helperDir := installCredentialHelper(t, "gh")
	helperPath := credentialHelperPath(helperDir, "gh")
	dir := t.TempDir()
	env := credentialChildEnvironment(dir, "block")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := runCredentialCommand(ctx, helperPath, []string{"auth", "token", "--hostname", "github.com"}, "", dir, env)
		result <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runCredentialCommand() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runCredentialCommand() did not cancel the child process")
	}
}

func installCredentialHelper(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("absolute test binary path: %v", err)
	}
	target := credentialHelperPath(dir, name)
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatalf("copy helper: %v", err)
	}
	return dir
}

func credentialHelperPath(dir, name string) string {
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		name += ".exe"
	}
	return filepath.Join(dir, name)
}

func credentialChildEnvironment(dir, mode string) []string {
	env := credentialEnvironment(os.Environ(), dir)
	env = append(env, "GITRG_TEST_HELPER=1", "GITRG_TEST_HELPER_MODE="+mode)
	return env
}

func assertCredentialEnvironmentScrubbed(t *testing.T, env []string) {
	t.Helper()
	for _, item := range env {
		name, value, _ := strings.Cut(item, "=")
		switch name {
		case "GITRG_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "CI_JOB_TOKEN", "GITLAB_CI", "GITLAB_USER_ID":
			if value != "" {
				t.Errorf("credential helper environment retained %s", name)
			}
		}
	}
}

// initCredentialHelperProcess turns the Go test binary into a deterministic
// gh executable. An init hook is needed because the production command passes
// the real helper arguments and therefore cannot add -test.run to the child.
func init() {
	if os.Getenv("GITRG_TEST_HELPER") != "1" {
		return
	}
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base != "gh" {
		return
	}
	switch os.Getenv("GITRG_TEST_HELPER_MODE") {
	case "oversize_stdout":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", maxCredentialOutput+1))
		os.Exit(0)
	case "oversize_stderr":
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", maxCredentialOutput+1))
		os.Exit(0)
	case "block":
		for {
			time.Sleep(time.Second)
		}
	}
	if !containsCredentialArgs(os.Args, "auth", "token", "--hostname", "github.com") {
		fmt.Fprintln(os.Stderr, "unexpected gh arguments")
		os.Exit(41)
	}
	fmt.Fprintln(os.Stdout, "subprocess-token")
	os.Exit(0)
}

func containsCredentialArgs(args []string, wanted ...string) bool {
	for start := range args {
		if len(args)-start < len(wanted) {
			continue
		}
		match := true
		for offset, value := range wanted {
			if args[start+offset] != value {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
