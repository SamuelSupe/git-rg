package search

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

const maxExpandedArchiveBytes int64 = 4 << 30
const maxCompressedArchiveBytes int64 = 512 << 20
const maxArchiveHeaders = 1_000_000
const maxArchivePathBytes int64 = 128 << 20

var errArchiveExpandedLimit = errors.New("archive exceeds expanded byte limit")
var errArchiveCompressedLimit = errors.New("archive exceeds compressed byte limit")
var errArchiveMetadataLimit = errors.New("archive exceeds metadata limit")
var errRepositoryPathLimit = errors.New("repository path exceeds byte limit")

type archiveUnavailableError struct {
	err error
}

func (e *archiveUnavailableError) Error() string { return e.err.Error() }
func (e *archiveUnavailableError) Unwrap() error { return e.err }

type bestEffortCacheWriter struct {
	transaction *cache.Transaction
	err         error
}

func (w *bestEffortCacheWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return len(data), nil
	}
	written, err := w.transaction.Write(data)
	if err != nil {
		w.err = err
		return len(data), nil
	}
	if written != len(data) {
		w.err = io.ErrShortWrite
		return len(data), nil
	}
	return written, nil
}

func (r *Runner) scanArchive(ctx context.Context, snapshot provider.Snapshot, globs *GlobSet, skip map[string]struct{}, summary *Summary, matchedFiles map[string]struct{}, emit Emitter) (bool, error) {
	key := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
	archive, cacheHit, err := r.Cache.OpenContext(ctx, key)
	if err != nil {
		if IsContextError(err) {
			return true, contextError(ctx)
		}
		if emitErr := emit(Event{Type: "warning", Code: "cache_read_failed", Message: err.Error()}); emitErr != nil {
			return true, &Error{Code: "write_output", Err: emitErr}
		}
	}

	var gzipReader *gzip.Reader
	var counter *countingReader
	var transaction *cache.Transaction
	var cacheWriter *bestEffortCacheWriter
	cacheCounted := false
	invalidateCache := func() error {
		if !cacheHit {
			return nil
		}
		if gzipReader != nil {
			_ = gzipReader.Close()
		}
		if archive != nil {
			_ = archive.Close()
		}
		removeErr := r.Cache.Remove(key)
		if cacheCounted {
			summary.CacheHits--
			cacheCounted = false
		}
		if removeErr != nil {
			if emitErr := emit(Event{Type: "warning", Code: "cache_remove_failed", Message: removeErr.Error()}); emitErr != nil {
				return &Error{Code: "write_output", Err: emitErr}
			}
		}
		return nil
	}
	if cacheHit {
		gzipReader, err = gzip.NewReader(&contextReader{ctx: ctx, reader: archive})
		if err != nil {
			archive.Close()
			if IsContextError(err) {
				return true, contextError(ctx)
			}
			if invalidateErr := invalidateCache(); invalidateErr != nil {
				return true, invalidateErr
			}
			cacheHit = false
			if emitErr := emit(Event{Type: "warning", Code: "cache_read_failed", Message: "cached archive is invalid; refreshing it"}); emitErr != nil {
				return true, &Error{Code: "write_output", Err: emitErr}
			}
		} else {
			summary.CacheHits++
			cacheCounted = true
			defer archive.Close()
		}
	}
	if !cacheHit {
		archive, err = r.Provider.OpenArchive(ctx, snapshot)
		if err != nil {
			return false, &archiveUnavailableError{err: err}
		}
		defer archive.Close()
		counter = &countingReader{reader: archive}
		stream := io.Reader(&boundedContextReader{
			ctx: ctx, reader: counter, remaining: maxCompressedArchiveBytes,
			limit: maxCompressedArchiveBytes, limitErr: errArchiveCompressedLimit,
		})
		if r.Cache.Enabled() {
			transaction, err = r.Cache.Begin(key)
			if err != nil {
				if emitErr := emit(Event{Type: "warning", Code: "cache_write_failed", Message: err.Error()}); emitErr != nil {
					return true, &Error{Code: "write_output", Err: emitErr}
				}
			} else {
				defer transaction.Abort()
				cacheWriter = &bestEffortCacheWriter{transaction: transaction}
				stream = io.TeeReader(stream, cacheWriter)
			}
		}
		defer func() { summary.DownloadedBytes += counter.read }()
		gzipReader, err = gzip.NewReader(&contextReader{ctx: ctx, reader: stream})
		if err != nil {
			return false, &archiveUnavailableError{err: fmt.Errorf("open tar.gz stream: %w", err)}
		}
	}
	defer gzipReader.Close()

	seen := make(map[string]struct{})
	expanded := &boundedContextReader{
		ctx: ctx, reader: gzipReader, remaining: maxExpandedArchiveBytes,
		limit: maxExpandedArchiveBytes, limitErr: errArchiveExpandedLimit,
	}
	tarReader := tar.NewReader(expanded)
	archiveHeaders := 0
	var archivePathBytes int64
	for {
		if err := ctx.Err(); err != nil {
			summary.Complete = false
			summary.Reason = "cancelled"
			return true, contextError(ctx)
		}
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			if IsContextError(nextErr) || IsResourceLimit(nextErr) {
				summary.Complete = false
				summary.Reason = incompleteReason(nextErr, "archive_error")
				return true, &Error{Code: "read_archive", Err: fmt.Errorf("read tar header: %w", nextErr)}
			}
			if invalidateErr := invalidateCache(); invalidateErr != nil {
				return true, invalidateErr
			}
			if len(seen) == 0 {
				return false, &archiveUnavailableError{err: fmt.Errorf("read tar header: %w", nextErr)}
			}
			summary.Complete = false
			summary.Reason = "archive_error"
			return true, &Error{Code: "read_archive", Err: fmt.Errorf("read tar header: %w", nextErr)}
		}
		archiveHeaders++
		archivePathBytes += int64(len(header.Name))
		if archiveHeaders > maxArchiveHeaders || archivePathBytes > maxArchivePathBytes {
			summary.Complete = false
			summary.Reason = "resource_limit"
			return true, &Error{Code: "read_archive", Err: fmt.Errorf("%w (%d headers, %d path bytes)", errArchiveMetadataLimit, archiveHeaders, archivePathBytes)}
		}
		relative, file, pathErr := archiveRelativePath(header.Name)
		if pathErr != nil {
			if IsResourceLimit(pathErr) {
				summary.Complete = false
				summary.Reason = "resource_limit"
				return true, &Error{Code: "read_archive", Err: pathErr}
			}
			if invalidateErr := invalidateCache(); invalidateErr != nil {
				return true, invalidateErr
			}
			if len(seen) == 0 {
				return false, &archiveUnavailableError{err: pathErr}
			}
			summary.Complete = false
			summary.Reason = "archive_error"
			return true, &Error{Code: "read_archive", Err: pathErr}
		}
		if !file {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if _, duplicate := seen[relative]; duplicate {
			if invalidateErr := invalidateCache(); invalidateErr != nil {
				return true, invalidateErr
			}
			summary.Complete = false
			summary.Reason = "archive_error"
			return true, &Error{Code: "read_archive", Err: fmt.Errorf("archive contains duplicate path %q", relative)}
		}
		seen[relative] = struct{}{}
		if !globs.Match(relative) {
			continue
		}
		if _, skipped := skip[relative]; skipped {
			continue
		}
		outcome := r.scanReader(ctx, relative, io.LimitReader(tarReader, header.Size), false, 0)
		if outcome.err != nil && shouldInvalidateArchive(outcome.err) {
			if invalidateErr := invalidateCache(); invalidateErr != nil {
				cleanupOutcome(outcome)
				return true, invalidateErr
			}
		}
		stopped, consumeErr := r.consumeOutcome(ctx, outcome, summary, matchedFiles, emit, "read_archive", "archive_error")
		if consumeErr != nil || stopped {
			return true, consumeErr
		}
	}
	if _, err := io.Copy(io.Discard, expanded); err != nil {
		if IsContextError(err) || IsResourceLimit(err) {
			summary.Complete = false
			summary.Reason = incompleteReason(err, "archive_error")
			return true, &Error{Code: "read_archive", Err: fmt.Errorf("verify gzip stream: %w", err)}
		}
		if invalidateErr := invalidateCache(); invalidateErr != nil {
			return true, invalidateErr
		}
		summary.Complete = false
		summary.Reason = "archive_error"
		return true, &Error{Code: "read_archive", Err: fmt.Errorf("verify gzip stream: %w", err)}
	}
	if cacheWriter != nil {
		if cacheWriter.err != nil {
			if emitErr := emit(Event{Type: "warning", Code: "cache_write_failed", Message: cacheWriter.err.Error()}); emitErr != nil {
				return true, &Error{Code: "write_output", Err: emitErr}
			}
		} else if err := ctx.Err(); err != nil {
			summary.Complete = false
			summary.Reason = "cancelled"
			return true, contextError(ctx)
		} else if commitErr := transaction.Commit(); commitErr != nil {
			if emitErr := emit(Event{Type: "warning", Code: "cache_write_failed", Message: commitErr.Error()}); emitErr != nil {
				return true, &Error{Code: "write_output", Err: emitErr}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		summary.Complete = false
		summary.Reason = "cancelled"
		return true, contextError(ctx)
	}
	return false, nil
}

func archiveRelativePath(name string) (string, bool, error) {
	if !utf8.ValidString(name) {
		return "", false, errors.New("archive contains a path that is not valid UTF-8")
	}
	if len(name) > provider.MaxRepositoryPathBytes {
		return "", false, fmt.Errorf("%w (%d bytes)", errRepositoryPathLimit, provider.MaxRepositoryPathBytes)
	}
	if strings.ContainsRune(name, '\x00') {
		return "", false, errors.New("archive contains a path with NUL")
	}
	name = strings.TrimPrefix(name, "./")
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return "", false, fmt.Errorf("archive contains unsafe path %q", name)
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", false, nil
	}
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false, fmt.Errorf("archive contains unsafe path %q", name)
	}
	_, relative, found := strings.Cut(cleaned, "/")
	if !found || relative == "" {
		return "", false, nil
	}
	if relative == ".." || strings.HasPrefix(relative, "../") {
		return "", false, fmt.Errorf("archive contains unsafe path %q", name)
	}
	return relative, true, nil
}

func shouldInvalidateArchive(err error) bool {
	return !IsResourceLimit(err) &&
		!errors.Is(err, errResultSpool) &&
		!IsContextError(err)
}
