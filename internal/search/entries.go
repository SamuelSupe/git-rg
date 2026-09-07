package search

import (
	"context"
	"encoding/gob"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/SamuelSupe/git-rg/internal/provider"
)

const maxSearchWorkers = 8

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
	cacheMiss       bool
	downloadedBytes int64
	skippedBinary   bool
	spoolPath       string
}

func (r *Runner) scanEntries(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, summary *Summary, matchedFiles map[string]struct{}, emit Emitter) (bool, error) {
	stopped, err := r.scanEntriesOrdered(ctx, snapshot, entries, r.scanEntry, func(outcome fileOutcome) (bool, error) {
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

func (r *Runner) scanCachedEntries(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, summary *Summary, matchedFiles map[string]struct{}, emit Emitter) ([]provider.Entry, bool, error) {
	missing := entries[:0]
	remainingStart := -1
	stopped, err := r.scanEntriesOrdered(ctx, snapshot, entries, r.scanCachedEntry, func(outcome fileOutcome) (bool, error) {
		if outcome.cacheMiss {
			if err := emitEvents(outcome.warnings, emit); err != nil {
				return true, err
			}
			missing = append(missing, entries[outcome.index])
			// Further cache hits cannot enable direct-blob scanning once misses
			// exceed its limit, so bound probes on mostly uncached trees.
			if len(missing) > maxSearchWorkers {
				remainingStart = outcome.index + 1
				return true, nil
			}
			return false, nil
		}
		summary.Transport = "blob"
		return r.consumeOutcome(ctx, outcome, summary, matchedFiles, emit, "read_blob", "blob_error")
	})
	if err != nil || (stopped && remainingStart < 0) {
		return nil, true, err
	}
	if remainingStart >= 0 {
		if ctx.Err() != nil {
			return nil, true, contextError(ctx)
		}
		missing = append(missing, entries[remainingStart:]...)
	}
	clear(entries[len(missing):])
	return missing, false, nil
}

func (r *Runner) scanEntriesOrdered(ctx context.Context, snapshot provider.Snapshot, entries []provider.Entry, scan func(context.Context, provider.Snapshot, provider.Entry) fileOutcome, consume func(fileOutcome) (bool, error)) (bool, error) {
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
				outcome := scan(runCtx, snapshot, job.entry)
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
		decoder := gob.NewDecoder(spool)
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
