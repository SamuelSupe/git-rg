package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

const binaryProbeSize = 8 << 10
const maxScannedFileBytes int64 = 512 << 20

var errBinaryContent = errors.New("binary content contains NUL")
var errFileTooLarge = errors.New("text file exceeds scan byte limit")

var probeReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, binaryProbeSize) }}

func entryCacheKey(snapshot provider.Snapshot, entry provider.Entry) string {
	keyParts := []string{snapshot.Repository.CacheNamespace(), "blob", entry.OID}
	if entry.OID == "" {
		keyParts = []string{snapshot.Repository.CacheNamespace(), "file", snapshot.Commit, entry.Path}
	}
	return cache.Key(keyParts...)
}

func (r *Runner) scanCachedEntry(ctx context.Context, snapshot provider.Snapshot, entry provider.Entry) fileOutcome {
	if reader, hit, err := r.Cache.OpenContext(ctx, entryCacheKey(snapshot, entry)); err == nil && hit {
		defer reader.Close()
		return r.scanReader(ctx, entry.Path, reader, true, 0)
	} else if err != nil {
		if IsContextError(err) {
			return fileOutcome{err: err}
		}
		return fileOutcome{cacheMiss: true, warnings: []Event{{Type: "warning", Code: "cache_read_failed", Message: err.Error()}}}
	}
	return fileOutcome{cacheMiss: true}
}

func (r *Runner) scanEntry(ctx context.Context, snapshot provider.Snapshot, entry provider.Entry) fileOutcome {
	cached := r.scanCachedEntry(ctx, snapshot, entry)
	if !cached.cacheMiss {
		return cached
	}
	key := entryCacheKey(snapshot, entry)
	warnings := cached.warnings

	remote, err := r.Provider.OpenBlob(ctx, snapshot, entry)
	if err != nil {
		return fileOutcome{err: err, warnings: warnings}
	}
	counter := &countingReader{reader: remote}
	buffered := probeReaders.Get().(*bufio.Reader)
	buffered.Reset(counter)
	defer func() {
		buffered.Reset(nil)
		probeReaders.Put(buffered)
	}()
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
	buffered := probeReaders.Get().(*bufio.Reader)
	buffered.Reset(bounded)
	defer func() {
		buffered.Reset(nil)
		probeReaders.Put(buffered)
	}()
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
		return fileOutcome{result: result, err: fmt.Errorf("%w for %q: %w", errResultSpool, path, err), cacheHit: cacheHit, downloadedBytes: downloaded}
	}
	result.Events = spool.events
	return fileOutcome{result: result, cacheHit: cacheHit, downloadedBytes: downloaded, spoolPath: spoolPath}
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
