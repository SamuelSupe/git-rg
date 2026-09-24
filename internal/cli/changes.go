package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/SamuelSupe/git-rg/internal/output"
	"github.com/SamuelSupe/git-rg/internal/proposal"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

type changeEvent struct {
	Type       string                  `json:"type"`
	Schema     int                     `json:"schema_version"`
	Code       string                  `json:"code,omitempty"`
	Message    string                  `json:"message,omitempty"`
	Repository string                  `json:"repository,omitempty"`
	Commit     string                  `json:"commit,omitempty"`
	Branch     string                  `json:"branch,omitempty"`
	Path       string                  `json:"path,omitempty"`
	Blob       string                  `json:"blob,omitempty"`
	Mode       string                  `json:"mode,omitempty"`
	Content    *string                 `json:"content,omitempty"`
	Diff       string                  `json:"diff,omitempty"`
	Agent      *proposal.AgentIdentity `json:"agent,omitempty"`
	Result     *proposal.Result        `json:"result,omitempty"`
}

func runChangeCommand(command string, args []string, stdout, stderr io.Writer) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	emit := func(event changeEvent) error {
		event.Schema = 1
		return encoder.Encode(event)
	}
	fail := func(code string, err error) int {
		if emitErr := emit(changeEvent{Type: "error", Code: code, Message: err.Error()}); emitErr != nil {
			fmt.Fprintf(stderr, "git-rg: write_output: %s\n", output.EscapeDiagnostic(emitErr.Error()))
		}
		fmt.Fprintf(stderr, "git-rg: %s: %s\n", code, output.EscapeDiagnostic(err.Error()))
		return 2
	}
	flags := flag.NewFlagSet("git-rg "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		if command == "read" {
			fmt.Fprintln(stderr, "Usage: git-rg read [FLAGS] REPOSITORY PATH\n\nRead a UTF-8 text file without cloning. Output is NDJSON.\n\nFlags:")
		} else {
			fmt.Fprintln(stderr, "Usage: git-rg propose --changes FILE [--dry-run | --enable-write] [FLAGS] REPOSITORY\n\nCreate a draft PR/MR from a JSON change plan without cloning.\nWrites are disabled by default. Publishing requires GITRG_WRITE_TOKEN.\nOutput is NDJSON. Rerun an identical plan to recover partial publication.\n\nFlags:")
		}
		flags.SetOutput(stderr)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	var providerName, apiBase, authMode, ref, changesFile string
	var enableWrite, dryRun bool
	var timeout time.Duration
	var maxRequests int
	var agent proposal.AgentIdentity
	flags.StringVar(&providerName, "provider", "", "github or gitlab (required for private hosts)")
	flags.StringVar(&apiBase, "api-base", "", "override the provider API base URL")
	flags.StringVar(&authMode, "auth", "auto", "read credential source: auto or env; writes require GITRG_WRITE_TOKEN")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "overall command timeout")
	flags.IntVar(&maxRequests, "max-requests", 100, "maximum remote HTTP requests including retries; 0 means unlimited")
	if command == "read" {
		flags.StringVar(&ref, "ref", "", "branch, tag, or commit (default: repository default branch)")
	} else {
		flags.StringVar(&changesFile, "changes", "", "JSON change plan file; - reads stdin")
		flags.BoolVar(&enableWrite, "enable-write", false, "explicitly enable remote commits and draft PR/MR creation for this invocation")
		flags.BoolVar(&dryRun, "dry-run", false, "validate files and emit a diff using read-only requests, even with --enable-write")
		flags.StringVar(&agent.Name, "agent-name", "", "self-reported agent name included in the PR/MR description")
		flags.StringVar(&agent.Model, "agent-model", "", "optional agent model; requires an agent name")
		flags.StringVar(&agent.RunID, "agent-run-id", "", "optional agent run ID; requires an agent name")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail("invalid_arguments", err)
	}
	positions := 1
	if command == "read" {
		positions = 2
	}
	if flags.NArg() != positions || maxRequests < 0 || timeout <= 0 || (authMode != "auto" && authMode != "env") {
		return fail("invalid_arguments", errors.New("check positional arguments, --auth, --timeout and --max-requests; see --help"))
	}
	write := command == "propose" && !dryRun
	if write && !enableWrite {
		return fail("write_disabled", provider.ErrWriteDisabled)
	}
	baseContext, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	ctx, cancel := context.WithTimeout(baseContext, timeout)
	defer cancel()
	var plan proposal.Plan
	if command == "propose" {
		if changesFile == "" {
			return fail("invalid_arguments", errors.New("--changes is required"))
		}
		var err error
		plan, err = readChangePlan(ctx, changesFile)
		if err != nil {
			if ctx.Err() != nil {
				return fail("cancelled", ctx.Err())
			}
			return fail("invalid_changes", err)
		}
		flags.Visit(func(f *flag.Flag) {
			if !strings.HasPrefix(f.Name, "agent-") {
				return
			}
			if plan.Agent == nil {
				plan.Agent = &proposal.AgentIdentity{}
			}
			switch f.Name {
			case "agent-name":
				plan.Agent.Name = agent.Name
			case "agent-model":
				plan.Agent.Model = agent.Model
			case "agent-run-id":
				plan.Agent.RunID = agent.RunID
			}
		})
		if err := plan.Validate(); err != nil {
			return fail("invalid_changes", err)
		}
	} else if err := proposal.ValidatePath(flags.Arg(1)); err != nil {
		return fail("invalid_arguments", err)
	}
	repo, err := provider.ParseRepository(flags.Arg(0), providerName, apiBase)
	if err != nil {
		return fail("invalid_repository", err)
	}
	var token, warning string
	if write {
		token, err = provider.WriteCredentials()
	} else {
		token, warning, err = provider.ResolveCredentials(ctx, repo, authMode)
	}
	if err != nil {
		return fail("provider_init_failed", err)
	}
	if warning != "" {
		if err := emit(changeEvent{Type: "warning", Code: "auth_unavailable", Message: warning}); err != nil {
			return fail("write_output", err)
		}
	}
	remote, err := provider.NewWithOptions(repo, provider.Options{Token: token, EnableWrite: write, Timeout: timeout, RequestLimit: maxRequests})
	if err != nil {
		return fail("provider_init_failed", err)
	}
	writer, ok := remote.(provider.ChangeProvider)
	if !ok {
		return fail("unsupported_provider", errors.New("provider does not support reading and proposing changes"))
	}
	if command == "read" {
		snapshot, err := remote.Resolve(ctx, repo, ref)
		if err != nil {
			return fail("read_failed", err)
		}
		entry, content, err := proposal.ReadFile(ctx, writer, snapshot, flags.Arg(1))
		if err != nil {
			return fail("read_failed", err)
		}
		if err := emit(changeEvent{Type: "file", Repository: repo.WebURL, Commit: snapshot.Commit, Path: entry.Path, Blob: entry.OID, Mode: entry.Mode, Content: &content}); err != nil {
			return fail("write_output", err)
		}
		return 0
	}
	prepared, err := proposal.Prepare(ctx, writer, repo, plan)
	if err != nil {
		return fail(changeErrorCode(err), err)
	}
	if err := emit(changeEvent{Type: "preview", Repository: repo.WebURL, Commit: plan.BaseCommit, Branch: plan.Branch, Diff: prepared.Diff, Agent: plan.Agent}); err != nil {
		return fail("write_output", err)
	}
	if dryRun {
		return 0
	}
	result, publishErr := prepared.Publish(ctx, writer)
	if err := emit(changeEvent{Type: "proposal", Repository: repo.WebURL, Result: &result}); err != nil {
		return fail("write_output", err)
	}
	if publishErr != nil {
		return fail(changeErrorCode(publishErr), publishErr)
	}
	return 0
}

func changeErrorCode(err error) string {
	switch {
	case errors.Is(err, provider.ErrWriteDisabled):
		return "write_disabled"
	case errors.Is(err, proposal.ErrBlobConflict):
		return "blob_conflict"
	case errors.Is(err, proposal.ErrBaseChanged):
		return "base_changed"
	case errors.Is(err, proposal.ErrBranchConflict):
		return "branch_conflict"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	}
	var budget *provider.RequestBudgetError
	if errors.As(err, &budget) {
		return "request_budget_exceeded"
	}
	return "proposal_failed"
}
