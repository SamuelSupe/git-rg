package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

const maxTreeManifestSize = 64 << 20

func (r *Runner) loadTree(ctx context.Context, snapshot provider.Snapshot, globs *GlobSet, summary *Summary) ([]provider.Entry, int, []Event, error) {
	key := cache.Key(snapshot.Repository.CacheNamespace(), "tree", snapshot.Commit)
	warnings := make([]Event, 0, 1)
	if reader, hit, err := r.Cache.OpenContext(ctx, key); err == nil && hit {
		entries, total, decodeErr := decodeTreeManifestContext(ctx, reader, globs)
		reader.Close()
		if decodeErr == nil {
			summary.CacheHits++
			return entries, total, warnings, nil
		}
		if IsContextError(decodeErr) {
			return nil, 0, warnings, decodeErr
		}
		warnings = append(warnings, Event{Type: "warning", Code: "cache_read_failed", Message: "cached tree manifest is invalid; refreshing it"})
		if removeErr := r.Cache.Remove(key); removeErr != nil {
			warnings = append(warnings, Event{Type: "warning", Code: "cache_remove_failed", Message: removeErr.Error()})
		}
	} else if err != nil {
		if IsContextError(err) {
			return nil, 0, warnings, err
		}
		warnings = append(warnings, Event{Type: "warning", Code: "cache_read_failed", Message: err.Error()})
	}

	entries, complete, err := r.Provider.ListTree(ctx, snapshot, true)
	if err != nil {
		return nil, 0, warnings, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, warnings, err
	}
	if !complete {
		return nil, 0, warnings, errors.New("provider returned an incomplete tree manifest when a complete manifest was required")
	}
	if entries == nil {
		entries = []provider.Entry{}
	}
	if err := normalizeTreeEntries(entries); err != nil {
		return nil, 0, warnings, fmt.Errorf("provider returned an invalid tree manifest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, warnings, err
	}
	if r.Cache.Enabled() && treeManifestFitsCache(entries) {
		encoded, encodeErr := json.Marshal(entries)
		if encodeErr == nil && len(encoded) <= maxTreeManifestSize && ctx.Err() == nil {
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
		return nil, 0, warnings, err
	}
	total := len(entries)
	return filterEntries(entries, globs), total, warnings, nil
}

func treeManifestFitsCache(entries []provider.Entry) bool {
	remaining := int64(maxTreeManifestSize - 2)
	for i, entry := range entries {
		size := int64(len(`{"path":"","oid":"","mode":""}`)) + jsonStringContentSize(entry.Path) + jsonStringContentSize(entry.OID) + jsonStringContentSize(entry.Mode)
		if entry.Size != 0 {
			size += int64(len(`,"size":`) + len(strconv.FormatInt(entry.Size, 10)))
		}
		if i > 0 {
			size++
		}
		if size > remaining {
			return false
		}
		remaining -= size
	}
	return true
}

// Match encoding/json's escaping before allocating the entire manifest.
func jsonStringContentSize(value string) int64 {
	var size int64
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\', '\n', '\r', '\t', '\b', '\f':
				size += 2
			case '<', '>', '&':
				size += 6
			default:
				if c < 0x20 {
					size += 6
				} else {
					size++
				}
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(value[i:])
		if r == '\u2028' || r == '\u2029' || r == utf8.RuneError && n == 1 {
			size += 6
		} else {
			size += int64(n)
		}
		i += n
	}
	return size
}

func decodeTreeManifestContext(ctx context.Context, reader io.Reader, globs *GlobSet) ([]provider.Entry, int, error) {
	limited := &io.LimitedReader{R: reader, N: maxTreeManifestSize + 1}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: limited})
	opening, err := decoder.Token()
	if err != nil {
		return nil, 0, err
	}
	if opening != json.Delim('[') {
		return nil, 0, errors.New("tree manifest must be a JSON array")
	}
	var entries []provider.Entry
	var entry provider.Entry
	var previous string
	total := 0
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		entry = provider.Entry{}
		if err := decoder.Decode(&entry); err != nil {
			return nil, 0, err
		}
		if err := validateTreeEntry(entry); err != nil {
			return nil, 0, err
		}
		// Persisted manifests are sorted. Checking adjacent paths validates
		// uniqueness even for entries excluded by the glob, without a full-tree map.
		if total > 0 && entry.Path <= previous {
			return nil, 0, errors.New("tree manifest paths must be unique and sorted")
		}
		previous = entry.Path
		total++
		if globs == nil || globs.Match(entry.Path) {
			entries = append(entries, entry)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, 0, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, 0, errors.New("tree manifest contains trailing JSON")
		}
		return nil, 0, err
	}
	if limited.N == 0 {
		return nil, 0, fmt.Errorf("tree manifest exceeds %d bytes", maxTreeManifestSize)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

func normalizeTreeEntries(entries []provider.Entry) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for i, entry := range entries {
		if err := validateTreeEntry(entry); err != nil {
			return err
		}
		if i > 0 && entries[i-1].Path == entry.Path {
			return fmt.Errorf("duplicate path %q", entry.Path)
		}
	}
	return nil
}

func validateTreeEntry(entry provider.Entry) error {
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
	return nil
}

func orderCandidates(entries []provider.Entry, paths []string) []provider.Entry {
	byPath := make(map[string]provider.Entry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	seen := make(map[string]struct{}, len(paths))
	prioritized := make([]provider.Entry, 0, len(paths))
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
	return prioritized
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
