package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

const minAutoIndexRequestHeadroom = 39
const archiveRequestReserve = 6
const blobRequestAttemptBudget = 3

func (r *Runner) Run(ctx context.Context, snapshot provider.Snapshot, emit Emitter) (Summary, error) {
	started := time.Now()
	summary := Summary{}
	matchedFiles := make(map[string]struct{})
	if r.Config.Mode == ModeIndexed {
		summary.Reason = "indexed_mode"
	}
	finish := func(err error) (Summary, error) {
		if err == nil && !summary.Truncated && ctx.Err() != nil {
			err = contextError(ctx)
		}
		if err != nil {
			summary.Complete = false
			if summary.Reason == "" || summary.Reason == "indexed_mode" {
				summary.Reason = incompleteReason(err, ErrorCode(err))
			}
		} else {
			summary.Complete = r.Config.Mode != ModeIndexed && !summary.Truncated
		}
		stats := r.Provider.RequestStats()
		summary.APIRequests = stats.Requests
		summary.APIRetries = stats.Retries
		summary.RequestLimit = stats.RequestLimit
		summary.RateLimit = stats.RateLimit
		summary.MatchedFiles = len(matchedFiles)
		summary.DurationMS = time.Since(started).Milliseconds()
		return summary, err
	}
	if ctx.Err() != nil {
		return finish(contextError(ctx))
	}
	globs, err := CompileGlobs(r.Config.Globs)
	if err != nil {
		return finish(&Error{Code: "invalid_glob", Err: err})
	}
	if r.Config.Mode == ModeIndexed {
		literal := r.Matcher.LiteralPrefix()
		if literal == "" {
			summary.Reason = "unsupported_pattern"
			return finish(&Error{Code: "indexed_pattern_unsupported", Err: errors.New("pattern has no literal prefix for portable indexed search")})
		}
		candidates, err := r.Provider.SearchCandidates(ctx, snapshot, literal)
		if err != nil {
			summary.Reason = incompleteReason(err, "index_error")
			return finish(&Error{Code: "indexed_search_failed", Err: err})
		}
		entries, err := indexedCandidates(candidates, globs)
		if err != nil {
			summary.Reason = incompleteReason(err, "index_error")
			return finish(&Error{Code: "indexed_search_failed", Err: err})
		}
		summary.Transport = "blob"
		_, err = r.scanEntries(ctx, snapshot, entries, &summary, matchedFiles, emit)
		return finish(err)
	}

	// An archive may omit export-ignore paths or rewrite export-subst/LFS content.
	// The immutable tree defines the search scope independently of archive settings.
	filtered, totalFiles, warnings, err := r.loadTree(ctx, snapshot, globs, &summary)
	if emitErr := emitEvents(warnings, emit); emitErr != nil {
		return finish(emitErr)
	}
	if err != nil {
		summary.Reason = incompleteReason(err, "tree_error")
		return finish(&Error{Code: "list_tree", Err: err})
	}
	if len(filtered) == 0 {
		return finish(nil)
	}
	preferBlobs := r.preferBlobScan(filtered, totalFiles)
	if !preferBlobs && len(r.Config.Globs) > 0 && len(filtered) < totalFiles && r.Cache.Enabled() {
		var stopped bool
		filtered, stopped, err = r.scanCachedEntries(ctx, snapshot, filtered, &summary, matchedFiles, emit)
		if stopped || err != nil || len(filtered) == 0 {
			return finish(err)
		}
		preferBlobs = r.preferBlobScan(filtered, totalFiles)
	}
	skip := make(map[string]struct{})
	if preferBlobs {
		summary.Transport = "blob"
		stopped, err := r.scanCandidateEntries(ctx, snapshot, filtered, skip, &summary, matchedFiles, emit, "blob_prefetch_failed")
		if err != nil || stopped || len(skip) == len(filtered) {
			return finish(err)
		}
	}

	key := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
	cached, _, cacheErr := r.Cache.OpenContext(ctx, key)
	if cacheErr != nil {
		if IsContextError(cacheErr) {
			return finish(cacheErr)
		}
		if err := emit(Event{Type: "warning", Code: "cache_read_failed", Message: cacheErr.Error()}); err != nil {
			return finish(&Error{Code: "write_output", Err: err})
		}
	}
	if cached != nil {
		defer cached.Close()
	}
	literal := r.Matcher.LiteralPrefix()
	if !preferBlobs && r.Config.Mode == ModeAuto && cached == nil && literal != "" && r.canUseAutoIndex() {
		candidates, searchErr := r.Provider.SearchCandidates(ctx, snapshot, literal)
		if searchErr != nil {
			if err := emit(Event{Type: "warning", Code: "index_unavailable", Message: searchErr.Error()}); err != nil {
				return finish(&Error{Code: "write_output", Err: err})
			}
		} else {
			prioritized := orderCandidates(filtered, candidates)
			prefetch := min(len(prioritized), 16)
			if r.Config.RequestLimit > 0 {
				available := r.Config.RequestLimit - r.Provider.RequestStats().Requests - archiveRequestReserve
				prefetch = min(prefetch, max(available/blobRequestAttemptBudget, 0))
			}
			if prefetch < len(prioritized) {
				if err := emit(Event{Type: "warning", Code: "candidate_prefetch_limited", Message: fmt.Sprintf("prefetching %d of %d available indexed candidates before the archive scan", prefetch, len(prioritized))}); err != nil {
					return finish(&Error{Code: "write_output", Err: err})
				}
			}
			if prefetch > 0 {
				summary.Transport = "blob"
			}
			stopped, err := r.scanCandidateEntries(ctx, snapshot, prioritized[:prefetch], skip, &summary, matchedFiles, emit, "candidate_prefetch_failed")
			if err != nil || stopped {
				return finish(err)
			}
		}
	}
	_, err = r.scanArchiveWithFallback(ctx, snapshot, filtered, skip, cached, &summary, matchedFiles, emit)
	return finish(err)
}

func (r *Runner) canUseAutoIndex() bool {
	// Keep enough request budget for candidate pagination/retries and the archive.
	stats := r.Provider.RequestStats()
	return stats.RequestLimit == 0 || stats.RequestLimit-stats.Requests >= minAutoIndexRequestHeadroom
}

func (r *Runner) preferBlobScan(entries []provider.Entry, totalFiles int) bool {
	// Cached selections have already been consumed; these limits bound only
	// potential downloads. Full scans remain on the archive path.
	if len(r.Config.Globs) == 0 || len(entries) >= totalFiles || len(entries) > maxSearchWorkers {
		return false
	}
	if len(entries) > 1 && len(entries)*4 > totalFiles {
		return false
	}
	var size int64
	for _, entry := range entries {
		// GitLab trees do not report blob sizes. Limit that case to one request.
		if entry.Size == 0 && len(entries) > 1 {
			return false
		}
		size += entry.Size
		if size > 8<<20 {
			return false
		}
	}
	stats := r.Provider.RequestStats()
	limit := stats.RequestLimit
	if limit == 0 {
		limit = r.Config.RequestLimit
	}
	return limit == 0 || limit-stats.Requests >= len(entries)*blobRequestAttemptBudget
}

func emitEvents(events []Event, emit Emitter) error {
	for _, event := range events {
		if err := emit(event); err != nil {
			return &Error{Code: "write_output", Err: err}
		}
	}
	return nil
}

func filterEntries(entries []provider.Entry, globs *GlobSet) []provider.Entry {
	filtered := entries[:0]
	for _, entry := range entries {
		if !globs.Match(entry.Path) {
			continue
		}
		filtered = append(filtered, entry)
	}
	clear(entries[len(filtered):])
	return filtered
}

func (r *Runner) scanCandidateEntries(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, skip map[string]struct{}, summary *Summary, matchedFiles map[string]struct{}, emit Emitter, failureCode string) (bool, error) {
	return r.scanEntriesOrdered(ctx, snapshot, entries, r.scanEntry, func(outcome fileOutcome) (bool, error) {
		entry := entries[outcome.index]
		if outcome.err != nil {
			summary.DownloadedBytes += outcome.downloadedBytes
			if err := emitEvents(outcome.warnings, emit); err != nil {
				return true, err
			}
			if IsContextError(outcome.err) {
				summary.Complete = false
				summary.Reason = "cancelled"
				return true, contextError(ctx)
			}
			if err := emit(Event{Type: "warning", Code: failureCode, Message: fmt.Sprintf("could not prefetch %q: %v; archive scan will retry the path", entry.Path, outcome.err)}); err != nil {
				return true, &Error{Code: "write_output", Err: err}
			}
			return false, nil
		}
		stopped, err := r.consumeOutcome(ctx, outcome, summary, matchedFiles, emit, "read_blob", "blob_error")
		if err != nil || stopped {
			return true, err
		}
		skip[entry.Path] = struct{}{}
		return false, nil
	})
}

func (r *Runner) scanArchiveWithFallback(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, skip map[string]struct{}, cached io.ReadCloser, summary *Summary, matchedFiles map[string]struct{}, emit Emitter) (bool, error) {
	remaining := make(map[string]provider.Entry, len(entries))
	for _, entry := range entries {
		if _, scanned := skip[entry.Path]; !scanned {
			remaining[entry.Path] = entry
		}
	}
	if len(remaining) == 0 {
		return false, nil
	}
	summary.Transport = "archive"
	stopped, archiveErr := r.scanArchive(ctx, snapshot, remaining, cached, summary, matchedFiles, emit)
	var unavailable *archiveUnavailableError
	if errors.As(archiveErr, &unavailable) {
		var budgetErr *provider.RequestBudgetError
		var httpErr *provider.HTTPError
		if errors.As(unavailable, &budgetErr) || (errors.As(unavailable, &httpErr) && httpErr.RateLimited) || IsContextError(unavailable) {
			return true, &Error{Code: "read_archive", Err: unavailable}
		}
		if err := emit(Event{Type: "warning", Code: "archive_unavailable", Message: unavailable.Error() + "; falling back to blob requests"}); err != nil {
			return true, &Error{Code: "write_output", Err: err}
		}
		summary.Transport = "blob_fallback"
	} else if archiveErr != nil || stopped {
		return stopped, archiveErr
	}
	missing := make([]provider.Entry, 0, len(remaining))
	for _, entry := range entries {
		if _, absent := remaining[entry.Path]; absent {
			missing = append(missing, entry)
		}
	}
	return r.scanEntries(ctx, snapshot, missing, summary, matchedFiles, emit)
}
