package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/SamuelSupe/git-rg/internal/agent"
	"github.com/SamuelSupe/git-rg/internal/output"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

const maxIssueBodyBytes = 64 << 10

type issueResult struct {
	provider.Issue
	Complete bool `json:"complete"`
}

type issueEvent struct {
	Type       string          `json:"type"`
	Schema     int             `json:"schema_version"`
	Code       string          `json:"code,omitempty"`
	Message    string          `json:"message,omitempty"`
	Repository string          `json:"repository,omitempty"`
	Title      string          `json:"title,omitempty"`
	Body       string          `json:"body,omitempty"`
	Agent      *agent.Identity `json:"agent,omitempty"`
	Result     *issueResult    `json:"result,omitempty"`
}

func runIssueCommand(args []string, stdout, stderr io.Writer) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	emit := func(event issueEvent) error {
		event.Schema = 1
		return encoder.Encode(event)
	}
	fail := func(code string, err error) int {
		if emitErr := emit(issueEvent{Type: "error", Code: code, Message: err.Error()}); emitErr != nil {
			fmt.Fprintf(stderr, "git-rg: write_output: %s\n", output.EscapeDiagnostic(emitErr.Error()))
		}
		fmt.Fprintf(stderr, "git-rg: %s: %s\n", code, output.EscapeDiagnostic(err.Error()))
		return 2
	}
	if len(args) > 0 && args[0] == "create" {
		args = args[1:]
	} else if len(args) != 1 || (args[0] != "--help" && args[0] != "-h") {
		return fail("invalid_arguments", errors.New("use issue create [FLAGS] REPOSITORY; see issue --help"))
	}
	flags := flag.NewFlagSet("git-rg issue create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		fmt.Fprintln(stderr, `Usage: git-rg issue create --title TITLE [--body TEXT | --body-file FILE] [--dry-run | --enable-write] [FLAGS] REPOSITORY

Create a GitHub or GitLab issue. Output is NDJSON; exit codes are 0 (success) and 2 (error).
Put flags before REPOSITORY. Use --body-file - to read Markdown from stdin.

Writes are off by default. Creation requires --enable-write and GITRG_WRITE_TOKEN.
GitHub fine-grained tokens need Issues: write; GitLab tokens need API access and permission to create issues.
Read tokens and gh/glab logins are never used for creation.
Dry-run needs no token and makes no remote requests, even with --enable-write.
Agent identity is self-reported; the platform author remains the authenticated account.
Creation is not retried or deduplicated. If confirmation is lost, inspect remote issues before retrying.

Examples:
  git-rg issue create --dry-run --title "Unexpected result" --body "Steps to reproduce..." github:OWNER/REPO
  git-rg issue create --enable-write --title "Unexpected result" --body-file issue.md --agent-name "Review Agent" --agent-run-id run-123 gitlab:GROUP/PROJECT

Flags:`)
		flags.SetOutput(stderr)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	var title, body, bodyFile, providerName, apiBase string
	var enableWrite, dryRun bool
	var timeout time.Duration
	var maxRequests int
	var identity agent.Identity
	flags.StringVar(&title, "title", "", "issue title (required; up to 200 UTF-8 bytes)")
	flags.StringVar(&body, "body", "", "Markdown description (up to 64 KiB including agent identity)")
	flags.StringVar(&bodyFile, "body-file", "", "read UTF-8 Markdown from a file; - reads stdin")
	flags.BoolVar(&enableWrite, "enable-write", false, "explicitly enable issue creation for this invocation")
	flags.BoolVar(&dryRun, "dry-run", false, "preview locally without creating an issue, even with --enable-write")
	flags.StringVar(&providerName, "provider", "", "github or gitlab (required for private hosts)")
	flags.StringVar(&apiBase, "api-base", "", "override the provider API base URL")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "overall timeout, including body-file and stdin reading")
	flags.IntVar(&maxRequests, "max-requests", 100, "maximum remote HTTP requests; 0 means unlimited")
	flags.StringVar(&identity.Name, "agent-name", "", "self-reported agent name included in the issue description")
	flags.StringVar(&identity.Model, "agent-model", "", "optional agent model; requires an agent name")
	flags.StringVar(&identity.RunID, "agent-run-id", "", "optional agent run ID; requires an agent name")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail("invalid_arguments", err)
	}
	if flags.NArg() != 1 || timeout <= 0 || maxRequests < 0 {
		return fail("invalid_arguments", errors.New("check repository, --timeout and --max-requests; see issue --help"))
	}
	if !dryRun && !enableWrite {
		return fail("write_disabled", provider.ErrWriteDisabled)
	}
	var bodySet, bodyFileSet bool
	var attribution *agent.Identity
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "body":
			bodySet = true
		case "body-file":
			bodyFileSet = true
		case "agent-name", "agent-model", "agent-run-id":
			attribution = &identity
		}
	})
	if bodySet && bodyFileSet || bodyFileSet && bodyFile == "" {
		return fail("invalid_arguments", errors.New("use either --body or --body-file with a non-empty file path"))
	}
	repo, err := provider.ParseRepository(flags.Arg(0), providerName, apiBase)
	if err != nil {
		return fail("invalid_repository", err)
	}
	if !utf8.ValidString(title) || strings.TrimSpace(title) == "" || len(title) > 200 || strings.ContainsFunc(title, unicode.IsControl) {
		return fail("invalid_issue", errors.New("title must contain 1 to 200 UTF-8 bytes without control characters"))
	}
	if err := attribution.Validate(); err != nil {
		return fail("invalid_issue", err)
	}
	baseContext, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	ctx, cancel := context.WithTimeout(baseContext, timeout)
	defer cancel()
	if bodyFileSet {
		data, err := readCommandInput(ctx, bodyFile, func(r io.Reader) ([]byte, error) {
			return io.ReadAll(io.LimitReader(r, maxIssueBodyBytes+1))
		})
		if err != nil {
			if ctx.Err() != nil {
				return fail("cancelled", ctx.Err())
			}
			return fail("invalid_issue", err)
		}
		body = string(data)
	}
	if !utf8.ValidString(body) || strings.ContainsRune(body, '\x00') {
		return fail("invalid_issue", errors.New("body must be UTF-8 text without NUL bytes"))
	}
	body = attribution.AppendToBody(body)
	if len(body) > maxIssueBodyBytes {
		return fail("invalid_issue", errors.New("body including agent identity is limited to 64 KiB"))
	}
	if dryRun {
		if err := emit(issueEvent{Type: "preview", Repository: repo.WebURL, Title: title, Body: body, Agent: attribution}); err != nil {
			return fail("write_output", err)
		}
		return 0
	}
	token, err := provider.WriteCredentials()
	if err != nil {
		return fail("provider_init_failed", err)
	}
	remote, err := provider.NewWithOptions(repo, provider.Options{Token: token, EnableWrite: true, Timeout: timeout, RequestLimit: maxRequests})
	if err != nil {
		return fail("provider_init_failed", err)
	}
	creator, ok := remote.(provider.IssueCreator)
	if !ok {
		return fail("unsupported_provider", errors.New("provider does not support issue creation"))
	}
	issue, err := creator.CreateIssue(ctx, repo, title, body)
	if err != nil {
		var httpErr *provider.HTTPError
		if remote.RequestStats().Requests > 0 && !(errors.As(err, &httpErr) && httpErr.Status >= 400 && httpErr.Status < 500 && httpErr.Status != http.StatusRequestTimeout) {
			return fail("issue_creation_uncertain", fmt.Errorf("issue creation could not be confirmed; check the repository's issues before retrying: %w", err))
		}
		return fail("issue_create_failed", err)
	}
	result := &issueResult{Issue: issue, Complete: true}
	if err := emit(issueEvent{Type: "issue", Repository: repo.WebURL, Agent: attribution, Result: result}); err != nil {
		return fail("write_output", fmt.Errorf("issue was created at %s but its result could not be written: %w", issue.URL, err))
	}
	return 0
}
