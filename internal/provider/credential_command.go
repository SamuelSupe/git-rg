package provider

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxCredentialOutput = 64 << 10

var errCredentialOutputLimit = errors.New("credential helper output limit exceeded")

func runCredentialCommand(ctx context.Context, executable string, args []string, stdin, dir string, env []string) (string, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &credentialOutput{remaining: maxCredentialOutput, cancel: cancel}
	cmd := exec.CommandContext(commandCtx, executable, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = credentialStream{output: output, capture: true}
	cmd.Stderr = credentialStream{output: output}
	// Bound inherited pipes even if a helper leaves a descendant running.
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	if output.exceeded {
		return "", errCredentialOutputLimit
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return output.stdout.String(), nil
}

type credentialOutput struct {
	mu        sync.Mutex
	stdout    bytes.Buffer
	remaining int
	exceeded  bool
	cancel    context.CancelFunc
}

type credentialStream struct {
	output  *credentialOutput
	capture bool
}

func (s credentialStream) Write(p []byte) (int, error) {
	s.output.mu.Lock()
	defer s.output.mu.Unlock()
	if len(p) > s.output.remaining {
		s.output.exceeded = true
		s.output.cancel()
		return 0, errCredentialOutputLimit
	}
	s.output.remaining -= len(p)
	if s.capture {
		return s.output.stdout.Write(p)
	}
	return len(p), nil
}

func credentialEnvironment(inherited []string, dir string) []string {
	env := make([]string, 0, len(inherited)+12)
	for _, entry := range inherited {
		key, value, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_CONFIG_") || strings.HasPrefix(upper, "GIT_TRACE") {
			continue
		}
		switch upper {
		case "GITRG_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
			"GH_HOST", "GH_REPO", "GH_DEBUG", "GH_PROMPT_DISABLED", "GH_NO_UPDATE_NOTIFIER", "GH_NO_EXTENSION_UPDATE_NOTIFIER",
			"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN", "GITLAB_HOST", "GITLAB_URI", "GL_HOST",
			"GITLAB_API_HOST", "GITLAB_SUBFOLDER", "GITLAB_SSH_HOST", "GITLAB_CLIENT_ID", "GLAB_USER", "GLAB_IS_OAUTH2",
			"GLAB_ENABLE_CI_AUTOLOGIN", "GLAB_CHECK_UPDATE", "GLAB_NO_PROMPT", "NO_PROMPT", "PROMPT_DISABLED",
			"GLAB_DEBUG", "GLAB_DEBUG_HTTP", "GLAB_SHOW_WHATS_NEW", "GLAB_NOTIFY_SKILL_UPDATES", "GLAB_SEND_TELEMETRY",
			"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
			"GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS":
			continue
		case "GH_CONFIG_DIR", "GLAB_CONFIG_DIR", "XDG_CONFIG_HOME":
			if value != "" && !filepath.IsAbs(value) {
				if absolute, err := filepath.Abs(value); err == nil {
					entry = key + "=" + absolute
				}
			}
		}
		env = append(env, entry)
	}
	return append(env,
		"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1",
		"GLAB_ENABLE_CI_AUTOLOGIN=false", "GLAB_CHECK_UPDATE=false", "GLAB_NO_PROMPT=true", "NO_PROMPT=true", "PROMPT_DISABLED=true",
		"GLAB_SHOW_WHATS_NEW=false", "GLAB_NOTIFY_SKILL_UPDATES=false", "GLAB_SEND_TELEMETRY=false",
		"GIT_TERMINAL_PROMPT=0", "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
}
