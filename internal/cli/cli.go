package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/output"
	"github.com/SamuelSupe/git-rg/internal/provider"
	"github.com/SamuelSupe/git-rg/internal/search"
)

const usageText = `Usage: git-rg [FLAGS] PATTERN REPOSITORY

Search one remote GitHub or GitLab repository without cloning it.

Repository forms:
  github:OWNER/REPO
  gitlab:GROUP/PROJECT
  https://HOST/OWNER/REPO.git
  git@HOST:GROUP/PROJECT.git

Commands:
  refs [FLAGS] REPOSITORY  list branch heads and tags

Flags:`

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type optionalInt struct {
	value int
	set   bool
}

func (v *optionalInt) String() string { return fmt.Sprint(v.value) }
func (v *optionalInt) Set(raw string) error {
	value, err := strconv.Atoi(raw)
	if err == nil {
		v.value = value
		v.set = true
	}
	return err
}

func Run(args []string, stdout, stderr io.Writer) int {
	return RunVersion(args, stdout, stderr, "dev")
}

func RunVersion(args []string, stdout, stderr io.Writer, version string) int {
	if len(args) > 0 && args[0] == "refs" {
		return runRefs(args[1:], stdout, stderr)
	}
	return runSearch(args, stdout, stderr, version)
}

func runSearch(args []string, stdout, stderr io.Writer, version string) (exitCode int) {
	started := time.Now()
	providerErrorCode := func(fallback string, err error) string {
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
		return fallback
	}

	flags := flag.NewFlagSet("git-rg", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		fmt.Fprintln(stderr, usageText)
		flags.SetOutput(stderr)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}

	var fixed, ignoreCase, word, noCache, showVersion, commitInfo bool
	var globs stringList
	var before, after, around optionalInt
	var ref, modeValue, formatValue, providerValue, apiBase string
	var maxResults, maxRequests int
	var timeout time.Duration
	flags.BoolVar(&fixed, "F", false, "treat PATTERN as a fixed string")
	flags.BoolVar(&fixed, "fixed-strings", false, "treat PATTERN as a fixed string")
	flags.BoolVar(&ignoreCase, "i", false, "search case-insensitively")
	flags.BoolVar(&ignoreCase, "ignore-case", false, "search case-insensitively")
	flags.BoolVar(&word, "w", false, "require Unicode word boundaries")
	flags.BoolVar(&word, "word-regexp", false, "require Unicode word boundaries")
	flags.Var(&globs, "g", "include glob, or exclude with ! (repeatable)")
	flags.Var(&globs, "glob", "include glob, or exclude with ! (repeatable)")
	flags.Var(&before, "B", "show NUM lines before each match")
	flags.Var(&before, "before-context", "show NUM lines before each match")
	flags.Var(&after, "A", "show NUM lines after each match")
	flags.Var(&after, "after-context", "show NUM lines after each match")
	flags.Var(&around, "C", "show NUM lines before and after each match")
	flags.Var(&around, "context", "show NUM lines before and after each match")
	flags.StringVar(&ref, "ref", "", "branch, tag, or commit (default: repository default branch)")
	flags.BoolVar(&commitInfo, "commit-info", false, "include commit author, committer, dates, and message")
	flags.StringVar(&modeValue, "mode", "auto", "search mode: auto, exact, or indexed")
	flags.StringVar(&formatValue, "format", "ndjson", "output format: ndjson or text")
	flags.IntVar(&maxResults, "max-results", 200, "maximum matching lines; 0 means unlimited")
	flags.IntVar(&maxRequests, "max-requests", 100, "maximum remote HTTP requests including retries; 0 means unlimited")
	flags.StringVar(&providerValue, "provider", "", "github or gitlab (required for private hosts)")
	flags.StringVar(&apiBase, "api-base", "", "override the provider API base URL")
	flags.BoolVar(&noCache, "no-cache", false, "disable the on-disk immutable object cache")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "overall command timeout")
	flags.BoolVar(&showVersion, "version", false, "print version and exit")

	parseErr := flags.Parse(args)
	outputFormat := "ndjson"
	if formatValue == "text" {
		outputFormat = "text"
	}
	renderer := output.NewBuffered(outputFormat, stdout, stderr)
	defer func() {
		if err := renderer.Close(); err != nil && exitCode != 2 {
			fmt.Fprintf(stderr, "git-rg: write_output: %v\n", err)
			exitCode = 2
		}
	}()
	emitError := func(code string, err error) {
		_ = renderer.Emit(search.Event{Type: "error", Code: code, Message: err.Error()})
	}
	if parseErr != nil {
		if errors.Is(parseErr, flag.ErrHelp) {
			return 0
		}
		emitError("invalid_arguments", parseErr)
		return 2
	}
	if showVersion {
		fmt.Fprintf(stdout, "git-rg %s\n", version)
		return 0
	}
	if flags.NArg() != 2 {
		emitError("invalid_arguments", fmt.Errorf("expected PATTERN and REPOSITORY, got %d positional arguments", flags.NArg()))
		flags.Usage()
		return 2
	}
	if maxResults < 0 || maxRequests < 0 || timeout <= 0 {
		emitError("invalid_arguments", errors.New("--max-results and --max-requests must be non-negative and --timeout must be positive"))
		return 2
	}
	if formatValue != "ndjson" && formatValue != "text" {
		emitError("invalid_format", fmt.Errorf("unsupported --format %q", formatValue))
		return 2
	}
	mode := search.Mode(modeValue)
	if mode != search.ModeAuto && mode != search.ModeExact && mode != search.ModeIndexed {
		emitError("invalid_mode", fmt.Errorf("unsupported --mode %q", modeValue))
		return 2
	}

	beforeCount, afterCount := 0, 0
	if around.set {
		beforeCount, afterCount = around.value, around.value
	}
	if before.set {
		beforeCount = before.value
	}
	if after.set {
		afterCount = after.value
	}
	if beforeCount < 0 || afterCount < 0 {
		emitError("invalid_arguments", errors.New("context line counts must be non-negative"))
		return 2
	}

	matcher, err := search.NewMatcher(search.MatcherConfig{
		Pattern:    flags.Arg(0),
		Fixed:      fixed,
		IgnoreCase: ignoreCase,
		Word:       word,
		Before:     beforeCount,
		After:      afterCount,
	})
	if err != nil {
		emitError("invalid_pattern", err)
		return 2
	}
	if _, err := search.CompileGlobs(globs); err != nil {
		emitError("invalid_glob", err)
		return 2
	}
	if mode == search.ModeIndexed && matcher.LiteralPrefix() == "" {
		emitError("indexed_pattern_unsupported", errors.New("pattern has no literal prefix for portable indexed search"))
		return 2
	}
	repository, err := provider.ParseRepository(flags.Arg(1), providerValue, apiBase)
	if err != nil {
		emitError("invalid_repository", err)
		return 2
	}
	remote, err := provider.NewWithOptions(repository, provider.Options{Timeout: timeout, RequestLimit: maxRequests})
	if err != nil {
		emitError("provider_init_failed", err)
		return 2
	}

	baseContext, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	ctx, cancel := context.WithTimeout(baseContext, timeout)
	defer cancel()

	snapshot, err := remote.Resolve(ctx, repository, ref)
	if err != nil {
		emitError(providerErrorCode("resolve_repository", err), err)
		return 2
	}
	if err := renderer.Emit(search.NewMetaEvent(snapshot, mode, commitInfo)); err != nil {
		fmt.Fprintf(stderr, "git-rg: write_output: %v\n", err)
		return 2
	}

	objectCache, cacheErr := cache.New(noCache)
	if cacheErr != nil {
		objectCache = cache.Disabled()
		_ = renderer.Emit(search.Event{Type: "warning", Code: "cache_disabled", Message: cacheErr.Error()})
	}
	if pruneErr := objectCache.PruneIfNeededContext(ctx); pruneErr != nil {
		_ = renderer.Emit(search.Event{Type: "warning", Code: "cache_prune_failed", Message: pruneErr.Error()})
	}
	runner := &search.Runner{
		Provider: remote,
		Cache:    objectCache,
		Matcher:  matcher,
		Config: search.Config{
			Mode:         mode,
			MaxResults:   maxResults,
			Workers:      8,
			Globs:        globs,
			RequestLimit: maxRequests,
		},
	}
	summary, runErr := runner.Run(ctx, snapshot, renderer.Emit)
	if ctx.Err() == nil {
		if pruneErr := objectCache.PruneIfNeededContext(ctx); pruneErr != nil {
			_ = renderer.Emit(search.Event{Type: "warning", Code: "cache_prune_failed", Message: pruneErr.Error()})
		}
	}
	if runErr != nil {
		emitError(search.ErrorCode(runErr), errors.New(search.ErrorMessage(runErr)))
	}
	if err := renderer.Flush(); err != nil {
		fmt.Fprintf(stderr, "git-rg: write_output: %v\n", err)
		return 2
	}
	summary.DurationMS = time.Since(started).Milliseconds()
	if err := renderer.Emit(search.NewSummaryEvent(summary)); err != nil {
		fmt.Fprintf(stderr, "git-rg: write_output: %v\n", err)
		return 2
	}
	if runErr != nil {
		return 2
	}
	if summary.MatchedLines == 0 {
		return 1
	}
	return 0
}
