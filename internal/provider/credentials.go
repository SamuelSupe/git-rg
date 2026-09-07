package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const credentialTimeout = 10 * time.Second

// ResolveCredentials preserves environment overrides and optionally reuses the
// target host's CLI login. Helper failures return a warning and no credential;
// cancellation of the caller's context remains fatal.
func ResolveCredentials(ctx context.Context, repository Repository, mode string) (token, warning string, err error) {
	return resolveCredentials(ctx, repository, mode, exec.LookPath, runCredentialCommand)
}

func resolveCredentials(ctx context.Context, repository Repository, mode string,
	lookup func(string) (string, error),
	run func(context.Context, string, []string, string, string, []string) (string, error),
) (token, warning string, err error) {
	if mode != "auto" && mode != "env" {
		return "", "", errors.New("--auth must be auto or env")
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if token := tokenFor(repository); token != "" || mode == "env" {
		return token, "", nil
	}
	helperCtx, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	helper := "gh"
	if repository.Provider == "gitlab" {
		helper = "glab"
	} else if repository.Provider != "github" {
		return "", "", errors.New("unsupported credential provider")
	}
	host, ok := credentialHost(repository)
	if !ok {
		return "", "automatic credentials require HTTPS and a matching repository/API origin; continuing anonymously; use GITRG_TOKEN explicitly or --auth env", nil
	}
	failed := func(reason string) (string, string, error) {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		return "", fmt.Sprintf("%s credentials for %s unavailable (%s); continuing anonymously; run %s auth login --hostname %s, or use --auth env", helper, host, reason, helper, host), nil
	}
	executable, err := lookup(helper)
	if errors.Is(err, exec.ErrNotFound) {
		return "", "", nil
	}
	if err != nil {
		return failed("cannot locate helper")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return failed("cannot locate helper")
	}
	dir, err := os.MkdirTemp("", "git-rg-auth-")
	if err != nil {
		return failed("cannot prepare helper working directory")
	}
	defer os.RemoveAll(dir)
	env := credentialEnvironment(os.Environ(), dir)
	invoke := func(args []string, stdin string) (string, error) {
		return run(helperCtx, executable, args, stdin, dir, env)
	}
	var raw string
	if helper == "gh" {
		raw, err = invoke([]string{"auth", "token", "--hostname", host}, "")
		if err == nil {
			token = strings.TrimSuffix(raw, "\n")
			if token != raw {
				token = strings.TrimSuffix(token, "\r")
			}
		}
	} else {
		// The helper alone can fall back to global configuration for unknown
		// hosts. Require a registered, working host before requesting its token.
		_, err = invoke([]string{"auth", "status", "--hostname", host}, "")
		if err == nil {
			stdin := "protocol=https\nhost=" + host + "\npath=" + (&url.URL{Path: repository.Project + ".git"}).EscapedPath() + "\n\n"
			raw, err = invoke([]string{"auth", "git-credential", "get"}, stdin)
			if err == nil {
				token, err = parseGitLabCredential(raw, host)
			}
		}
	}
	if helperCtx.Err() != nil {
		return failed("helper timed out or was canceled")
	}
	if err != nil {
		if errors.Is(err, errCredentialOutputLimit) {
			return failed("helper output exceeds 64 KiB")
		}
		return failed("helper failed or returned an unsupported credential")
	}
	if !validCredentialToken(token) {
		return failed("helper returned an empty or invalid token")
	}
	return token, "", nil
}

func credentialHost(repository Repository) (string, bool) {
	web, err := url.Parse(repository.WebURL)
	if err != nil || web.Scheme != "https" || web.Hostname() == "" || web.User != nil {
		return "", false
	}
	base, err := url.Parse(repository.APIBase)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil {
		return "", false
	}
	expectedBase, err := resolveAPIBase(repository.Provider, repository.Host, "")
	if err != nil {
		return "", false
	}
	expected, err := url.Parse(expectedBase)
	if err != nil || credentialAuthority(base) != credentialAuthority(expected) {
		return "", false
	}
	declared, err := url.Parse("https://" + repository.Host)
	if err != nil || credentialAuthority(declared) != credentialAuthority(web) {
		return "", false
	}
	return credentialAuthority(web), true
}

func credentialAuthority(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	return host
}

func validCredentialToken(token string) bool {
	return token != "" && utf8.ValidString(token) && !strings.ContainsFunc(token, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	})
}

func parseGitLabCredential(raw, host string) (string, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", errors.New("invalid credential output")
		}
		switch key {
		case "password", "username", "protocol", "host":
			if old, exists := fields[key]; exists && old != value {
				return "", errors.New("conflicting credential fields")
			}
			fields[key] = value
		}
	}
	if fields["username"] == "" || fields["username"] == "gitlab-ci-token" || !validCredentialToken(fields["password"]) {
		return "", errors.New("unsupported credential")
	}
	if protocol, ok := fields["protocol"]; ok && protocol != "https" {
		return "", errors.New("credential protocol mismatch")
	}
	if returnedHost, ok := fields["host"]; ok && returnedHost != host {
		return "", errors.New("credential host mismatch")
	}
	return fields["password"], nil
}
