package search

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

func TestRunnerExactScansInPathOrderAndSkipsBinary(t *testing.T) {
	remote := &fakeProvider{
		entries: []provider.Entry{
			{Path: "z.txt", OID: "z"},
			{Path: "binary.dat", OID: "binary"},
			{Path: "a.txt", OID: "a"},
		},
		blobs: map[string][]byte{
			"z":      []byte("needle in z\n"),
			"binary": []byte{'n', 0, 'e', 'e', 'd', 'l', 'e'},
			"a":      []byte("needle in a\n"),
		},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var events []Event
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 2}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if summary.MatchedLines != 2 || summary.MatchedFiles != 2 || summary.ScannedFiles != 3 || summary.SkippedBinary != 1 || !summary.Complete || summary.Truncated {
		t.Fatalf("summary = %#v", summary)
	}
	var matches []string
	matchText := make(map[string]string)
	for _, event := range events {
		if event.Type == "match" {
			matches = append(matches, event.Path)
			matchText[event.Path] = event.Text
		}
	}
	if !reflect.DeepEqual(matches, []string{"a.txt", "z.txt"}) {
		t.Fatalf("match paths = %v, want [a.txt z.txt]", matches)
	}
	if !reflect.DeepEqual(matchText, map[string]string{"a.txt": "needle in a", "z.txt": "needle in z"}) {
		t.Fatalf("match text by path = %v, want file-specific lines", matchText)
	}
	if got := remote.openCount(); got != 0 || remote.archiveCount() != 1 || remote.listCount() != 1 || !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) {
		t.Fatalf("OpenBlob/OpenArchive/ListTree calls = %d/%d/%d/%v, want 0/1/1/[true]", got, remote.archiveCount(), remote.listCount(), remote.listRequireCompleteCopy())
	}
}

func TestRunnerExactSupplementsArchiveOmissionFromTreeBlob(t *testing.T) {
	remote := &fakeProvider{
		entries: []provider.Entry{
			{Path: "a.txt", OID: "a", Mode: "100644"},
			{Path: "missing.txt", OID: "missing", Mode: "100644"},
		},
		blobs: map[string][]byte{
			"a":       []byte("needle archive\n"),
			"missing": []byte("needle original\n"),
		},
		archiveData: makeTestArchive(t, testArchiveFile{
			name: "owner-repo-commit/a.txt",
			data: []byte("needle archive\n"),
		}),
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var matches []string
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		if event.Type == "match" {
			matches = append(matches, event.Path)
		}
		return nil
	})
	if err != nil || !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != 2 || summary.MatchedFiles != 2 {
		t.Fatalf("summary/error = %#v/%v, want complete archive plus blob supplement", summary, err)
	}
	if !reflect.DeepEqual(matches, []string{"a.txt", "missing.txt"}) {
		t.Fatalf("match paths = %v, want archive path followed by omitted tree path", matches)
	}
	if remote.archiveCount() != 1 || remote.openCount() != 1 || remote.listCount() != 1 {
		t.Fatalf("Archive/Blob/ListTree calls = %d/%d/%d, want 1/1/1", remote.archiveCount(), remote.openCount(), remote.listCount())
	}
}

func TestRunnerNarrowGlobUsesBlobBeforeArchiveOrIndex(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode Mode
		hot  bool
	}{
		{name: "exact cold", mode: ModeExact},
		{name: "auto cold", mode: ModeAuto},
		{name: "exact blob cache", mode: ModeExact, hot: true},
		{name: "auto blob cache", mode: ModeAuto, hot: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			selected := []byte("needle selected\n")
			remote := &fakeProvider{
				entries: []provider.Entry{
					{Path: "selected.md", OID: "selected", Mode: "100644"},
					{Path: "other.go", OID: "other", Mode: "100644", Size: 1},
					{Path: "third.txt", OID: "third", Mode: "100644", Size: 1},
				},
				blobs: map[string][]byte{
					"selected": selected,
					"other":    []byte("other\n"),
					"third":    []byte("third\n"),
				},
				candidates: []string{"selected.md"},
			}
			objectCache := cache.Disabled()
			if tt.hot {
				objectCache = newTestCache(t)
				blobKey := cache.Key(testSnapshot().Repository.CacheNamespace(), "blob", testGitBlobOID(selected))
				stored, _, err := objectCache.Put(blobKey, bytes.NewReader(selected))
				if err != nil {
					t.Fatalf("seed blob cache: %v", err)
				}
				stored.Close()
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			runner := &Runner{
				Provider: remote,
				Cache:    objectCache,
				Matcher:  matcher,
				Config:   Config{Mode: tt.mode, Workers: 1, Globs: []string{"*.md"}},
			}
			summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
			if err != nil || !summary.Complete || summary.Transport != "blob" || summary.MatchedLines != 1 || summary.ScannedFiles != 1 {
				t.Fatalf("summary/error = %#v/%v, want complete narrow blob scan", summary, err)
			}
			if remote.listCount() != 1 || !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) {
				t.Fatalf("ListTree calls = %d/%v, want one complete tree request", remote.listCount(), remote.listRequireCompleteCopy())
			}
			if remote.archiveCount() != 0 || remote.searchCount() != 0 {
				t.Fatalf("OpenArchive/SearchCandidates calls = %d/%d, want zero for narrow glob", remote.archiveCount(), remote.searchCount())
			}
			wantBlobCalls := 1
			if tt.hot {
				wantBlobCalls = 0
				if summary.CacheHits != 1 {
					t.Fatalf("CacheHits = %d, want one hot blob cache hit", summary.CacheHits)
				}
			} else if summary.CacheHits != 0 {
				t.Fatalf("CacheHits = %d, want zero cold-cache hits", summary.CacheHits)
			}
			if remote.openCount() != wantBlobCalls {
				t.Fatalf("OpenBlob calls = %d, want %d", remote.openCount(), wantBlobCalls)
			}
		})
	}
}

func TestRunnerNarrowGlobConsumesCachedBlobsBeforeTransportLimits(t *testing.T) {
	const normalEntrySize = int64(len("needle file 000\n"))
	for _, tt := range []struct {
		name          string
		mode          Mode
		selected      int
		cached        int
		missingIndex  int
		entrySize     int64
		stats         provider.RequestStats
		wantOpenBlobs int
	}{
		{name: "exact nine all cached", mode: ModeExact, selected: 9, cached: 9, missingIndex: -1, entrySize: normalEntrySize},
		{name: "auto nine all cached", mode: ModeAuto, selected: 9, cached: 9, missingIndex: -1, entrySize: normalEntrySize},
		{name: "nine cached one missing in middle", mode: ModeExact, selected: 10, cached: 9, missingIndex: 4, entrySize: normalEntrySize, wantOpenBlobs: 1},
		{name: "unknown size cached", mode: ModeExact, selected: 2, cached: 2, missingIndex: -1, entrySize: 0},
		{name: "over eight mib cached", mode: ModeExact, selected: 1, cached: 1, missingIndex: -1, entrySize: 9 << 20},
		{name: "request budget exhausted cached", mode: ModeExact, selected: 1, cached: 1, missingIndex: -1, entrySize: normalEntrySize, stats: provider.RequestStats{Requests: 1, RequestLimit: 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner, remote, snapshot, _ := newCachedGlobRunner(t, 100, tt.selected, tt.cached, tt.missingIndex, tt.entrySize, tt.mode, tt.stats)
			summary, err := runner.Run(context.Background(), snapshot, func(Event) error { return nil })
			if err != nil || !summary.Complete || summary.MatchedLines != tt.selected || summary.MatchedFiles != tt.selected || summary.ScannedFiles != tt.selected {
				t.Fatalf("summary/error = %#v/%v, want complete %d-file result", summary, err, tt.selected)
			}
			if remote.listCount() != 0 || remote.searchCount() != 0 || remote.archiveCount() != 0 || remote.openCount() != tt.wantOpenBlobs {
				t.Fatalf("provider calls = list %d/search %d/archive %d/blob %d, want 0/0/0/%d", remote.listCount(), remote.searchCount(), remote.archiveCount(), remote.openCount(), tt.wantOpenBlobs)
			}
			if summary.CacheHits != tt.cached+1 {
				t.Fatalf("CacheHits = %d, want tree plus %d cached blobs", summary.CacheHits, tt.cached)
			}
			if summary.Transport != "blob" {
				t.Fatalf("Transport = %q, want blob", summary.Transport)
			}
		})
	}
}

func TestRunnerNarrowGlobCorruptCachedBlobWarnsAndFallsBack(t *testing.T) {
	runner, remote, snapshot, root := newCachedGlobRunner(t, 100, 9, 9, -1, int64(len("needle file\n")), ModeExact, provider.RequestStats{})
	corruptCachedPayload(t, root, []byte(fmt.Sprintf("needle file %03d", 7)))
	var warnings []Event
	summary, err := runner.Run(context.Background(), snapshot, func(event Event) error {
		if event.Type == "warning" {
			warnings = append(warnings, event)
		}
		return nil
	})
	if err != nil || !summary.Complete || summary.MatchedLines != 9 || summary.MatchedFiles != 9 || summary.ScannedFiles != 9 {
		t.Fatalf("summary/error = %#v/%v, want complete nine-file result", summary, err)
	}
	if !hasEventCode(warnings, "cache_read_failed") {
		t.Fatalf("warnings = %#v, want cache_read_failed for corrupted blob", warnings)
	}
	if remote.openCount() != 1 || remote.archiveCount() != 0 || remote.searchCount() != 0 || remote.listCount() != 0 {
		t.Fatalf("provider calls = list %d/search %d/archive %d/blob %d paths=%v, want 0/0/0/1", remote.listCount(), remote.searchCount(), remote.archiveCount(), remote.openCount(), remote.blobPathsCopy())
	}
	if summary.CacheHits != 9 || summary.Transport != "blob" {
		t.Fatalf("CacheHits/Transport = %d/%q, want tree plus eight valid blob hits and blob", summary.CacheHits, summary.Transport)
	}
}

func TestRunnerNarrowGlobCachedPrefixHonorsResultLimit(t *testing.T) {
	runner, remote, snapshot, _ := newCachedGlobRunner(t, 100, 9, 1, -1, int64(len("needle file\n")), ModeExact, provider.RequestStats{})
	runner.Config.MaxResults = 1
	summary, err := runner.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || summary.Complete || !summary.Truncated || summary.Reason != "result_limit" || summary.MatchedLines != 1 || summary.MatchedFiles != 1 {
		t.Fatalf("summary/error = %#v/%v, want cached prefix result-limit stop", summary, err)
	}
	if remote.openCount() != 0 || remote.archiveCount() != 0 || remote.searchCount() != 0 || remote.listCount() != 0 {
		t.Fatalf("provider calls = list %d/search %d/archive %d/blob %d, want no requests after cached prefix", remote.listCount(), remote.searchCount(), remote.archiveCount(), remote.openCount())
	}
	if summary.CacheHits != 2 || summary.Transport != "blob" {
		t.Fatalf("CacheHits/Transport = %d/%q, want tree plus first blob and blob", summary.CacheHits, summary.Transport)
	}
}

func TestRunnerNarrowGlobArchiveCompletesAfterCacheMissCutoff(t *testing.T) {
	const selectedFiles = 20
	runner, remote, snapshot, _ := newCachedGlobRunner(t, 100, selectedFiles, 1, -1, int64(len("needle file 000\n")), ModeExact, provider.RequestStats{})
	runner.Config.Globs = []string{"pkg/file-0[01]?.txt"}
	remote.archiveErr = nil
	var matches []string
	summary, err := runner.Run(context.Background(), snapshot, func(event Event) error {
		if event.Type == "match" {
			matches = append(matches, event.Path)
		}
		return nil
	})
	if err != nil || !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != selectedFiles || summary.MatchedFiles != selectedFiles || summary.ScannedFiles != selectedFiles {
		t.Fatalf("summary/error = %#v/%v, want complete archive result for %d selected files", summary, err, selectedFiles)
	}
	if summary.CacheHits != 2 || remote.listCount() != 0 || remote.searchCount() != 0 || remote.archiveCount() != 1 || remote.openCount() != 0 {
		t.Fatalf("cache/provider counts = cache %d, list %d/search %d/archive %d/blob %d; want 2/0/0/1/0", summary.CacheHits, remote.listCount(), remote.searchCount(), remote.archiveCount(), remote.openCount())
	}
	want := make([]string, selectedFiles)
	counts := make(map[string]int, selectedFiles)
	for i := range want {
		want[i] = fmt.Sprintf("pkg/file-%03d.txt", i)
	}
	for _, path := range matches {
		counts[path]++
	}
	sort.Strings(matches)
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("match paths = %v, want each selected path once in %v", matches, want)
	}
	for _, path := range want {
		if counts[path] != 1 {
			t.Fatalf("match count for %q = %d, want one", path, counts[path])
		}
	}
}

func newCachedGlobRunner(t *testing.T, totalFiles, selectedFiles, cachedFiles, missingIndex int, entrySize int64, mode Mode, stats provider.RequestStats) (*Runner, *fakeProvider, provider.Snapshot, string) {
	t.Helper()
	objectCache := newTestCache(t)
	root := os.Getenv("XDG_CACHE_HOME")
	if root == "" {
		t.Fatal("XDG_CACHE_HOME is empty after newTestCache")
	}
	snapshot := testSnapshot()
	entries := make([]provider.Entry, totalFiles)
	blobs := make(map[string][]byte, totalFiles)
	for i := range entries {
		data := []byte(fmt.Sprintf("needle file %03d\n", i))
		oid := testGitBlobOID(data)
		entries[i] = provider.Entry{
			Path: fmt.Sprintf("pkg/file-%03d.txt", i),
			OID:  oid,
			Mode: "100644",
			Size: entrySize,
		}
		blobs[oid] = data
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal cached tree: %v", err)
	}
	treeKey := cache.Key(snapshot.Repository.CacheNamespace(), "tree", snapshot.Commit)
	stored, _, err := objectCache.Put(treeKey, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("seed tree cache: %v", err)
	}
	stored.Close()
	seeded := 0
	for i := 0; i < selectedFiles && seeded < cachedFiles; i++ {
		if i == missingIndex {
			continue
		}
		blobKey := cache.Key(snapshot.Repository.CacheNamespace(), "blob", entries[i].OID)
		stored, _, err := objectCache.Put(blobKey, bytes.NewReader(blobs[entries[i].OID]))
		if err != nil {
			t.Fatalf("seed blob cache %q: %v", entries[i].Path, err)
		}
		stored.Close()
		seeded++
	}
	remote := &fakeProvider{
		entries:    entries,
		blobs:      blobs,
		archiveErr: errors.New("archive intentionally disabled"),
		stats:      stats,
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{
		Provider: remote,
		Cache:    objectCache,
		Matcher:  matcher,
		Config: Config{
			Mode:    mode,
			Workers: 4,
			Globs:   []string{cachedGlobPattern(selectedFiles)},
		},
	}
	return runner, remote, snapshot, root
}

func cachedGlobPattern(selectedFiles int) string {
	if selectedFiles == 1 {
		return "pkg/file-000.txt"
	}
	return fmt.Sprintf("pkg/file-00[0-%d].txt", selectedFiles-1)
}

func corruptCachedPayload(t *testing.T, root string, marker []byte) {
	t.Helper()
	found := false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if found || info.IsDir() || !strings.HasSuffix(info.Name(), ".entry") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		index := bytes.Index(data, marker)
		if index < 0 {
			return nil
		}
		data[index] ^= 0xff
		if err := os.WriteFile(path, data, info.Mode().Perm()); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk cache to corrupt %q: %v", marker, err)
	}
	if !found {
		t.Fatalf("cache payload %q not found", marker)
	}
}

func TestRunnerNarrowGlobBlobPrefetchFailureFallsBackToCachedArchive(t *testing.T) {
	objectCache := newTestCache(t)
	snapshot := testSnapshot()
	data := []byte("needle selected\n")
	archiveKey := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
	stored, _, err := objectCache.Put(archiveKey, bytes.NewReader(makeTestArchive(t, testArchiveFile{
		name: "owner-repo-commit/selected.md",
		data: data,
	})))
	if err != nil {
		t.Fatalf("seed archive cache: %v", err)
	}
	stored.Close()
	remote := &fakeProvider{
		entries: []provider.Entry{
			{Path: "selected.md", OID: "selected", Mode: "100644", Size: int64(len(data))},
			{Path: "other.go", OID: "other", Mode: "100644", Size: 1},
		},
		blobs:    map[string][]byte{"selected": data, "other": []byte("other\n")},
		blobErrs: map[string]error{"selected.md": errors.New("blob endpoint unavailable")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var warnings []Event
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1, Globs: []string{"*.md"}}}
	summary, err := runner.Run(context.Background(), snapshot, func(event Event) error {
		if event.Type == "warning" {
			warnings = append(warnings, event)
		}
		return nil
	})
	if err != nil || !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != 1 || summary.ScannedFiles != 1 {
		t.Fatalf("summary/error = %#v/%v, want complete cached archive fallback", summary, err)
	}
	if !hasEventCode(warnings, "blob_prefetch_failed") {
		t.Fatalf("warnings = %#v, want blob_prefetch_failed", warnings)
	}
	if summary.CacheHits != 1 || remote.archiveCount() != 0 || remote.searchCount() != 0 || remote.openCount() != 1 {
		t.Fatalf("cache/Archive/Search/Blob = %d/%d/%d/%d, want cached archive, no index, one failed blob", summary.CacheHits, remote.archiveCount(), remote.searchCount(), remote.openCount())
	}
}

func TestRunnerExplicitFullGlobStillUsesArchive(t *testing.T) {
	remote := &fakeProvider{
		entries: []provider.Entry{
			{Path: "a.txt", OID: "a", Mode: "100644", Size: int64(len("needle a\n"))},
			{Path: "b.txt", OID: "b", Mode: "100644", Size: int64(len("needle b\n"))},
		},
		blobs: map[string][]byte{"a": []byte("needle a\n"), "b": []byte("needle b\n")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1, Globs: []string{"**"}}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil || !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != 2 {
		t.Fatalf("summary/error = %#v/%v, want complete full archive scan", summary, err)
	}
	if remote.archiveCount() != 1 || remote.openCount() != 0 || remote.searchCount() != 0 {
		t.Fatalf("Archive/Blob/Search calls = %d/%d/%d, want 1/0/0", remote.archiveCount(), remote.openCount(), remote.searchCount())
	}
}

func TestRunnerNarrowGlobFallsBackToArchiveWhenBlobBudgetOrSizeIsUnsafe(t *testing.T) {
	data := []byte("needle selected\n")
	oversizedData := append([]byte("needle\n"), bytes.Repeat([]byte{'x'}, maxLineBytes)...)
	for _, tt := range []struct {
		name         string
		data         []byte
		entrySize    int64
		stats        provider.RequestStats
		requestLimit int
		wantBlobs    int
	}{
		{name: "request budget", data: data, entrySize: int64(len(data)), stats: provider.RequestStats{Requests: 1, RequestLimit: 3}, requestLimit: 3},
		{name: "oversized blob", data: oversizedData, entrySize: int64(len(oversizedData))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			remote := &fakeProvider{
				entries: []provider.Entry{
					{Path: "selected.md", OID: "selected", Mode: "100644", Size: tt.entrySize},
					{Path: "other.go", OID: "other", Mode: "100644", Size: 1},
				},
				blobs: map[string][]byte{"selected": tt.data, "other": []byte("other\n")},
				stats: tt.stats,
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1, Globs: []string{"*.md"}, RequestLimit: tt.requestLimit}}
			summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
			if err != nil || !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != 1 {
				t.Fatalf("summary/error = %#v/%v, want complete archive path", summary, err)
			}
			if remote.archiveCount() != 1 || remote.searchCount() != 0 || remote.openCount() != tt.wantBlobs {
				t.Fatalf("Archive/Search/Blob calls = %d/%d/%d, want 1/0/%d", remote.archiveCount(), remote.searchCount(), remote.openCount(), tt.wantBlobs)
			}
		})
	}
}

func TestRunnerExactRejectsArchiveContentTransformationAndScansOriginalBlob(t *testing.T) {
	original := []byte("needle original\n")
	transformed := []byte("needle replaced\n")
	for _, tt := range []struct {
		name      string
		pattern   string
		wantTexts []string
		wantLines int
	}{
		{name: "original blob", pattern: "original", wantTexts: []string{"needle original"}, wantLines: 1},
		{name: "archive-only transformation", pattern: "replaced", wantLines: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			remote := &fakeProvider{
				entries: []provider.Entry{{Path: "README.md", OID: "original", Mode: "100644", Size: int64(len(original))}},
				blobs:   map[string][]byte{"original": original},
				archiveData: makeTestArchive(t, testArchiveFile{
					name: "owner-repo-commit/README.md",
					data: transformed,
				}),
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: tt.pattern})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			var texts []string
			runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
			summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
				if event.Type == "match" {
					texts = append(texts, event.Text)
				}
				return nil
			})
			if err != nil || !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != tt.wantLines {
				t.Fatalf("summary/error = %#v/%v, want complete original-blob result", summary, err)
			}
			if !reflect.DeepEqual(texts, tt.wantTexts) {
				t.Fatalf("match texts = %v, want original blob content only", texts)
			}
			if remote.openCount() != 1 {
				t.Fatalf("OpenBlob calls = %d, want one supplement after archive digest mismatch", remote.openCount())
			}
		})
	}
}

func TestRunnerLateNULDiscardsEarlierMatch(t *testing.T) {
	data := append([]byte("needle\n"), bytes.Repeat([]byte{'x'}, binaryProbeSize)...)
	data = append(data, 0, '\n')
	remote := &fakeProvider{
		entries:    []provider.Entry{{Path: "late.bin", OID: "late", Mode: "100644"}},
		blobs:      map[string][]byte{"late": data},
		candidates: []string{"late.bin"},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var events []Event
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeIndexed, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if summary.MatchedLines != 0 || summary.ScannedFiles != 1 || summary.SkippedBinary != 1 {
		t.Fatalf("summary = %#v, want one discarded binary scan and no match", summary)
	}
	for _, event := range events {
		if event.Type == "match" {
			t.Fatalf("events = %#v, want no match event after late NUL", events)
		}
	}
}

func TestRunnerAutoWithoutLiteralSkipsIndexButRequiresCompleteTree(t *testing.T) {
	remote := &fakeProvider{
		entries: []provider.Entry{{Path: "a.txt", OID: "a"}},
		blobs:   map[string][]byte{"a": []byte("needle\n")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: `.*needle`})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != 1 {
		t.Fatalf("summary = %#v, want complete archive scan", summary)
	}
	if remote.searchCount() != 0 || remote.listCount() != 1 || remote.archiveCount() != 1 || !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) {
		t.Fatalf("Search/List/Archive calls = %d/%d/%d/%v, want 0/1/1/[true]", remote.searchCount(), remote.listCount(), remote.archiveCount(), remote.listRequireCompleteCopy())
	}
}

func TestRunnerAutoSkipsOptionalIndexBelowHeadroomBoundary(t *testing.T) {
	remote := &fakeProvider{
		entries:    []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:      map[string][]byte{"a": []byte("needle a\n"), "b": []byte("needle b\n")},
		candidates: []string{"a.txt"},
		stats:      provider.RequestStats{Requests: 2, RequestLimit: 40},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1, RequestLimit: 40}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !summary.Complete || summary.Transport != "archive" || summary.MatchedLines != 2 {
		t.Fatalf("summary = %#v, want complete archive scan with both matches", summary)
	}
	if remote.searchCount() != 0 || remote.archiveCount() != 1 {
		t.Fatalf("SearchCandidates/OpenArchive calls = %d/%d, want 0/1", remote.searchCount(), remote.archiveCount())
	}
}

func TestRunnerAutoRejectsIncompleteTreeManifest(t *testing.T) {
	remote := &fakeProvider{
		entries:     []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:       map[string][]byte{"a": []byte("needle a\n"), "b": []byte("needle b\n")},
		candidates:  []string{"a.txt"},
		treePartial: true,
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err == nil || ErrorCode(err) != "list_tree" || summary.Complete || summary.Reason != "tree_error" {
		t.Fatalf("summary/error = %#v/%v, want incomplete tree error", summary, err)
	}
	if !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) || remote.archiveCount() != 0 || remote.openCount() != 0 {
		t.Fatalf("ListTree/Archive/Blob calls = %v/%d/%d, want [true]/0/0", remote.listRequireCompleteCopy(), remote.archiveCount(), remote.openCount())
	}
}

func TestRunnerAutoPrefetchesAtMost16UniqueCandidatesInStableOrder(t *testing.T) {
	entries := make([]provider.Entry, 24)
	blobs := make(map[string][]byte, len(entries))
	candidates := make([]string, 0, 22)
	for i := range entries {
		path := fmt.Sprintf("file%02d.txt", i)
		oid := fmt.Sprintf("oid-%02d", i)
		entries[i] = provider.Entry{Path: path, OID: oid}
		blobs[oid] = []byte("needle\n")
		if i >= 4 {
			candidates = append(candidates, path)
		}
	}
	// Reverse the index order and add duplicates: orderCandidates must still
	// produce one stable, path-sorted prefetch batch.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] > candidates[j] })
	candidates = append(candidates, "file04.txt", "file23.txt")
	remote := &fakeProvider{entries: entries, blobs: blobs, candidates: candidates}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1}}
	var matchPaths []string
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		if event.Type == "match" {
			matchPaths = append(matchPaths, event.Path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantPrefetch := make([]string, 16)
	for i := range wantPrefetch {
		wantPrefetch[i] = fmt.Sprintf("file%02d.txt", i+4)
	}
	if !reflect.DeepEqual(remote.blobPathsCopy(), wantPrefetch) {
		t.Fatalf("prefetched blob paths = %v, want %v", remote.blobPathsCopy(), wantPrefetch)
	}
	wantMatchPaths := append(append([]string{}, wantPrefetch...), "file00.txt", "file01.txt", "file02.txt", "file03.txt", "file20.txt", "file21.txt", "file22.txt", "file23.txt")
	if !reflect.DeepEqual(matchPaths, wantMatchPaths) {
		t.Fatalf("match paths = %v, want %v", matchPaths, wantMatchPaths)
	}
	if !summary.Complete || summary.ScannedFiles != 24 || remote.archiveCount() != 1 {
		t.Fatalf("summary/archive calls = %#v/%d, want complete 24-file archive completion", summary, remote.archiveCount())
	}
}

func TestRunnerAutoPrefetchShrinksWithFiniteRequestBudget(t *testing.T) {
	for _, tt := range []struct {
		name         string
		requests     int
		requestLimit int
		wantPrefetch int
	}{
		{name: "forty-two remaining", requests: 8, requestLimit: 50, wantPrefetch: 12},
		{name: "forty-five remaining", requests: 5, requestLimit: 50, wantPrefetch: 13},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const candidateCount = 20
			entries := make([]provider.Entry, candidateCount)
			blobs := make(map[string][]byte, candidateCount)
			candidates := make([]string, candidateCount)
			for index := range entries {
				path := fmt.Sprintf("file-%02d.txt", index)
				oid := fmt.Sprintf("oid-%02d", index)
				entries[index] = provider.Entry{Path: path, OID: oid}
				blobs[oid] = []byte("needle\n")
				candidates[index] = path
			}
			remote := &fakeProvider{
				entries:    entries,
				blobs:      blobs,
				candidates: candidates,
				stats:      provider.RequestStats{Requests: tt.requests, RequestLimit: tt.requestLimit},
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1, RequestLimit: tt.requestLimit}}
			summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if !summary.Complete || summary.MatchedLines != candidateCount {
				t.Fatalf("summary = %#v, want complete scan of all candidates", summary)
			}
			if got := len(remote.blobPathsCopy()); got != tt.wantPrefetch {
				t.Fatalf("prefetched blobs = %d, want %d for finite request budget", got, tt.wantPrefetch)
			}
			if remote.searchCount() != 1 || remote.listCount() != 1 || remote.archiveCount() != 1 {
				t.Fatalf("Search/List/Archive calls = %d/%d/%d, want 1/1/1", remote.searchCount(), remote.listCount(), remote.archiveCount())
			}
		})
	}
}

func TestRunnerCandidatePrefetchUsesWorkerBoundedConcurrency(t *testing.T) {
	const workers = 3
	entries := make([]provider.Entry, 4)
	blobs := make(map[string][]byte, len(entries))
	candidates := make([]string, 0, len(entries))
	for i := range entries {
		path := fmt.Sprintf("file%02d.txt", i)
		oid := fmt.Sprintf("oid-%02d", i)
		entries[i] = provider.Entry{Path: path, OID: oid}
		blobs[oid] = []byte("needle\n")
		candidates = append(candidates, path)
	}
	startGate := make(chan struct{})
	slowRelease := make(chan struct{})
	started := make(chan string, len(entries))
	completed := make(chan string, len(entries))
	remote := &fakeProvider{
		entries:      entries,
		blobs:        blobs,
		candidates:   candidates,
		blobGate:     startGate,
		blobBlocks:   map[string]<-chan struct{}{"file00.txt": slowRelease},
		blobStarted:  started,
		blobComplete: completed,
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: workers}}
	type result struct {
		summary Summary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
		done <- result{summary: summary, err: err}
	}()
	for i := 0; i < workers; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial candidate workers")
		}
	}
	if got := remote.maxActiveBlobCount(); got != workers {
		t.Fatalf("initial active OpenBlob calls = %d, want %d concurrent workers", got, workers)
	}
	close(startGate)
	for i := 0; i < workers-1; i++ {
		select {
		case path := <-completed:
			if path == "file00.txt" {
				t.Fatalf("slow first candidate completed before its release")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for non-first candidate completion")
		}
	}
	if got := remote.openCount(); got != workers {
		t.Fatalf("OpenBlob calls while first candidate is blocked = %d, want %d", got, workers)
	}
	close(slowRelease)
	select {
	case result := <-done:
		if result.err != nil || !result.summary.Complete || result.summary.MatchedLines != len(entries) {
			t.Fatalf("summary/error = %#v/%v, want complete candidate scan", result.summary, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bounded candidate scan")
	}
	if got := remote.maxActiveBlobCount(); got > workers {
		t.Fatalf("maximum active OpenBlob calls = %d, exceeds workers=%d", got, workers)
	}
}

func TestRunnerAutoPrefetchFailureIsRetriedByCompleteArchive(t *testing.T) {
	remote := &fakeProvider{
		entries:    []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:      map[string][]byte{"a": []byte("needle a\n"), "b": []byte("needle b\n")},
		candidates: []string{"a.txt"},
		blobErrs:   map[string]error{"a.txt": errors.New("prefetch blob unavailable")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var warnings []Event
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		if event.Type == "warning" {
			warnings = append(warnings, event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !summary.Complete || summary.MatchedLines != 2 || summary.Transport != "archive" {
		t.Fatalf("summary = %#v, want complete archive retry", summary)
	}
	if remote.openCount() != 1 || remote.archiveCount() != 1 {
		t.Fatalf("OpenBlob/OpenArchive calls = %d/%d, want 1/1", remote.openCount(), remote.archiveCount())
	}
	if !hasEventCode(warnings, "candidate_prefetch_failed") {
		t.Fatalf("warnings = %#v, want candidate_prefetch_failed", warnings)
	}
}

func TestRunnerCorruptCachedArchiveRefreshesDuringSameRun(t *testing.T) {
	objectCache := newTestCache(t)
	snapshot := testSnapshot()
	archiveKey := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
	stored, _, err := objectCache.Put(archiveKey, bytes.NewReader([]byte("not a gzip stream")))
	if err != nil {
		t.Fatalf("seed invalid archive cache: %v", err)
	}
	stored.Close()
	remote := &fakeProvider{
		entries: []provider.Entry{{Path: "a.txt", OID: "a"}},
		blobs:   map[string][]byte{"a": []byte("needle\n")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var warnings []Event
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
	summary, err := runner.Run(context.Background(), snapshot, func(event Event) error {
		if event.Type == "warning" {
			warnings = append(warnings, event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !summary.Complete || summary.CacheHits != 0 || remote.archiveCount() != 1 {
		t.Fatalf("summary/archive calls = %#v/%d, want refreshed archive cache", summary, remote.archiveCount())
	}
	if !hasEventCode(warnings, "cache_read_failed") {
		t.Fatalf("warnings = %#v, want cache_read_failed", warnings)
	}
	reader, hit, err := objectCache.Open(archiveKey)
	if err != nil || !hit {
		t.Fatalf("refreshed archive cache = hit %v, error %v", hit, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close refreshed archive cache: %v", err)
	}
}

func TestRunnerDeepCorruptCachedArchiveIsRemoved(t *testing.T) {
	objectCache := newTestCache(t)
	snapshot := testSnapshot()
	archiveKey := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
	corrupt := makeTestArchive(t, testArchiveFile{name: "owner-repo-commit/a.txt", data: []byte("needle\n")})
	corrupt[len(corrupt)-1] ^= 0xff
	stored, _, err := objectCache.Put(archiveKey, bytes.NewReader(corrupt))
	if err != nil {
		t.Fatalf("seed corrupt archive cache: %v", err)
	}
	stored.Close()
	remote := &fakeProvider{
		entries: []provider.Entry{{Path: "a.txt", OID: "a", Mode: "100644"}},
		blobs:   map[string][]byte{"a": []byte("needle\n")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
	summary, err := runner.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err == nil || ErrorCode(err) != "read_archive" || summary.Complete || summary.Reason != "archive_error" {
		t.Fatalf("summary/error = %#v/%v, want deep archive checksum failure", summary, err)
	}
	if summary.CacheHits != 0 || remote.archiveCount() != 0 {
		t.Fatalf("cache/archive counters = %d/%d, want invalid cache removed without remote refresh", summary.CacheHits, remote.archiveCount())
	}
	if _, hit, err := objectCache.Open(archiveKey); err != nil || hit {
		t.Fatalf("corrupt archive cache = hit %v, error %v; want removed", hit, err)
	}
}

func TestRunnerExactRejectsIncompleteTreeManifest(t *testing.T) {
	remote := &fakeProvider{
		entries:     []provider.Entry{{Path: "a.txt", OID: "a"}},
		blobs:       map[string][]byte{"a": []byte("needle\n")},
		archiveErr:  errors.New("archive endpoint unavailable"),
		treePartial: true,
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err == nil || ErrorCode(err) != "list_tree" || summary.Complete || summary.Reason != "tree_error" {
		t.Fatalf("summary/error = %#v/%v, want list_tree tree_error", summary, err)
	}
	if remote.archiveCount() != 0 || remote.openCount() != 0 || !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) {
		t.Fatalf("Archive/Blob/ListTree calls = %d/%d/%v, want 0/0/[true]", remote.archiveCount(), remote.openCount(), remote.listRequireCompleteCopy())
	}
}

func TestRunnerArchiveRejectsUnsafeOutOfOrderAndDuplicatePaths(t *testing.T) {
	tests := []struct {
		name         string
		files        []testArchiveFile
		wantFallback bool
		wantSuccess  bool
	}{
		{
			name:         "unsafe path",
			files:        []testArchiveFile{{name: "owner-repo-commit/../escape.txt", data: []byte("needle\n")}},
			wantFallback: true,
		},
		{
			name:        "out of order",
			files:       []testArchiveFile{{name: "owner-repo-commit/z.txt", data: []byte("z\n")}, {name: "owner-repo-commit/a.txt", data: []byte("a\n")}},
			wantSuccess: true,
		},
		{
			name:  "duplicate",
			files: []testArchiveFile{{name: "owner-repo-commit/a.txt", data: []byte("a\n")}, {name: "owner-repo-commit/a.txt", data: []byte("a again\n")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := &fakeProvider{
				entries: []provider.Entry{
					{Path: "a.txt", OID: "a", Mode: "100644"},
					{Path: "z.txt", OID: "z", Mode: "100644"},
				},
				blobs: map[string][]byte{
					"a": []byte("needle a\n"),
					"z": []byte("needle z\n"),
				},
				archiveData: makeTestArchive(t, tt.files...),
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
			summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
			if tt.wantFallback {
				if err != nil || !summary.Complete || summary.Transport != "blob_fallback" {
					t.Fatalf("summary/error = %#v/%v, want complete blob fallback", summary, err)
				}
				return
			}
			if tt.wantSuccess {
				if err != nil || !summary.Complete || summary.Transport != "archive" {
					t.Fatalf("summary/error = %#v/%v, want successful archive scan", summary, err)
				}
				return
			}
			if err == nil || ErrorCode(err) != "read_archive" || summary.Complete || summary.Reason != "archive_error" {
				t.Fatalf("summary/error = %#v/%v, want read_archive archive_error", summary, err)
			}
		})
	}
}

func TestRunnerAutoFallsBackWhenIndexUnavailable(t *testing.T) {
	remote := &fakeProvider{
		entries:   []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:     map[string][]byte{"a": []byte("needle\n"), "b": []byte("needle too\n")},
		searchErr: errors.New("index offline"),
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var events []Event
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if summary.MatchedLines != 2 || !summary.Complete || summary.Reason != "" {
		t.Fatalf("summary = %#v, want complete fallback", summary)
	}
	if remote.searchCount() != 1 {
		t.Fatalf("SearchCandidates calls = %d, want 1", remote.searchCount())
	}
	if len(events) == 0 || events[0].Type != "warning" || events[0].Code != "index_unavailable" {
		t.Fatalf("first events = %#v, want index_unavailable warning", events)
	}
}

func TestRunnerIndexedModeUsesOnlyCandidatesAndReportsIncompleteSummary(t *testing.T) {
	remote := &fakeProvider{
		entries:    []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:      map[string][]byte{"a": []byte("needle a\n"), "b": []byte("needle b\n"), "not-in-tree": []byte("needle external\n")},
		candidates: []string{"b.txt", "not-in-tree", "b.txt"},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeIndexed, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if summary.MatchedLines != 2 || summary.ScannedFiles != 2 || summary.Complete || summary.Reason != "indexed_mode" {
		t.Fatalf("summary = %#v, want two indexed candidates and indexed_mode", summary)
	}
	if got := remote.openCount(); got != 2 || remote.listCount() != 0 {
		t.Fatalf("OpenBlob/ListTree calls = %d/%d, want 2/0", got, remote.listCount())
	}
}

func TestRunnerIndexedFiltersAndSortsCandidatesWithoutTreeManifest(t *testing.T) {
	remote := &fakeProvider{
		blobs: map[string][]byte{
			"a.go":      []byte("needle a\n"),
			"z.go":      []byte("needle z\n"),
			"README.md": []byte("needle readme\n"),
		},
		candidates: []string{"z.go", "README.md", "a.go", "z.go"},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var matches []string
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeIndexed, Workers: 1, Globs: []string{"*.go"}}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		if event.Type == "match" {
			matches = append(matches, event.Path)
		}
		return nil
	})
	if err != nil || summary.MatchedLines != 2 || summary.ScannedFiles != 2 {
		t.Fatalf("summary/error = %#v/%v, want two filtered candidates", summary, err)
	}
	if !reflect.DeepEqual(remote.blobPathsCopy(), []string{"a.go", "z.go"}) || !reflect.DeepEqual(matches, []string{"a.go", "z.go"}) {
		t.Fatalf("blob/match paths = %v/%v, want sorted go candidates", remote.blobPathsCopy(), matches)
	}
	if remote.listCount() != 0 {
		t.Fatalf("ListTree calls = %d, want zero for indexed mode", remote.listCount())
	}
}

func TestRunnerIndexedRejectsUnsafeCandidatePath(t *testing.T) {
	remote := &fakeProvider{candidates: []string{"../escape.go"}}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeIndexed, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err == nil || ErrorCode(err) != "indexed_search_failed" || summary.Complete || summary.Reason != "index_error" {
		t.Fatalf("summary/error = %#v/%v, want invalid candidate path failure", summary, err)
	}
	if remote.listCount() != 0 || remote.openCount() != 0 {
		t.Fatalf("ListTree/OpenBlob calls = %d/%d, want zero after candidate validation", remote.listCount(), remote.openCount())
	}
}

func TestRunnerLateInvalidUTF8DiscardsEarlierMatch(t *testing.T) {
	data := append([]byte("needle\n"), bytes.Repeat([]byte{'x'}, binaryProbeSize)...)
	data = append(data, 0xff, '\n')
	remote := &fakeProvider{
		blobs:      map[string][]byte{"invalid.txt": data},
		candidates: []string{"invalid.txt"},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var events []Event
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeIndexed, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil || summary.MatchedLines != 0 || summary.SkippedBinary != 1 || summary.ScannedFiles != 1 {
		t.Fatalf("summary/error = %#v/%v, want invalid UTF-8 skipped without match", summary, err)
	}
	for _, event := range events {
		if event.Type == "match" || event.Type == "context" {
			t.Fatalf("events = %#v, want no events from invalid UTF-8 file", events)
		}
	}
}

func TestRunnerUnlimitedMatchesSpoolAndCleanTemporaryEvents(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	const lines = 1024
	remote := &fakeProvider{
		blobs:      map[string][]byte{"many.txt": bytes.Repeat([]byte("needle\n"), lines)},
		candidates: []string{"many.txt"},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	events := 0
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeIndexed, MaxResults: 0, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		if event.Type == "match" {
			events++
		}
		return nil
	})
	if err != nil || summary.MatchedLines != lines || summary.Truncated || events != lines {
		t.Fatalf("summary/error/events = %#v/%v/%d, want unlimited %d matches", summary, err, events, lines)
	}
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("ReadDir(temp) error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "git-rg-events-") {
			t.Fatalf("event spool %q remains after Run", entry.Name())
		}
	}
}

func TestScanReaderSmallResultsStayInMemory(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Matcher: matcher, Config: Config{Workers: 1}}
	outcome := runner.scanReader(context.Background(), "small.txt", strings.NewReader("needle\n"), false, 0)
	defer cleanupOutcome(outcome)
	if outcome.err != nil || outcome.spoolPath != "" || len(outcome.result.Events) != 1 {
		t.Fatalf("small outcome = %#v, want in-memory event and no spool file", outcome)
	}
	if outcome.result.Events[0].Type != "match" || outcome.result.Events[0].Text != "needle" {
		t.Fatalf("small events = %#v, want one match event", outcome.result.Events)
	}
}

func TestScanReaderLargeResultsSpillAndCleanup(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle", Before: 1, After: 1})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	data := bytes.Repeat([]byte("before\nneedle\nafter\n"), 400)
	want, err := matcher.Scan("large.txt", bytes.NewReader(data), 0)
	if err != nil {
		t.Fatalf("Matcher.Scan() error = %v", err)
	}
	runner := &Runner{Matcher: matcher, Config: Config{Workers: 1}}
	outcome := runner.scanReader(context.Background(), "large.txt", bytes.NewReader(data), false, 0)
	spoolPath := outcome.spoolPath
	if outcome.err != nil || spoolPath == "" || len(outcome.result.Events) != 0 {
		cleanupOutcome(outcome)
		t.Fatalf("large outcome = %#v, want disk spill without retained in-memory events", outcome)
	}
	if _, err := os.Stat(spoolPath); err != nil {
		cleanupOutcome(outcome)
		t.Fatalf("large spool stat error = %v, want existing spill file", err)
	}
	var got []Event
	var summary Summary
	_, err = runner.consumeOutcome(context.Background(), outcome, &summary, make(map[string]struct{}), func(event Event) error {
		got = append(got, event)
		return nil
	}, "read_result_spool", "spool_error")
	if err != nil {
		t.Fatalf("consumeOutcome() error = %v", err)
	}
	if !reflect.DeepEqual(got, want.Events) || summary.MatchedLines != want.Matches {
		t.Fatalf("spilled events/summary = %#v/%#v, want %#v/%d", got, summary, want.Events, want.Matches)
	}
	if _, err := os.Stat(spoolPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("large spool after cleanup error = %v, want removed file", err)
	}
}

func TestScanReaderLateNULAbortsSpilledResults(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	data := []byte(strings.Repeat("needle\n", int(maxMemorySpoolBytes/512)+32))
	data = append(data, bytes.Repeat([]byte{'x'}, binaryProbeSize)...)
	data = append(data, 0, '\n')
	runner := &Runner{Matcher: matcher, Config: Config{Workers: 1}}
	outcome := runner.scanReader(context.Background(), "late-large.bin", bytes.NewReader(data), false, 0)
	defer cleanupOutcome(outcome)
	if outcome.err != nil || !outcome.skippedBinary || outcome.spoolPath != "" || len(outcome.result.Events) != 0 {
		t.Fatalf("late binary outcome = %#v, want discarded spilled results", outcome)
	}
}

func TestRunnerResultLimitTruncatesGlobalOutput(t *testing.T) {
	remote := &fakeProvider{
		entries: []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:   map[string][]byte{"a": []byte("needle\n"), "b": []byte("needle\n")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var matches int
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, MaxResults: 1, Workers: 2}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		if event.Type == "match" {
			matches++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if matches != 1 || summary.MatchedLines != 1 || !summary.Truncated || summary.Complete || summary.Reason != "result_limit" {
		t.Fatalf("matches/summary = %d/%#v", matches, summary)
	}
}

func TestRunnerReusesArchiveCache(t *testing.T) {
	objectCache := newTestCache(t)
	remote := &fakeProvider{entries: []provider.Entry{{Path: "a.txt", OID: "a"}}, blobs: map[string][]byte{"a": []byte("needle\n")}}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	config := Config{Mode: ModeExact, Workers: 1}
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: config}
	first, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if first.CacheHits != 0 || remote.listCount() != 1 || remote.openCount() != 0 || remote.archiveCount() != 1 {
		t.Fatalf("first summary/calls = %#v/%d/%d/%d", first, remote.listCount(), remote.openCount(), remote.archiveCount())
	}
	if remote.listCount() != 1 || !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) {
		t.Fatalf("first ListTree calls = %d/%v, want one complete tree request", remote.listCount(), remote.listRequireCompleteCopy())
	}
	second, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if second.CacheHits != 2 || remote.listCount() != 1 || remote.openCount() != 0 || remote.archiveCount() != 1 {
		t.Fatalf("second summary/calls = %#v/%d/%d/%d, want tree+archive cache reuse", second, remote.listCount(), remote.openCount(), remote.archiveCount())
	}
	if second.Transport != "archive" {
		t.Fatalf("second transport = %q, want archive", second.Transport)
	}
}

func TestRunnerAutoHotArchiveCacheSkipsIndexAfterCompleteTreeCache(t *testing.T) {
	objectCache := newTestCache(t)
	remote := &fakeProvider{
		entries: []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs: map[string][]byte{
			"a": []byte("needle a\n"),
			"b": []byte("needle b\n"),
		},
		candidates: []string{"a.txt"},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeAuto, Workers: 1}}
	first, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil || !first.Complete || first.MatchedLines != 2 {
		t.Fatalf("first summary/error = %#v/%v, want complete auto scan", first, err)
	}
	if remote.searchCount() != 1 || remote.listCount() != 1 || remote.archiveCount() != 1 {
		t.Fatalf("first Search/List/Archive calls = %d/%d/%d, want 1/1/1", remote.searchCount(), remote.listCount(), remote.archiveCount())
	}
	second, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err != nil || !second.Complete || second.MatchedLines != 2 {
		t.Fatalf("second summary/error = %#v/%v, want complete hot-cache auto scan", second, err)
	}
	if second.CacheHits != 2 || remote.searchCount() != 1 || remote.listCount() != 1 || remote.archiveCount() != 1 {
		t.Fatalf("second summary/calls = %#v/%d/%d/%d, want tree+archive cache and no index", second, remote.searchCount(), remote.listCount(), remote.archiveCount())
	}
}

func TestRunnerCachedArchiveCancellationMarksSummaryIncompleteAndKeepsCache(t *testing.T) {
	for _, tt := range []struct {
		name         string
		cancelBefore bool
	}{
		{name: "cancel before run", cancelBefore: true},
		{name: "cancel while emitting", cancelBefore: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objectCache := newTestCache(t)
			snapshot := testSnapshot()
			archiveKey := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
			stored, _, err := objectCache.Put(archiveKey, bytes.NewReader(makeTestArchive(t, testArchiveFile{
				name: "owner-repo-commit/a.txt",
				data: []byte("needle\n"),
			})))
			if err != nil {
				t.Fatalf("seed archive cache error = %v", err)
			}
			stored.Close()
			remote := &fakeProvider{
				entries: []provider.Entry{{Path: "a.txt", OID: "a", Mode: "100644"}},
				blobs:   map[string][]byte{"a": []byte("needle\n")},
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelBefore {
				cancel()
			}
			runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
			summary, err := runner.Run(ctx, snapshot, func(event Event) error {
				if !tt.cancelBefore && event.Type == "match" {
					cancel()
				}
				return nil
			})
			if err == nil || !errors.Is(err, context.Canceled) || summary.Complete || summary.Reason != "cancelled" {
				t.Fatalf("summary/error = %#v/%v, want cancelled incomplete cached scan", summary, err)
			}
			if remote.archiveCount() != 0 {
				t.Fatalf("OpenArchive calls = %d, want zero for cached archive", remote.archiveCount())
			}
			reader, hit, err := objectCache.Open(archiveKey)
			if err != nil || !hit {
				t.Fatalf("cached archive after cancellation = hit %v, error %v; want preserved cache", hit, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("close cached archive after cancellation: %v", err)
			}
		})
	}
}

func TestRunnerExactResultLimitDoesNotCommitPartialArchive(t *testing.T) {
	objectCache := newTestCache(t)
	snapshot := testSnapshot()
	remote := &fakeProvider{
		entries: []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:   map[string][]byte{"a": []byte("needle\n"), "b": []byte("needle\n")},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, MaxResults: 1, Workers: 1}}
	summary, err := runner.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !summary.Truncated || summary.Reason != "result_limit" || summary.Transport != "archive" {
		t.Fatalf("summary = %#v, want truncated archive result", summary)
	}
	if remote.archiveCount() != 1 || remote.openCount() != 0 {
		t.Fatalf("archive/blob calls = %d/%d, want 1/0", remote.archiveCount(), remote.openCount())
	}
	archiveKey := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
	if _, hit, err := objectCache.Open(archiveKey); err != nil || hit {
		t.Fatalf("partial archive cache = hit %v, error %v; want no committed archive", hit, err)
	}
}

func TestRunnerExactFallsBackToBlobRequestsWhenArchiveUnavailable(t *testing.T) {
	remote := &fakeProvider{
		entries:    []provider.Entry{{Path: "a.txt", OID: "a"}, {Path: "b.txt", OID: "b"}},
		blobs:      map[string][]byte{"a": []byte("needle\n"), "b": []byte("needle too\n")},
		archiveErr: errors.New("archive endpoint unavailable"),
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	var warnings []Event
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
		if event.Type == "warning" {
			warnings = append(warnings, event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if summary.MatchedLines != 2 || !summary.Complete || summary.Transport != "blob_fallback" {
		t.Fatalf("summary = %#v, want complete blob fallback", summary)
	}
	if remote.archiveCount() != 1 || remote.openCount() != 2 {
		t.Fatalf("archive/blob calls = %d/%d, want 1/2", remote.archiveCount(), remote.openCount())
	}
	if remote.listCount() != 1 || !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) {
		t.Fatalf("ListTree calls = %d/%v, want one complete manifest request", remote.listCount(), remote.listRequireCompleteCopy())
	}
	if len(warnings) != 1 || warnings[0].Code != "archive_unavailable" {
		t.Fatalf("warnings = %#v, want archive_unavailable", warnings)
	}
}

func TestRunnerExactFallsBackForInvalidArchiveResponses(t *testing.T) {
	unsafeFirstHeader := makeTestArchive(t, testArchiveFile{name: "owner-repo-commit/../escape.txt", data: []byte("bad\n")})
	tests := []struct {
		name        string
		archiveData []byte
	}{
		{name: "non gzip response", archiveData: []byte("not a gzip stream")},
		{name: "invalid first tar header", archiveData: unsafeFirstHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := &fakeProvider{
				entries: []provider.Entry{
					{Path: "a.txt", OID: "a", Mode: "100644"},
					{Path: "b.txt", OID: "b", Mode: "100644"},
				},
				blobs:       map[string][]byte{"a": []byte("needle a\n"), "b": []byte("needle b\n")},
				archiveData: tt.archiveData,
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			var warnings []Event
			runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
			summary, err := runner.Run(context.Background(), testSnapshot(), func(event Event) error {
				if event.Type == "warning" {
					warnings = append(warnings, event)
				}
				return nil
			})
			if err != nil || !summary.Complete || summary.Transport != "blob_fallback" || summary.MatchedLines != 2 || summary.ScannedFiles != 2 {
				t.Fatalf("summary/error = %#v/%v, want complete two-file blob fallback", summary, err)
			}
			if remote.archiveCount() != 1 || remote.openCount() != 2 || remote.listCount() != 1 || !reflect.DeepEqual(remote.listRequireCompleteCopy(), []bool{true}) {
				t.Fatalf("Archive/Blob/ListTree calls = %d/%d/%d/%v, want 1/2/1/[true]", remote.archiveCount(), remote.openCount(), remote.listCount(), remote.listRequireCompleteCopy())
			}
			if !hasEventCode(warnings, "archive_unavailable") {
				t.Fatalf("warnings = %#v, want archive_unavailable", warnings)
			}
		})
	}
}

func TestRunnerInvalidTreeCacheRefreshesFromProvider(t *testing.T) {
	oid := testGitBlobOID([]byte("needle\n"))
	validA := `{"path":"a.txt","oid":"` + oid + `","mode":"100644"}`
	validB := `{"path":"b.txt","oid":"` + oid + `","mode":"100644"}`
	tests := []struct {
		name   string
		cached []byte
	}{
		{name: "trailing JSON", cached: []byte(`[` + validA + `] {}`)},
		{name: "unsafe unselected path", cached: []byte(`[` + validA + `,{"path":"../escape","oid":"` + oid + `","mode":"100644"}]`)},
		{name: "duplicate unselected path", cached: []byte(`[` + validA + `,` + validB + `,` + validB + `]`)},
		{name: "unordered unselected path", cached: []byte(`[` + validB + `,` + validA + `]`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objectCache := newTestCache(t)
			snapshot := testSnapshot()
			treeKey := cache.Key(snapshot.Repository.CacheNamespace(), "tree", snapshot.Commit)
			stored, _, err := objectCache.Put(treeKey, bytes.NewReader(tt.cached))
			if err != nil {
				t.Fatalf("seed invalid tree cache: %v", err)
			}
			stored.Close()
			remote := &fakeProvider{
				entries:    []provider.Entry{{Path: "a.txt", OID: "a", Mode: "100644"}},
				blobs:      map[string][]byte{"a": []byte("needle\n")},
				archiveErr: errors.New("archive unavailable"),
			}
			matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			var warnings []Event
			runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1, Globs: []string{"a.txt"}}}
			summary, err := runner.Run(context.Background(), snapshot, func(event Event) error {
				if event.Type == "warning" {
					warnings = append(warnings, event)
				}
				return nil
			})
			if err != nil || !summary.Complete || summary.MatchedLines != 1 || summary.CacheHits != 0 || summary.Transport != "blob_fallback" || remote.listCount() != 1 {
				t.Fatalf("summary/error/list calls = %#v/%v/%d, want refreshed tree and one complete fallback match", summary, err, remote.listCount())
			}
			if !hasEventCode(warnings, "cache_read_failed") {
				t.Fatalf("warnings = %#v, want cache_read_failed", warnings)
			}
			if reader, hit, err := objectCache.Open(treeKey); err != nil || !hit {
				t.Fatalf("refreshed tree cache = hit %v, error %v; want hit", hit, err)
			} else {
				reader.Close()
			}
		})
	}
}

func TestRunnerValidTreeCacheHitCountsInSummary(t *testing.T) {
	objectCache := newTestCache(t)
	snapshot := testSnapshot()
	treeKey := cache.Key(snapshot.Repository.CacheNamespace(), "tree", snapshot.Commit)
	encoded, err := json.Marshal([]provider.Entry{{Path: "a.txt", OID: testGitBlobOID([]byte("needle\n")), Mode: "100644"}})
	if err != nil {
		t.Fatalf("json.Marshal(tree) error = %v", err)
	}
	stored, _, err := objectCache.Put(treeKey, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("seed tree cache: %v", err)
	}
	stored.Close()
	remote := &fakeProvider{
		entries:    []provider.Entry{{Path: "a.txt", OID: "a", Mode: "100644"}},
		blobs:      map[string][]byte{"a": []byte("needle\n")},
		archiveErr: errors.New("archive unavailable"),
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1}}
	summary, err := runner.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || !summary.Complete || summary.MatchedLines != 1 || summary.CacheHits != 1 || summary.Transport != "blob_fallback" || remote.listCount() != 0 {
		t.Fatalf("summary/error/list calls = %#v/%v/%d, want one tree cache hit", summary, err, remote.listCount())
	}
}

func TestRunnerLargeTreeManifestIsPersistedAndHotHitFiltersBeforeProvider(t *testing.T) {
	const entryCount = 200_000
	selectedData := []byte("needle selected\n")
	selectedOID := testGitBlobOID(selectedData)
	entries := make([]provider.Entry, entryCount)
	for i := range entries {
		entries[i] = provider.Entry{
			Path: fmt.Sprintf("pkg/file-%06d.txt", i),
			OID:  selectedOID,
			Mode: "100644",
			Size: int64(len(selectedData)),
		}
	}
	targetPath := entries[0].Path
	snapshot := testSnapshot()
	objectCache := newTestCache(t)
	blobKey := cache.Key(snapshot.Repository.CacheNamespace(), "blob", selectedOID)
	stored, _, err := objectCache.Put(blobKey, bytes.NewReader(selectedData))
	if err != nil {
		t.Fatalf("seed selected blob cache: %v", err)
	}
	stored.Close()

	coldProvider := &fakeProvider{
		entries: entries,
		blobs:   map[string][]byte{selectedOID: selectedData},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	config := Config{Mode: ModeExact, Workers: 1, Globs: []string{targetPath}}
	cold := &Runner{Provider: coldProvider, Cache: objectCache, Matcher: matcher, Config: config}
	coldSummary, err := cold.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || !coldSummary.Complete || coldSummary.MatchedLines != 1 || coldSummary.MatchedFiles != 1 || coldSummary.ScannedFiles != 1 {
		t.Fatalf("cold summary/error = %#v/%v, want one complete cached-blob match", coldSummary, err)
	}
	if coldProvider.listCount() != 1 || coldProvider.openCount() != 0 || coldSummary.CacheHits != 1 {
		t.Fatalf("cold provider/cache = list %d, blob %d, cache hits %d; want 1/0/1", coldProvider.listCount(), coldProvider.openCount(), coldSummary.CacheHits)
	}

	treeKey := cache.Key(snapshot.Repository.CacheNamespace(), "tree", snapshot.Commit)
	treeReader, hit, err := objectCache.Open(treeKey)
	if err != nil || !hit {
		t.Fatalf("persisted tree cache = hit %v, error %v; want hit", hit, err)
	}
	persisted, err := io.ReadAll(treeReader)
	treeReader.Close()
	if err != nil {
		t.Fatalf("read persisted tree cache: %v", err)
	}
	if len(persisted) < 20<<20 || len(persisted) >= maxTreeManifestSize {
		t.Fatalf("persisted tree JSON size = %d, want roughly 21 MiB and below %d", len(persisted), maxTreeManifestSize)
	}

	hotProvider := &fakeProvider{}
	hot := &Runner{Provider: hotProvider, Cache: objectCache, Matcher: matcher, Config: config}
	hotSummary, err := hot.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || !hotSummary.Complete || hotSummary.MatchedLines != 1 || hotSummary.MatchedFiles != 1 || hotSummary.ScannedFiles != 1 {
		t.Fatalf("hot summary/error = %#v/%v, want one complete hot-cache match", hotSummary, err)
	}
	if hotSummary.CacheHits != 2 || hotProvider.listCount() != 0 || hotProvider.openCount() != 0 || hotProvider.archiveCount() != 0 || hotSummary.APIRequests != 0 {
		t.Fatalf("hot provider/cache = %#v, want tree+blob hits and zero provider requests", hotSummary)
	}
}

func TestRunnerEmptyTreeManifestRoundTripsThroughCache(t *testing.T) {
	snapshot := testSnapshot()
	objectCache := newTestCache(t)
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	config := Config{Mode: ModeExact, Workers: 1}
	coldProvider := &fakeProvider{}
	cold := &Runner{Provider: coldProvider, Cache: objectCache, Matcher: matcher, Config: config}
	coldSummary, err := cold.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || !coldSummary.Complete || coldSummary.ScannedFiles != 0 || coldSummary.CacheHits != 0 {
		t.Fatalf("cold summary/error = %#v/%v, want complete empty-tree scan", coldSummary, err)
	}
	if coldProvider.listCount() != 1 {
		t.Fatalf("cold ListTree calls = %d, want one provider refresh", coldProvider.listCount())
	}

	hotProvider := &fakeProvider{}
	hot := &Runner{Provider: hotProvider, Cache: objectCache, Matcher: matcher, Config: config}
	hotSummary, err := hot.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || !hotSummary.Complete || hotSummary.ScannedFiles != 0 || hotSummary.CacheHits != 1 {
		t.Fatalf("hot summary/error = %#v/%v, want complete empty-tree cache hit", hotSummary, err)
	}
	if hotProvider.listCount() != 0 {
		t.Fatalf("hot ListTree calls = %d, want zero provider requests", hotProvider.listCount())
	}
}

func TestRunnerEscapedTreePathsAboveManifestLimitAreNotCached(t *testing.T) {
	const (
		entryCount   = 30_000
		escapedChars = 400
	)
	selectedData := []byte("needle selected\n")
	selectedOID := testGitBlobOID(selectedData)
	escaped := strings.Repeat("<", escapedChars)
	entries := make([]provider.Entry, entryCount)
	for i := range entries {
		entries[i] = provider.Entry{
			Path: fmt.Sprintf("pkg/%s-%06d.txt", escaped, i),
			OID:  selectedOID,
			Mode: "100644",
			Size: int64(len(selectedData)),
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("json.Marshal(escaped tree) error = %v", err)
	}
	fits := treeManifestFitsCache(entries)
	if len(encoded) <= maxTreeManifestSize || fits {
		t.Fatalf("escaped tree JSON size = %d, fits = %v; want actual JSON above limit and rejected", len(encoded), fits)
	}

	snapshot := testSnapshot()
	objectCache := newTestCache(t)
	blobKey := cache.Key(snapshot.Repository.CacheNamespace(), "blob", selectedOID)
	stored, _, err := objectCache.Put(blobKey, bytes.NewReader(selectedData))
	if err != nil {
		t.Fatalf("seed selected blob cache: %v", err)
	}
	stored.Close()
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	targetPath := entries[0].Path
	remote := &fakeProvider{entries: entries, blobs: map[string][]byte{selectedOID: selectedData}}
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeExact, Workers: 1, Globs: []string{targetPath}}}
	summary, err := runner.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || !summary.Complete || summary.MatchedLines != 1 || summary.ScannedFiles != 1 {
		t.Fatalf("summary/error = %#v/%v, want complete cached-blob match", summary, err)
	}
	treeKey := cache.Key(snapshot.Repository.CacheNamespace(), "tree", snapshot.Commit)
	reader, hit, err := objectCache.Open(treeKey)
	if reader != nil {
		reader.Close()
	}
	if err != nil || hit {
		t.Fatalf("escaped tree cache = hit %v, error %v; want no persisted manifest", hit, err)
	}
}

func TestRunnerResultLimitDoesNotCommitPartialBlobCache(t *testing.T) {
	objectCache := newTestCache(t)
	snapshot := testSnapshot()
	data := append([]byte("needle\n"), bytes.Repeat([]byte{'x'}, 128<<10)...)
	tracked := &trackingReader{data: data}
	remote := &fakeProvider{
		entries:     []provider.Entry{{Path: "large.txt", OID: "large", Mode: "100644"}},
		blobs:       map[string][]byte{"large": data},
		candidates:  []string{"large.txt"},
		blobReaders: map[string]*trackingReader{"large.txt": tracked},
	}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	limited := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeIndexed, MaxResults: 1, Workers: 1}}
	first, err := limited.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || !first.Truncated || first.Reason != "result_limit" {
		t.Fatalf("first summary/error = %#v/%v, want result-limit truncation", first, err)
	}
	if tracked.read >= int64(len(data)) {
		t.Fatalf("limited blob read %d bytes of %d; want early stop", tracked.read, len(data))
	}
	blobKey := cache.Key(snapshot.Repository.CacheNamespace(), "file", snapshot.Commit, "large.txt")
	if _, hit, err := objectCache.Open(blobKey); err != nil || hit {
		t.Fatalf("partial blob cache = hit %v, error %v; want no committed entry", hit, err)
	}
	remote.mu.Lock()
	remote.blobReaders = nil
	remote.mu.Unlock()

	full := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: Config{Mode: ModeIndexed, Workers: 1}}
	second, err := full.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || second.MatchedLines != 1 {
		t.Fatalf("complete scan summary/error = %#v/%v, want one match", second, err)
	}
	reader, hit, err := objectCache.Open(blobKey)
	if err != nil || !hit {
		t.Fatalf("complete blob cache = hit %v, error %v; want committed entry", hit, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close complete blob cache: %v", err)
	}
	third, err := full.Run(context.Background(), snapshot, func(Event) error { return nil })
	if err != nil || third.CacheHits != 1 || remote.openCount() != 2 {
		t.Fatalf("cached scan summary/error/open calls = %#v/%v/%d, want blob cache hit and no third blob request", third, err, remote.openCount())
	}
}

func TestRunnerInvalidGlobFailsBeforeProviderCalls(t *testing.T) {
	remote := &fakeProvider{}
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner := &Runner{Provider: remote, Cache: cache.Disabled(), Matcher: matcher, Config: Config{Mode: ModeExact, Globs: []string{"["}}}
	summary, err := runner.Run(context.Background(), testSnapshot(), func(Event) error { return nil })
	if err == nil || ErrorCode(err) != "invalid_glob" {
		t.Fatalf("Run() error = %v, want invalid_glob", err)
	}
	if summary.Complete || remote.listCount() != 0 {
		t.Fatalf("summary/calls = %#v/%d", summary, remote.listCount())
	}
}

func TestResourceLimitErrorChainsRemainClassified(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "limited spool",
			err: func() error {
				writer := &limitedSpoolWriter{writer: io.Discard, written: maxResultSpoolBytes}
				_, err := writer.Write([]byte("x"))
				return err
			}(),
		},
		{
			name: "bounded archive reader",
			err: func() error {
				reader := &boundedContextReader{ctx: context.Background(), reader: strings.NewReader("x"), limit: 4, limitErr: errArchiveExpandedLimit}
				_, err := reader.Read(make([]byte, 1))
				return err
			}(),
		},
		{
			name: "compressed archive",
			err:  fmt.Errorf("compressed: %w", errArchiveCompressedLimit),
		},
		{
			name: "archive metadata",
			err:  fmt.Errorf("%w (headers)", errArchiveMetadataLimit),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil || !IsResourceLimit(tt.err) || ErrorCode(tt.err) != "resource_limit" {
				t.Fatalf("error = %v, want resource_limit error chain", tt.err)
			}
		})
	}
}

func TestArchiveRelativePathRejectsNUL(t *testing.T) {
	if _, _, err := archiveRelativePath("owner-repo/file\x00.txt"); err == nil {
		t.Fatal("archiveRelativePath() accepted NUL path")
	}
}

func TestOversizedRepositoryPathsAreResourceLimited(t *testing.T) {
	longPath := strings.Repeat("p", provider.MaxRepositoryPathBytes+1)
	assertResourceLimit := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !IsResourceLimit(err) || ErrorCode(err) != "resource_limit" {
			t.Fatalf("error = %v, want resource_limit", err)
		}
	}

	t.Run("archive path", func(t *testing.T) {
		_, _, err := archiveRelativePath("owner-repo/" + longPath)
		assertResourceLimit(t, err)
	})

	t.Run("cached tree manifest", func(t *testing.T) {
		encoded, err := json.Marshal([]provider.Entry{{Path: longPath, OID: testGitBlobOID([]byte("x")), Mode: "100644"}})
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		_, _, err = decodeTreeManifestContext(context.Background(), bytes.NewReader(encoded), nil)
		assertResourceLimit(t, err)
	})

	t.Run("indexed candidate", func(t *testing.T) {
		globs, err := CompileGlobs(nil)
		if err != nil {
			t.Fatalf("CompileGlobs() error = %v", err)
		}
		_, err = indexedCandidates([]string{longPath}, globs)
		assertResourceLimit(t, err)
	})
}

func TestDecodeTreeManifestContextStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := decodeTreeManifestContext(ctx, strings.NewReader(`[{"path":"a.txt","oid":"`+testGitBlobOID([]byte("needle\n"))+`","mode":"100644"}]`), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeTreeManifestContext() error = %v, want context.Canceled", err)
	}
}

func TestConsumeOutcomeCanceledSpoolCleansTemporaryFile(t *testing.T) {
	spoolPath := t.TempDir() + "/events"
	if err := os.WriteFile(spoolPath, []byte(`{"type":"match","path":"a.txt","line":1,"text":"needle"}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(spool) error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped, err := (&Runner{}).consumeOutcome(ctx, fileOutcome{spoolPath: spoolPath}, &Summary{}, make(map[string]struct{}), func(Event) error { return nil }, "read_result_spool", "spool_error")
	if !stopped || !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeOutcome() stopped/error = %v/%v, want canceled stop", stopped, err)
	}
	if _, statErr := os.Stat(spoolPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled spool stat error = %v, want temporary file removed", statErr)
	}
}

func TestScanArchiveCanceledDuringCachedGzipKeepsValidCache(t *testing.T) {
	objectCache := newTestCache(t)
	snapshot := testSnapshot()
	archiveKey := cache.Key(snapshot.Repository.CacheNamespace(), "archive", snapshot.Commit)
	stored, _, err := objectCache.Put(archiveKey, bytes.NewReader(makeTestArchive(t, testArchiveFile{name: "owner-repo-commit/a.txt", data: []byte("needle\n")})))
	if err != nil {
		t.Fatalf("seed archive cache error = %v", err)
	}
	stored.Close()
	archive, hit, err := objectCache.Open(archiveKey)
	if err != nil || !hit {
		t.Fatalf("open cached archive = hit %v, error %v", hit, err)
	}
	remaining := map[string]provider.Entry{
		"a.txt": {Path: "a.txt", OID: testGitBlobOID([]byte("needle\n")), Mode: "100644"},
	}
	ctx := &cancelAfterErrContext{Context: context.Background(), remaining: 2}
	var summary Summary
	stopped, err := (&Runner{Cache: objectCache}).scanArchive(ctx, snapshot, remaining, archive, &summary, make(map[string]struct{}), func(Event) error { return nil })
	if !stopped || !errors.Is(err, context.Canceled) {
		t.Fatalf("scanArchive() stopped/error = %v/%v, want cancellation during cached gzip initialization", stopped, err)
	}
	reader, hit, err := objectCache.Open(archiveKey)
	if err != nil || !hit {
		t.Fatalf("cached archive after cancellation = hit %v, error %v; want valid entry preserved", hit, err)
	}
	reader.Close()
}

type cancelAfterErrContext struct {
	context.Context
	remaining int
}

func newTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("HOME", root)
	t.Setenv("LocalAppData", root)
	objectCache, err := cache.New(false)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}
	return objectCache
}

func (c *cancelAfterErrContext) Err() error {
	if c.remaining > 0 {
		c.remaining--
		return nil
	}
	return context.Canceled
}

type fakeProvider struct {
	mu           sync.Mutex
	entries      []provider.Entry
	blobs        map[string][]byte
	candidates   []string
	searchErr    error
	listCalls    int
	openCalls    int
	searchCalls  int
	archiveCalls int
	archiveData  []byte
	archiveErr   error
	stats        provider.RequestStats
	treePartial  bool
	treeErr      error
	listRequires []bool
	blobPaths    []string
	blobErrs     map[string]error
	blobReaders  map[string]*trackingReader
	blobGate     <-chan struct{}
	blobBlocks   map[string]<-chan struct{}
	blobStarted  chan<- string
	blobComplete chan<- string
	activeBlobs  int
	maxActive    int
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Resolve(context.Context, provider.Repository, string) (provider.Snapshot, error) {
	return testSnapshot(), nil
}

func (f *fakeProvider) ListTree(_ context.Context, _ provider.Snapshot, requireComplete bool) ([]provider.Entry, bool, error) {
	f.mu.Lock()
	f.listCalls++
	f.listRequires = append(f.listRequires, requireComplete)
	entries := append([]provider.Entry(nil), f.entries...)
	for i := range entries {
		if entries[i].Mode == "" {
			entries[i].Mode = "100644"
		}
		if !isGitBlobOID(entries[i].OID) {
			if data, ok := fakeBlobData(f.blobs, entries[i]); ok {
				entries[i].OID = testGitBlobOID(data)
			}
		}
	}
	complete := !f.treePartial
	err := f.treeErr
	f.mu.Unlock()
	return entries, complete, err
}

func (f *fakeProvider) OpenBlob(ctx context.Context, _ provider.Snapshot, entry provider.Entry) (io.ReadCloser, error) {
	f.mu.Lock()
	f.openCalls++
	f.blobPaths = append(f.blobPaths, entry.Path)
	f.activeBlobs++
	if f.activeBlobs > f.maxActive {
		f.maxActive = f.activeBlobs
	}
	gate := f.blobGate
	block := f.blobBlocks[entry.Path]
	started := f.blobStarted
	completed := f.blobComplete
	err, hasErr := f.blobErrs[entry.Path]
	data, ok := fakeBlobData(f.blobs, entry)
	if !ok && entry.OID == "" {
		for _, manifestEntry := range f.entries {
			if manifestEntry.Path == entry.Path {
				data, ok = fakeBlobData(f.blobs, manifestEntry)
				break
			}
		}
	}
	source := f.blobReaders[entry.Path]
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.activeBlobs--
		f.mu.Unlock()
		if completed != nil {
			completed <- entry.Path
		}
	}()
	if started != nil {
		started <- entry.Path
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if hasErr {
		return nil, err
	}
	if source != nil {
		return io.NopCloser(source), nil
	}
	if !ok {
		return nil, errors.New("missing fake blob " + entry.OID)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeProvider) OpenArchive(context.Context, provider.Snapshot) (io.ReadCloser, error) {
	f.mu.Lock()
	f.archiveCalls++
	if f.archiveErr != nil {
		err := f.archiveErr
		f.mu.Unlock()
		return nil, err
	}
	if f.archiveData != nil {
		data := append([]byte(nil), f.archiveData...)
		f.mu.Unlock()
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	entries := append([]provider.Entry(nil), f.entries...)
	blobs := make(map[string][]byte, len(f.blobs))
	for oid, data := range f.blobs {
		blobs[oid] = append([]byte(nil), data...)
	}
	f.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		data, ok := fakeBlobData(blobs, entry)
		if !ok {
			return nil, errors.New("missing fake blob " + entry.OID)
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: "owner-repo-commit/" + entry.Path, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tarWriter.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(compressed.Bytes())), nil
}

func (f *fakeProvider) SearchCandidates(context.Context, provider.Snapshot, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return append([]string(nil), f.candidates...), nil
}

func (f *fakeProvider) RequestStats() provider.RequestStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats := f.stats
	if stats.RateLimit != nil {
		copy := *stats.RateLimit
		stats.RateLimit = &copy
	}
	return stats
}

func (f *fakeProvider) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeProvider) listRequireCompleteCopy() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.listRequires...)
}

func (f *fakeProvider) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openCalls
}

func (f *fakeProvider) searchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchCalls
}

func (f *fakeProvider) archiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.archiveCalls
}

func (f *fakeProvider) blobPathsCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.blobPaths...)
}

func (f *fakeProvider) maxActiveBlobCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func hasEventCode(events []Event, code string) bool {
	for _, event := range events {
		if event.Code == code {
			return true
		}
	}
	return false
}

func testGitBlobOID(data []byte) string {
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func testGitBlobOID256(data []byte) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func isGitBlobOID(value string) bool {
	if len(value) != sha1.Size*2 && len(value) != 32*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func fakeBlobData(blobs map[string][]byte, entry provider.Entry) ([]byte, bool) {
	if data, ok := blobs[entry.OID]; ok {
		return data, true
	}
	if entry.OID == "" {
		if data, ok := blobs[entry.Path]; ok {
			return data, true
		}
	}
	for _, data := range blobs {
		if testGitBlobOID(data) == entry.OID || testGitBlobOID256(data) == entry.OID {
			return data, true
		}
	}
	return nil, false
}

type testArchiveFile struct {
	name     string
	data     []byte
	typeflag byte
}

type trackingReader struct {
	data   []byte
	offset int
	read   int64
}

func (r *trackingReader) Read(buffer []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	read := copy(buffer, r.data[r.offset:])
	r.offset += read
	r.read += int64(read)
	return read, nil
}

func makeTestArchive(t *testing.T, files ...testArchiveFile) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, file := range files {
		typeflag := file.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.data)), Typeflag: typeflag}); err != nil {
			t.Fatalf("write archive header %q: %v", file.name, err)
		}
		if _, err := tarWriter.Write(file.data); err != nil {
			t.Fatalf("write archive data %q: %v", file.name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return compressed.Bytes()
}

func testSnapshot() provider.Snapshot {
	repo := provider.Repository{Provider: "fake", Host: "fake.test", Project: "owner/repo", APIBase: "https://fake.test/api"}
	return provider.Snapshot{Repository: repo, Commit: "commit-1", TreeOID: "tree-1", ResolvedRef: "main"}
}

var _ provider.Provider = (*fakeProvider)(nil)
