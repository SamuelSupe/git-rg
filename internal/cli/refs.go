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
	"sort"
	"strings"
	"syscall"
	"time"

	"git-rg/internal/output"
	"git-rg/internal/provider"
)

const refsUsageText = `Usage: git-rg refs [FLAGS] REPOSITORY

List remote branch heads and tags without cloning the repository.

Flags:`

const maxRefAssociationPairs int64 = 1_000_000
const maxRefAssociationBytes int64 = 128 << 20

type refsEvent struct {
	Type         string           `json:"type"`
	Schema       int              `json:"schema_version,omitempty"`
	Provider     string           `json:"provider,omitempty"`
	Repository   string           `json:"repository,omitempty"`
	Kind         provider.RefKind `json:"kind,omitempty"`
	Name         string           `json:"name,omitempty"`
	Commit       string           `json:"commit,omitempty"`
	HeadTags     []string         `json:"head_tags,omitempty"`
	HeadBranches []string         `json:"head_branches,omitempty"`
	Code         string           `json:"code,omitempty"`
	Message      string           `json:"message,omitempty"`
	Summary      *refsSummary     `json:"summary,omitempty"`
}

type refsSummary struct {
	Branches     int                 `json:"branches"`
	Tags         int                 `json:"tags"`
	APIRequests  int                 `json:"api_requests"`
	APIRetries   int                 `json:"api_retries"`
	RequestLimit int                 `json:"request_limit"`
	RateLimit    *provider.RateLimit `json:"rate_limit,omitempty"`
	Complete     bool                `json:"complete"`
}

type refsRenderer struct {
	format string
	out    io.Writer
	err    io.Writer
	json   *json.Encoder
}

func newRefsRenderer(format string, stdout, stderr io.Writer) *refsRenderer {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return &refsRenderer{format: format, out: stdout, err: stderr, json: encoder}
}

func (r *refsRenderer) emit(event refsEvent) error {
	if r.format == "ndjson" {
		if err := r.json.Encode(event); err != nil {
			return err
		}
		if event.Type == "error" {
			_, _ = fmt.Fprintf(r.err, "git-rg: %s: %s\n", event.Code, output.EscapeDiagnostic(event.Message))
		}
		return nil
	}
	if event.Type == "error" {
		_, err := fmt.Fprintf(r.err, "git-rg: %s: %s\n", event.Code, output.EscapeDiagnostic(event.Message))
		return err
	}
	if event.Type != "ref" {
		return nil
	}
	related := event.HeadTags
	if event.Kind == provider.RefKindTag {
		related = event.HeadBranches
	}
	if len(related) == 0 {
		_, err := fmt.Fprintf(r.out, "%s\t%s\t%s\n", event.Kind, event.Name, event.Commit)
		return err
	}
	_, err := fmt.Fprintf(r.out, "%s\t%s\t%s\t%s\n", event.Kind, event.Name, event.Commit, strings.Join(related, "\t"))
	return err
}

func runRefs(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("git-rg refs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		fmt.Fprintln(stderr, refsUsageText)
		flags.SetOutput(stderr)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	var kindValue, formatValue, providerValue, apiBase string
	var maxRequests int
	var timeout time.Duration
	flags.StringVar(&kindValue, "kind", "all", "ref kind: all, branch, or tag")
	flags.StringVar(&formatValue, "format", "ndjson", "output format: ndjson or text")
	flags.IntVar(&maxRequests, "max-requests", 100, "maximum remote HTTP requests including retries; 0 means unlimited")
	flags.StringVar(&providerValue, "provider", "", "github or gitlab (required for private hosts)")
	flags.StringVar(&apiBase, "api-base", "", "override the provider API base URL")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "overall command timeout")

	parseErr := flags.Parse(args)
	outputFormat := "ndjson"
	if formatValue == "text" {
		outputFormat = "text"
	}
	renderer := newRefsRenderer(outputFormat, stdout, stderr)
	emitError := func(code string, err error) {
		_ = renderer.emit(refsEvent{Type: "error", Code: code, Message: err.Error()})
	}
	if parseErr != nil {
		if errors.Is(parseErr, flag.ErrHelp) {
			return 0
		}
		emitError("invalid_arguments", parseErr)
		return 2
	}
	if flags.NArg() != 1 {
		emitError("invalid_arguments", fmt.Errorf("expected REPOSITORY, got %d positional arguments", flags.NArg()))
		flags.Usage()
		return 2
	}
	if formatValue != "ndjson" && formatValue != "text" {
		emitError("invalid_format", fmt.Errorf("unsupported --format %q", formatValue))
		return 2
	}
	if maxRequests < 0 || timeout <= 0 {
		emitError("invalid_arguments", errors.New("--max-requests must be non-negative and --timeout must be positive"))
		return 2
	}
	kind := provider.RefKind(kindValue)
	if kind != provider.RefKindAll && kind != provider.RefKindBranch && kind != provider.RefKindTag {
		emitError("invalid_ref_kind", fmt.Errorf("unsupported --kind %q", kindValue))
		return 2
	}
	repository, err := provider.ParseRepository(flags.Arg(0), providerValue, apiBase)
	if err != nil {
		emitError("invalid_repository", err)
		return 2
	}
	remote, err := provider.NewWithOptions(repository, provider.Options{Timeout: timeout, RequestLimit: maxRequests})
	if err != nil {
		emitError("provider_init_failed", err)
		return 2
	}
	lister, ok := remote.(provider.RefLister)
	if !ok {
		emitError("list_refs_unsupported", fmt.Errorf("provider %q does not support listing refs", repository.Provider))
		return 2
	}

	baseContext, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	ctx, cancel := context.WithTimeout(baseContext, timeout)
	defer cancel()
	refs, err := lister.ListRefs(ctx, repository, kind)
	if err != nil {
		emitError(refsProviderErrorCode(err), err)
		return 2
	}
	branchesByCommit, tagsByCommit, err := refNamesByCommitContext(ctx, refs)
	if err != nil {
		emitError(refsProviderErrorCode(err), err)
		return 2
	}
	if err := ctx.Err(); err != nil {
		emitError(refsProviderErrorCode(err), err)
		return 2
	}
	if err := renderer.emit(refsEvent{Type: "meta", Schema: 1, Provider: repository.Provider, Repository: repository.WebURL}); err != nil {
		fmt.Fprintf(stderr, "git-rg: write_output: %v\n", err)
		return 2
	}
	branches, tags := 0, 0
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			emitError(refsProviderErrorCode(err), err)
			return 2
		}
		event := refsEvent{Type: "ref", Kind: ref.Kind, Name: ref.Name, Commit: ref.Commit}
		if ref.Kind == provider.RefKindBranch {
			branches++
			event.HeadTags = tagsByCommit[ref.Commit]
		} else {
			tags++
			event.HeadBranches = branchesByCommit[ref.Commit]
		}
		if err := renderer.emit(event); err != nil {
			fmt.Fprintf(stderr, "git-rg: write_output: %v\n", err)
			return 2
		}
	}
	if err := ctx.Err(); err != nil {
		emitError(refsProviderErrorCode(err), err)
		return 2
	}
	stats := remote.RequestStats()
	summary := &refsSummary{
		Branches: branches, Tags: tags, APIRequests: stats.Requests, APIRetries: stats.Retries,
		RequestLimit: stats.RequestLimit, RateLimit: stats.RateLimit, Complete: true,
	}
	if err := renderer.emit(refsEvent{Type: "summary", Summary: summary}); err != nil {
		fmt.Fprintf(stderr, "git-rg: write_output: %v\n", err)
		return 2
	}
	return 0
}

func refNamesByCommitContext(ctx context.Context, refs []provider.Ref) (map[string][]string, map[string][]string, error) {
	branches := make(map[string][]string)
	tags := make(map[string][]string)
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if ref.Kind == provider.RefKindBranch {
			branches[ref.Commit] = append(branches[ref.Commit], ref.Name)
		} else {
			tags[ref.Commit] = append(tags[ref.Commit], ref.Name)
		}
	}
	for _, names := range branches {
		sort.Strings(names)
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	for _, names := range tags {
		sort.Strings(names)
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	var associationPairs, associationBytes int64
	for commit, branchNames := range branches {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		tagNames := tags[commit]
		if len(tagNames) == 0 {
			continue
		}
		pairs := int64(len(branchNames)) * int64(len(tagNames))
		associationPairs += pairs
		if associationPairs > maxRefAssociationPairs {
			return nil, nil, &provider.ResourceLimitError{Resource: "branch/tag association pairs", Limit: maxRefAssociationPairs}
		}
		associationBytes += int64(len(branchNames))*stringBytes(tagNames) + int64(len(tagNames))*stringBytes(branchNames)
		if associationBytes > maxRefAssociationBytes {
			return nil, nil, &provider.ResourceLimitError{Resource: "branch/tag association output bytes", Limit: maxRefAssociationBytes}
		}
	}
	return branches, tags, nil
}

func stringBytes(values []string) int64 {
	var total int64
	for _, value := range values {
		total += int64(len(value))
	}
	return total
}

func refsProviderErrorCode(err error) string {
	var budgetErr *provider.RequestBudgetError
	if errors.As(err, &budgetErr) {
		return "request_budget_exceeded"
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) && httpErr.RateLimited {
		return "rate_limited"
	}
	var resourceErr *provider.ResourceLimitError
	if errors.As(err, &resourceErr) {
		return "resource_limit"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "list_refs_cancelled"
	}
	return "list_refs_failed"
}
