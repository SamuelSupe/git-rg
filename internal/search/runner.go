package search

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"git-rg/internal/cache"
	"git-rg/internal/provider"
)

const binaryProbeSize = 8 << 10
const maxTreeManifestSize = 64 << 20
const maxSearchWorkers = 8
const minAutoIndexRequestHeadroom = 39
const archiveRequestReserve = 6
const blobRequestAttemptBudget = 3
const maxScannedFileBytes int64 = 512 << 20
const maxResultSpoolBytes int64 = 32 << 20

var errBinaryContent = errors.New("binary content contains NUL")
var errResultSpool = errors.New("buffer search result")
var errFileTooLarge = errors.New("text file exceeds scan byte limit")
var errResultSpoolLimit = errors.New("file results exceed spool byte limit")

type indexedEntry struct {
	index int
	entry provider.Entry
}

type fileOutcome struct {
	index           int
	result          FileResult
	warnings        []Event
	err             error
	cacheHit        bool
	downloadedBytes int64
	skippedBinary   bool
	spoolPath       string
}

func (r *Runner) Run(ctx context.Context, snapshot provider.Snapshot, emit Emitter) (Summary, error) {
	started := time.Now()
	summary := Summary{Complete: r.Config.Mode != ModeIndexed}
	if r.Config.Mode == ModeIndexed {
		summary.Reason = "indexed_mode"
	}
	finish := func(err error) (Summary, error) {
		stats := r.Provider.RequestStats()
		summary.APIRequests = stats.Requests
		summary.APIRetries = stats.Retries
		summary.RequestLimit = stats.RequestLimit
		summary.RateLimit = stats.RateLimit
		summary.DurationMS = time.Since(started).Milliseconds()
		return summary, err
	}

	globs, err := CompileGlobs(r.Config.Globs)
	if err != nil {
		summary.Complete = false
		summary.Reason = "invalid_glob"
		return finish(&Error{Code: "invalid_glob", Err: err})
	}
	matchedFiles := make(map[string]struct{})
	if r.Config.Mode == ModeExact {
		stopped, runErr := r.scanArchiveWithFallback(ctx, snapshot, globs, nil, &summary, matchedFiles, emit)
		summary.MatchedFiles = len(matchedFiles)
		if runErr != nil || stopped {
			return finish(runErr)
		}
	} else if r.Config.Mode == ModeIndexed {
		literal := r.Matcher.LiteralPrefix()
		if literal == "" {
			summary.Complete = false
			summary.Reason = "unsupported_pattern"
			return finish(&Error{Code: "indexed_pattern_unsupported", Err: errors.New("pattern has no literal prefix for portable indexed search")})
		}
		candidates, searchErr := r.Provider.SearchCandidates(ctx, snapshot, literal)
		if searchErr != nil {
			summary.Complete = false
			summary.Reason = incompleteReason(searchErr, "index_error")
			return finish(&Error{Code: "indexed_search_failed", Err: searchErr})
		}
		ordered, candidateErr := indexedCandidates(candidates, globs)
		if candidateErr != nil {
			summary.Complete = false
			summary.Reason = "index_error"
			return finish(&Error{Code: "indexed_search_failed", Err: candidateErr})
		}
		summary.Transport = "blob"
		_, err = r.scanEntries(ctx, snapshot, ordered, &summary, matchedFiles, emit)
		if err != nil {
			summary.MatchedFiles = len(matchedFiles)
			return finish(err)
		}
	} else {
		skip := make(map[string]struct{})
		literal := r.Matcher.LiteralPrefix()
		if literal != "" && r.canUseAutoIndex() {
			candidates, searchErr := r.Provider.SearchCandidates(ctx, snapshot, literal)
			if searchErr != nil {
				if emitErr := emit(Event{Type: "warning", Code: "index_unavailable", Message: searchErr.Error()}); emitErr != nil {
					return finish(&Error{Code: "write_output", Err: emitErr})
				}
			} else if len(candidates) > 0 {
				entries, warnings, treeComplete, treeErr := r.loadTree(ctx, snapshot, false, &summary)
				if emitErr := emitEvents(warnings, emit); emitErr != nil {
					return finish(emitErr)
				}
				if treeErr != nil {
					if IsContextError(treeErr) {
						summary.Complete = false
						summary.Reason = "cancelled"
						return finish(contextError(ctx))
					}
					if emitErr := emit(Event{Type: "warning", Code: "candidate_manifest_unavailable", Message: treeErr.Error()}); emitErr != nil {
						return finish(&Error{Code: "write_output", Err: emitErr})
					}
				} else {
					if !treeComplete {
						if emitErr := emit(Event{Type: "warning", Code: "candidate_manifest_partial", Message: "tree manifest was limited to one response; archive scan still covers the full commit"}); emitErr != nil {
							return finish(&Error{Code: "write_output", Err: emitErr})
						}
					}
					filtered, byPath := filterEntries(entries, globs, nil)
					prioritized, _ := orderCandidates(filtered, byPath, candidates, true)
					prefetch := min(len(prioritized), 16)
					if r.Config.RequestLimit > 0 {
						available := r.Config.RequestLimit - r.Provider.RequestStats().Requests - archiveRequestReserve
						prefetch = min(prefetch, max(available/blobRequestAttemptBudget, 0))
					}
					if prefetch < len(prioritized) {
						if emitErr := emit(Event{Type: "warning", Code: "candidate_prefetch_limited", Message: fmt.Sprintf("prefetching %d of %d available indexed candidates before the archive scan", prefetch, len(prioritized))}); emitErr != nil {
							return finish(&Error{Code: "write_output", Err: emitErr})
						}
					}
					stopped, prefetchErr := r.scanCandidateEntries(ctx, snapshot, prioritized[:prefetch], skip, &summary, matchedFiles, emit)
					if prefetchErr != nil || stopped {
						summary.MatchedFiles = len(matchedFiles)
						return finish(prefetchErr)
					}
				}
			}
		}
		stopped, runErr := r.scanArchiveWithFallback(ctx, snapshot, globs, skip, &summary, matchedFiles, emit)
		summary.MatchedFiles = len(matchedFiles)
		if runErr != nil || stopped {
			return finish(runErr)
		}
	}
	summary.MatchedFiles = len(matchedFiles)
	if ctx.Err() != nil && !summary.Truncated {
		summary.Complete = false
		summary.Reason = "cancelled"
		return finish(contextError(ctx))
	}
	return finish(nil)
}

func (r *Runner) canUseAutoIndex() bool {
	// Candidate search may use ten pages with retries, followed by one tree
	// request. Preserve three attempts for both the tree and archive, including
	// the archive redirect used by GitHub.
	stats := r.Provider.RequestStats()
	return stats.RequestLimit == 0 || stats.RequestLimit-stats.Requests >= minAutoIndexRequestHeadroom
}

func emitEvents(events []Event, emit Emitter) error {
	for _, event := range events {
		if err := emit(event); err != nil {
			return &Error{Code: "write_output", Err: err}
		}
	}
	return nil
}

func filterEntries(entries []provider.Entry, globs *GlobSet, skip map[string]struct{}) ([]provider.Entry, map[string]provider.Entry) {
	filtered := make([]provider.Entry, 0, len(entries))
	byPath := make(map[string]provider.Entry, len(entries))
	for _, entry := range entries {
		if !globs.Match(entry.Path) {
			continue
		}
		if _, skipped := skip[entry.Path]; skipped {
			continue
		}
		filtered = append(filtered, entry)
		byPath[entry.Path] = entry
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Path < filtered[j].Path })
	return filtered, byPath
}

func (r *Runner) scanCandidateEntries(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, skip map[string]struct{}, summary *Summary, matchedFiles map[string]struct{}, emit Emitter) (bool, error) {
	return r.scanEntriesOrdered(ctx, snapshot, entries, func(outcome fileOutcome) (bool, error) {
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
			if err := emit(Event{Type: "warning", Code: "candidate_prefetch_failed", Message: fmt.Sprintf("could not prefetch %q: %v; archive scan will retry the path", entry.Path, outcome.err)}); err != nil {
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

func (r *Runner) scanArchiveWithFallback(ctx context.Context, snapshot provider.Snapshot, globs *GlobSet, skip map[string]struct{}, summary *Summary, matchedFiles map[string]struct{}, emit Emitter) (bool, error) {
	summary.Transport = "archive"
	stopped, archiveErr := r.scanArchive(ctx, snapshot, globs, skip, summary, matchedFiles, emit)
	var unavailable *archiveUnavailableError
	if !errors.As(archiveErr, &unavailable) {
		return stopped, archiveErr
	}
	var budgetErr *provider.RequestBudgetError
	var httpErr *provider.HTTPError
	if errors.As(unavailable, &budgetErr) || (errors.As(unavailable, &httpErr) && httpErr.RateLimited) || IsContextError(unavailable) {
		summary.Complete = false
		summary.Reason = incompleteReason(unavailable, "archive_error")
		return true, &Error{Code: "read_archive", Err: unavailable}
	}
	if err := emit(Event{Type: "warning", Code: "archive_unavailable", Message: unavailable.Error() + "; falling back to blob requests"}); err != nil {
		return true, &Error{Code: "write_output", Err: err}
	}
	entries, warnings, _, treeErr := r.loadTree(ctx, snapshot, true, summary)
	if err := emitEvents(warnings, emit); err != nil {
		return true, err
	}
	if treeErr != nil {
		summary.Complete = false
		summary.Reason = incompleteReason(treeErr, "tree_error")
		return true, &Error{Code: "list_tree", Err: treeErr}
	}
	filtered, _ := filterEntries(entries, globs, skip)
	summary.Transport = "blob_fallback"
	return r.scanEntries(ctx, snapshot, filtered, summary, matchedFiles, emit)
}

func (r *Runner) scanEntries(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, summary *Summary, matchedFiles map[string]struct{}, emit Emitter) (bool, error) {
	stopped, err := r.scanEntriesOrdered(ctx, snapshot, entries, func(outcome fileOutcome) (bool, error) {
		return r.consumeOutcome(ctx, outcome, summary, matchedFiles, emit, "read_blob", "blob_error")
	})
	if err != nil {
		if IsContextError(err) && !summary.Truncated {
			summary.Complete = false
			summary.Reason = "cancelled"
		}
		return true, err
	}
	if ctx.Err() != nil && !summary.Truncated {
		summary.Complete = false
		summary.Reason = "cancelled"
		return true, contextError(ctx)
	}
	return stopped, nil
}

func (r *Runner) scanEntriesOrdered(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, consume func(fileOutcome) (bool, error)) (bool, error) {
	if len(entries) == 0 {
		return false, nil
	}
	workers := r.Config.Workers
	if workers <= 0 {
		workers = maxSearchWorkers
	}
	workers = min(workers, maxSearchWorkers, len(entries))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan indexedEntry, workers)
	results := make(chan fileOutcome, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for job := range jobs {
				outcome := r.scanEntry(runCtx, snapshot, job.entry)
				outcome.index = job.index
				select {
				case results <- outcome:
				case <-runCtx.Done():
					cleanupOutcome(outcome)
					return
				}
			}
		}()
	}
	go func() {
		group.Wait()
		close(results)
	}()

	nextToDispatch := 0
	jobsClosed := false
	// The window advances only after the next ordered result is consumed, so a
	// slow early file cannot make pending grow beyond the worker count.
	dispatch := func() {
		jobs <- indexedEntry{index: nextToDispatch, entry: entries[nextToDispatch]}
		nextToDispatch++
		if nextToDispatch == len(entries) {
			close(jobs)
			jobsClosed = true
		}
	}
	for range workers {
		dispatch()
	}

	pending := make(map[int]fileOutcome, workers)
	next := 0
	stopped := false
	var fatal error
	for outcome := range results {
		if stopped {
			cleanupOutcome(outcome)
			continue
		}
		pending[outcome.index] = outcome
		for {
			current, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			stop, err := consume(current)
			if err != nil || stop {
				fatal = err
				stopped = true
				cancel()
				if !jobsClosed {
					close(jobs)
					jobsClosed = true
				}
				break
			}
			if nextToDispatch < len(entries) {
				dispatch()
			}
		}
	}
	for _, outcome := range pending {
		cleanupOutcome(outcome)
	}
	if fatal != nil {
		return true, fatal
	}
	if ctx.Err() != nil && !stopped {
		return true, contextError(ctx)
	}
	return stopped, nil
}

func (r *Runner) consumeOutcome(ctx context.Context, current fileOutcome, summary *Summary, matchedFiles map[string]struct{}, emit Emitter, errorCode, reason string) (bool, error) {
	defer cleanupOutcome(current)
	checkContext := func() (bool, error) {
		if err := ctx.Err(); err != nil {
			summary.Complete = false
			summary.Reason = "cancelled"
			return true, contextError(ctx)
		}
		return false, nil
	}
	if stopped, err := checkContext(); stopped {
		return stopped, err
	}
	summary.DownloadedBytes += current.downloadedBytes
	for _, warning := range current.warnings {
		if stopped, err := checkContext(); stopped {
			return stopped, err
		}
		if err := emit(warning); err != nil {
			return true, &Error{Code: "write_output", Err: err}
		}
	}
	if current.err != nil {
		summary.Complete = false
		summary.Reason = incompleteReason(current.err, reason)
		return true, &Error{Code: errorCode, Err: current.err}
	}
	summary.ScannedFiles++
	if current.cacheHit {
		summary.CacheHits++
	}
	if current.skippedBinary {
		summary.SkippedBinary++
	}
	consumeEvent := func(event Event) (bool, error) {
		if stopped, err := checkContext(); stopped {
			return stopped, err
		}
		if event.Type == "match" {
			summary.MatchedLines++
			matchedFiles[event.Path] = struct{}{}
		}
		if err := emit(event); err != nil {
			return true, &Error{Code: "write_output", Err: err}
		}
		if stopped, err := checkContext(); stopped {
			return stopped, err
		}
		if r.Config.MaxResults > 0 && summary.MatchedLines >= r.Config.MaxResults {
			summary.Complete = false
			summary.Truncated = true
			summary.Reason = "result_limit"
			return true, nil
		}
		return false, nil
	}
	for _, event := range current.result.Events {
		if stopped, err := consumeEvent(event); err != nil || stopped {
			return stopped, err
		}
	}
	if current.spoolPath != "" {
		spool, err := os.Open(current.spoolPath)
		if err != nil {
			return true, &Error{Code: "read_result_spool", Err: err}
		}
		decoder := json.NewDecoder(spool)
		for {
			if stopped, err := checkContext(); stopped {
				spool.Close()
				return stopped, err
			}
			var event Event
			if err := decoder.Decode(&event); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				spool.Close()
				return true, &Error{Code: "read_result_spool", Err: err}
			}
			if stopped, err := consumeEvent(event); err != nil || stopped {
				spool.Close()
				return stopped, err
			}
		}
		if err := spool.Close(); err != nil {
			return true, &Error{Code: "read_result_spool", Err: err}
		}
	}
	return false, nil
}

func cleanupOutcome(outcome fileOutcome) {
	if outcome.spoolPath != "" {
		_ = os.Remove(outcome.spoolPath)
	}
}

func (r *Runner) loadTree(ctx context.Context, snapshot provider.Snapshot, requireComplete bool, summary *Summary) ([]provider.Entry, []Event, bool, error) {
	key := cache.Key(snapshot.Repository.CacheNamespace(), "tree", snapshot.Commit)
	warnings := make([]Event, 0, 1)
	if reader, hit, err := r.Cache.OpenContext(ctx, key); err == nil && hit {
		entries, decodeErr := decodeTreeManifestContext(ctx, reader)
		reader.Close()
		if decodeErr == nil {
			summary.CacheHits++
			return entries, warnings, true, nil
		}
		if IsContextError(decodeErr) {
			return nil, warnings, false, decodeErr
		}
		warnings = append(warnings, Event{Type: "warning", Code: "cache_read_failed", Message: "cached tree manifest is invalid; refreshing it"})
		if removeErr := r.Cache.Remove(key); removeErr != nil {
			warnings = append(warnings, Event{Type: "warning", Code: "cache_remove_failed", Message: removeErr.Error()})
		}
	} else if err != nil {
		if IsContextError(err) {
			return nil, warnings, false, err
		}
		warnings = append(warnings, Event{Type: "warning", Code: "cache_read_failed", Message: err.Error()})
	}

	entries, complete, err := r.Provider.ListTree(ctx, snapshot, requireComplete)
	if err != nil {
		return nil, warnings, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, warnings, false, err
	}
	if requireComplete && !complete {
		return nil, warnings, false, errors.New("provider returned an incomplete tree manifest when a complete manifest was required")
	}
	if err := normalizeTreeEntries(entries); err != nil {
		return nil, warnings, false, fmt.Errorf("provider returned an invalid tree manifest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, warnings, false, err
	}
	if complete && r.Cache.Enabled() && treeManifestFitsCache(entries) {
		encoded, encodeErr := json.Marshal(entries)
		if encodeErr == nil && ctx.Err() == nil {
			stored, _, putErr := r.Cache.PutContext(ctx, key, bytes.NewReader(encoded))
			if stored != nil {
				stored.Close()
			}
			if putErr != nil && !IsContextError(putErr) {
				warnings = append(warnings, Event{Type: "warning", Code: "cache_write_failed", Message: putErr.Error()})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, warnings, false, err
	}
	return entries, warnings, complete, nil
}

func treeManifestFitsCache(entries []provider.Entry) bool {
	const encodedEntryOverhead int64 = 128
	remaining := int64(maxTreeManifestSize - 2)
	for _, entry := range entries {
		encodedUpperBound := encodedEntryOverhead + 6*int64(len(entry.Path)+len(entry.OID)+len(entry.Mode))
		if encodedUpperBound > remaining {
			return false
		}
		remaining -= encodedUpperBound
	}
	return true
}

func decodeTreeManifest(reader io.Reader) ([]provider.Entry, error) {
	return decodeTreeManifestContext(context.Background(), reader)
}

func decodeTreeManifestContext(ctx context.Context, reader io.Reader) ([]provider.Entry, error) {
	limited := &io.LimitedReader{R: reader, N: maxTreeManifestSize + 1}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: limited})
	var entries []provider.Entry
	if err := decoder.Decode(&entries); err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, errors.New("tree manifest must be a JSON array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("tree manifest contains trailing JSON")
		}
		return nil, err
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("tree manifest exceeds %d bytes", maxTreeManifestSize)
	}
	if err := normalizeTreeEntries(entries); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func normalizeTreeEntries(entries []provider.Entry) error {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if len(entry.Path) > provider.MaxRepositoryPathBytes {
			return fmt.Errorf("entry path: %w (%d bytes)", errRepositoryPathLimit, provider.MaxRepositoryPathBytes)
		}
		if entry.Path == "" || entry.Path == "." || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") || path.IsAbs(entry.Path) || path.Clean(entry.Path) != entry.Path || strings.ContainsRune(entry.Path, '\x00') {
			return fmt.Errorf("invalid path %q", entry.Path)
		}
		if entry.OID == "" {
			return fmt.Errorf("entry %q has no object ID", entry.Path)
		}
		if entry.Mode != "100644" && entry.Mode != "100755" {
			return fmt.Errorf("entry %q has unsupported mode %q", entry.Path, entry.Mode)
		}
		if entry.Size < 0 {
			return fmt.Errorf("entry %q has a negative size", entry.Path)
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return fmt.Errorf("duplicate path %q", entry.Path)
		}
		seen[entry.Path] = struct{}{}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return nil
}

func orderCandidates(all []provider.Entry, byPath map[string]provider.Entry, paths []string, indexedOnly bool) ([]provider.Entry, int) {
	seen := make(map[string]struct{}, len(paths))
	prioritized := make([]provider.Entry, 0, len(all))
	for _, candidatePath := range paths {
		entry, ok := byPath[candidatePath]
		if !ok {
			continue
		}
		if _, duplicate := seen[candidatePath]; duplicate {
			continue
		}
		seen[candidatePath] = struct{}{}
		prioritized = append(prioritized, entry)
	}
	sort.Slice(prioritized, func(i, j int) bool { return prioritized[i].Path < prioritized[j].Path })
	if indexedOnly {
		return prioritized, len(prioritized)
	}
	for _, entry := range all {
		if _, candidate := seen[entry.Path]; !candidate {
			prioritized = append(prioritized, entry)
		}
	}
	return prioritized, len(seen)
}

func indexedCandidates(paths []string, globs *GlobSet) ([]provider.Entry, error) {
	seen := make(map[string]struct{}, len(paths))
	entries := make([]provider.Entry, 0, len(paths))
	for _, candidatePath := range paths {
		if len(candidatePath) > provider.MaxRepositoryPathBytes {
			return nil, fmt.Errorf("index path: %w (%d bytes)", errRepositoryPathLimit, provider.MaxRepositoryPathBytes)
		}
		if candidatePath == "" || candidatePath == "." || candidatePath == ".." || strings.HasPrefix(candidatePath, "../") || path.IsAbs(candidatePath) || path.Clean(candidatePath) != candidatePath || strings.ContainsRune(candidatePath, '\x00') || !utf8.ValidString(candidatePath) {
			return nil, fmt.Errorf("index returned invalid path %q", candidatePath)
		}
		if !globs.Match(candidatePath) {
			continue
		}
		if _, duplicate := seen[candidatePath]; duplicate {
			continue
		}
		seen[candidatePath] = struct{}{}
		entries = append(entries, provider.Entry{Path: candidatePath, Mode: "100644"})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (r *Runner) scanEntry(ctx context.Context, snapshot provider.Snapshot, entry provider.Entry) fileOutcome {
	keyParts := []string{snapshot.Repository.CacheNamespace(), "blob", entry.OID}
	if entry.OID == "" {
		keyParts = []string{snapshot.Repository.CacheNamespace(), "file", snapshot.Commit, entry.Path}
	}
	key := cache.Key(keyParts...)
	warnings := make([]Event, 0, 1)
	if reader, hit, err := r.Cache.OpenContext(ctx, key); err == nil && hit {
		defer reader.Close()
		return r.scanReader(ctx, entry.Path, reader, true, 0)
	} else if err != nil {
		if IsContextError(err) {
			return fileOutcome{err: err}
		}
		warnings = append(warnings, Event{Type: "warning", Code: "cache_read_failed", Message: err.Error()})
	}

	remote, err := r.Provider.OpenBlob(ctx, snapshot, entry)
	if err != nil {
		return fileOutcome{err: err, warnings: warnings}
	}
	counter := &countingReader{reader: remote}
	buffered := bufio.NewReaderSize(counter, binaryProbeSize)
	probe, _ := buffered.Peek(binaryProbeSize)
	if err := ctx.Err(); err != nil {
		remote.Close()
		return fileOutcome{err: err, warnings: warnings, downloadedBytes: counter.read}
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		remote.Close()
		return fileOutcome{warnings: warnings, downloadedBytes: counter.read, skippedBinary: true}
	}

	if r.Cache.Enabled() {
		defer remote.Close()
		transaction, cacheErr := r.Cache.Begin(key)
		if cacheErr != nil {
			warnings = append(warnings, Event{Type: "warning", Code: "cache_write_failed", Message: cacheErr.Error()})
			outcome := r.scanReader(ctx, entry.Path, buffered, false, 0)
			outcome.downloadedBytes = counter.read
			outcome.warnings = append(outcome.warnings, warnings...)
			return outcome
		}
		defer transaction.Abort()
		cacheWriter := &bestEffortCacheWriter{transaction: transaction}
		outcome := r.scanReader(ctx, entry.Path, io.TeeReader(buffered, cacheWriter), false, 0)
		outcome.downloadedBytes = counter.read
		if cacheWriter.err != nil {
			warnings = append(warnings, Event{Type: "warning", Code: "cache_write_failed", Message: cacheWriter.err.Error()})
		} else if outcome.err == nil && !outcome.skippedBinary && !outcome.result.HitLimit {
			if err := ctx.Err(); err != nil {
				outcome.err = err
				outcome.warnings = append(outcome.warnings, warnings...)
				return outcome
			}
			if commitErr := transaction.Commit(); commitErr != nil {
				warnings = append(warnings, Event{Type: "warning", Code: "cache_write_failed", Message: commitErr.Error()})
			}
		}
		if outcome.err == nil {
			outcome.err = ctx.Err()
		}
		outcome.warnings = append(outcome.warnings, warnings...)
		return outcome
	}

	defer remote.Close()
	outcome := r.scanReader(ctx, entry.Path, buffered, false, 0)
	outcome.downloadedBytes = counter.read
	outcome.warnings = append(outcome.warnings, warnings...)
	if outcome.err == nil {
		outcome.err = ctx.Err()
	}
	return outcome
}

func (r *Runner) scanReader(ctx context.Context, path string, reader io.Reader, cacheHit bool, downloaded int64) fileOutcome {
	bounded := &boundedContextReader{
		ctx: ctx, reader: reader, remaining: maxScannedFileBytes,
		limit: maxScannedFileBytes, limitErr: errFileTooLarge,
	}
	buffered := bufio.NewReaderSize(bounded, binaryProbeSize)
	probe, _ := buffered.Peek(binaryProbeSize)
	if err := ctx.Err(); err != nil {
		return fileOutcome{err: err, cacheHit: cacheHit, downloadedBytes: downloaded}
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return fileOutcome{cacheHit: cacheHit, downloadedBytes: downloaded, skippedBinary: true}
	}
	spool := &eventSpool{}
	result, err := r.Matcher.ScanEmitContext(ctx, path, &binaryDetectReader{reader: buffered}, r.Config.MaxResults, spool.Emit)
	if errors.Is(err, errBinaryContent) || errors.Is(err, errInvalidUTF8) {
		spool.Abort()
		return fileOutcome{cacheHit: cacheHit, downloadedBytes: downloaded, skippedBinary: true}
	}
	if err != nil {
		spool.Abort()
		return fileOutcome{result: result, err: err, cacheHit: cacheHit, downloadedBytes: downloaded}
	}
	spoolPath, err := spool.Finish()
	if err != nil {
		return fileOutcome{result: result, err: err, cacheHit: cacheHit, downloadedBytes: downloaded}
	}
	return fileOutcome{result: result, cacheHit: cacheHit, downloadedBytes: downloaded, spoolPath: spoolPath}
}

type eventSpool struct {
	file    *os.File
	encoder *json.Encoder
	writer  *limitedSpoolWriter
}

func (s *eventSpool) Emit(event Event) error {
	if s.file == nil {
		file, err := os.CreateTemp("", "git-rg-events-*")
		if err != nil {
			return err
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			os.Remove(file.Name())
			return err
		}
		s.file = file
		s.writer = &limitedSpoolWriter{writer: file}
		s.encoder = json.NewEncoder(s.writer)
	}
	return s.encoder.Encode(event)
}

type limitedSpoolWriter struct {
	writer  io.Writer
	written int64
}

func (w *limitedSpoolWriter) Write(data []byte) (int, error) {
	if w.written+int64(len(data)) > maxResultSpoolBytes {
		return 0, fmt.Errorf("%w (%d bytes)", errResultSpoolLimit, maxResultSpoolBytes)
	}
	written, err := w.writer.Write(data)
	w.written += int64(written)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	return written, err
}

func (s *eventSpool) Finish() (string, error) {
	if s.file == nil {
		return "", nil
	}
	name := s.file.Name()
	if err := s.file.Close(); err != nil {
		os.Remove(name)
		s.file = nil
		return "", err
	}
	s.file = nil
	return name, nil
}

func (s *eventSpool) Abort() {
	if s.file == nil {
		return
	}
	name := s.file.Name()
	s.file.Close()
	os.Remove(name)
	s.file = nil
}

type binaryDetectReader struct {
	reader io.Reader
}

func (r *binaryDetectReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if bytes.IndexByte(buffer[:read], 0) >= 0 {
		return read, errBinaryContent
	}
	return read, err
}

type countingReader struct {
	reader io.Reader
	read   int64
}

type boundedContextReader struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
	limit     int64
	limitErr  error
}

func (r *boundedContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		var probe [1]byte
		read, err := r.reader.Read(probe[:])
		if read > 0 {
			return 0, fmt.Errorf("%w (%d bytes)", r.limitErr, r.limit)
		}
		if err == nil {
			return 0, io.ErrNoProgress
		}
		return 0, err
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:int(r.remaining)]
	}
	read, err := r.reader.Read(buffer)
	r.remaining -= int64(read)
	return read, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.read += int64(read)
	return read, err
}

func IsContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func incompleteReason(err error, fallback string) string {
	var budgetErr *provider.RequestBudgetError
	if errors.As(err, &budgetErr) {
		return "request_budget"
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) && httpErr.RateLimited {
		return "rate_limit"
	}
	if IsContextError(err) {
		return "cancelled"
	}
	if IsResourceLimit(err) {
		return "resource_limit"
	}
	return fallback
}

func IsResourceLimit(err error) bool {
	var providerLimit *provider.ResourceLimitError
	if errors.As(err, &providerLimit) {
		return true
	}
	return errors.Is(err, errLineTooLong) ||
		errors.Is(err, errContextWindowTooLarge) ||
		errors.Is(err, errSubmatchLimit) ||
		errors.Is(err, errFileTooLarge) ||
		errors.Is(err, errResultSpoolLimit) ||
		errors.Is(err, errArchiveCompressedLimit) ||
		errors.Is(err, errArchiveExpandedLimit) ||
		errors.Is(err, errArchiveMetadataLimit) ||
		errors.Is(err, errRepositoryPathLimit)
}

func ErrorCode(err error) string {
	var budgetErr *provider.RequestBudgetError
	if errors.As(err, &budgetErr) {
		return "request_budget_exceeded"
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) && httpErr.RateLimited {
		return "rate_limited"
	}
	if IsResourceLimit(err) {
		return "resource_limit"
	}
	var searchErr *Error
	if errors.As(err, &searchErr) {
		return searchErr.Code
	}
	return "search_failed"
}

func ErrorMessage(err error) string {
	var searchErr *Error
	if errors.As(err, &searchErr) {
		return searchErr.Err.Error()
	}
	return fmt.Sprint(err)
}
