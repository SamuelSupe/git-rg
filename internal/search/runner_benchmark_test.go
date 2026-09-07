package search

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

// BenchmarkRunner measures the local CPU and filesystem path used by Runner.
// The provider below is deliberately in-memory and adds no network latency.
// Fixture construction and archive compression happen before the timer starts.
func BenchmarkRunner(b *testing.B) {
	fixtures := benchmarkFixtures()
	for _, fixture := range fixtures {
		fixture := fixture
		b.Run(fixture.name+"/exact/cold-cache", func(b *testing.B) {
			benchmarkRunnerCase(b, fixture, ModeExact, false, nil)
		})
		b.Run(fixture.name+"/exact/hot-cache", func(b *testing.B) {
			benchmarkRunnerCase(b, fixture, ModeExact, true, nil)
		})
		b.Run(fixture.name+"/auto/cold-cache", func(b *testing.B) {
			benchmarkRunnerCase(b, fixture, ModeAuto, false, nil)
		})
		b.Run(fixture.name+"/auto/hot-cache", func(b *testing.B) {
			benchmarkRunnerCase(b, fixture, ModeAuto, true, nil)
		})
	}
	// The first fixture file uses an exact single-file glob so both modes cover
	// the direct-blob path for a small candidate set.
	fixture := fixtures[0]
	b.Run(fixture.name+"/exact/narrow-glob-cold-cache", func(b *testing.B) {
		benchmarkRunnerCase(b, fixture, ModeExact, false, []string{"pkg/file-0000.txt"})
	})
	b.Run(fixture.name+"/auto/narrow-glob-cold-cache", func(b *testing.B) {
		benchmarkRunnerCase(b, fixture, ModeAuto, false, []string{"pkg/file-0000.txt"})
	})
}

func benchmarkRunnerCase(b *testing.B, fixture benchmarkFixture, mode Mode, hotCache bool, globs []string) {
	b.Helper()
	matcher, err := NewMatcher(MatcherConfig{Pattern: "needle"})
	if err != nil {
		b.Fatalf("NewMatcher() error = %v", err)
	}

	cacheRoot := b.TempDir()
	// cache.New uses different environment variables on the supported hosts.
	b.Setenv("HOME", cacheRoot)
	b.Setenv("USERPROFILE", cacheRoot)
	b.Setenv("XDG_CACHE_HOME", cacheRoot)
	b.Setenv("LocalAppData", cacheRoot)
	objectCache, err := cache.New(false)
	if err != nil {
		b.Fatalf("cache.New() error = %v", err)
	}

	config := Config{Mode: mode, Workers: 4, Globs: globs}
	expectedLines, expectedFiles := benchmarkExpectedSummary(fixture, globs)
	archiveKey := cache.Key(fixture.snapshot.Repository.CacheNamespace(), "archive", fixture.snapshot.Commit)
	treeKey := cache.Key(fixture.snapshot.Repository.CacheNamespace(), "tree", fixture.snapshot.Commit)
	if hotCache {
		warmProvider := &benchmarkProvider{fixture: fixture}
		warmConfig := config
		warmConfig.Mode = ModeExact
		warmSummary, _, warmErr := runBenchmarkQuery(warmProvider, objectCache, matcher, warmConfig, fixture)
		if warmErr != nil {
			b.Fatalf("warm-up Run() error = %v", warmErr)
		}
		verifyBenchmarkSummary(b, warmSummary, expectedLines, expectedFiles)
		warmTreeManifest, err := json.Marshal(fixture.entries)
		if err != nil {
			b.Fatalf("marshal warm tree manifest = %v", err)
		}
		stored, _, err := objectCache.Put(treeKey, bytes.NewReader(warmTreeManifest))
		if stored != nil {
			stored.Close()
		}
		if err != nil {
			b.Fatalf("write warm tree manifest = %v", err)
		}
		candidateSet := make(map[string]struct{}, len(fixture.candidates))
		for _, path := range fixture.candidates {
			candidateSet[path] = struct{}{}
		}
		for _, entry := range fixture.entries {
			if _, ok := candidateSet[entry.Path]; !ok {
				continue
			}
			blobKey := cache.Key(fixture.snapshot.Repository.CacheNamespace(), "blob", entry.OID)
			stored, _, err := objectCache.Put(blobKey, bytes.NewReader(fixture.blobs[entry.OID]))
			if stored != nil {
				stored.Close()
			}
			if err != nil {
				b.Fatalf("write warm blob %q = %v", entry.Path, err)
			}
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	var totalFirstMatchNS int64
	var totalRequests int64
	var totalDownloadedBytes int64
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if !hotCache {
			if err := clearBenchmarkCache(objectCache, fixture, archiveKey, treeKey); err != nil {
				b.Fatalf("clear cold cache = %v", err)
			}
		}
		remote := &benchmarkProvider{fixture: fixture}
		b.StartTimer()
		summary, firstMatchNS, err := runBenchmarkQuery(remote, objectCache, matcher, config, fixture)
		b.StopTimer()
		if err != nil {
			b.Fatalf("Run() error = %v", err)
		}
		verifyBenchmarkSummary(b, summary, expectedLines, expectedFiles)
		if firstMatchNS < 0 {
			b.Fatal("Run() emitted no match event")
		}
		totalFirstMatchNS += firstMatchNS
		totalRequests += int64(remote.RequestStats().Requests)
		totalDownloadedBytes += summary.DownloadedBytes
	}

	b.ReportMetric(float64(totalFirstMatchNS)/float64(b.N), "first-match-ns/op")
	b.ReportMetric(float64(totalRequests)/float64(b.N), "remote-requests/op")
	b.ReportMetric(float64(totalDownloadedBytes)/float64(b.N), "downloaded-B/op")
}

func clearBenchmarkCache(objectCache *cache.Cache, fixture benchmarkFixture, archiveKey, treeKey string) error {
	if err := objectCache.Remove(archiveKey); err != nil {
		return err
	}
	if err := objectCache.Remove(treeKey); err != nil {
		return err
	}
	for _, entry := range fixture.entries {
		key := cache.Key(fixture.snapshot.Repository.CacheNamespace(), "blob", entry.OID)
		if err := objectCache.Remove(key); err != nil {
			return err
		}
	}
	return nil
}

func runBenchmarkQuery(remote *benchmarkProvider, objectCache *cache.Cache, matcher *Matcher, config Config, fixture benchmarkFixture) (Summary, int64, error) {
	started := time.Now()
	firstMatchNS := int64(-1)
	runner := &Runner{Provider: remote, Cache: objectCache, Matcher: matcher, Config: config}
	summary, err := runner.Run(context.Background(), fixture.snapshot, func(event Event) error {
		if event.Type == "match" && firstMatchNS < 0 {
			firstMatchNS = time.Since(started).Nanoseconds()
		}
		return nil
	})
	return summary, firstMatchNS, err
}

func verifyBenchmarkSummary(b *testing.B, summary Summary, expectedLines, expectedFiles int) {
	b.Helper()
	if !summary.Complete || summary.Truncated || summary.MatchedLines != expectedLines || summary.MatchedFiles != expectedFiles {
		b.Fatalf("summary = %#v, want complete result with %d lines in %d files", summary, expectedLines, expectedFiles)
	}
}

func benchmarkExpectedSummary(fixture benchmarkFixture, globs []string) (int, int) {
	compiled, err := CompileGlobs(globs)
	if err != nil {
		panic(err)
	}
	lines, files := 0, 0
	for _, entry := range fixture.entries {
		if !compiled.Match(entry.Path) {
			continue
		}
		data := fixture.blobs[entry.OID]
		if !bytes.Contains(data, []byte("needle")) {
			continue
		}
		lines += strings.Count(string(data), "needle")
		files++
	}
	return lines, files
}

type benchmarkFixture struct {
	name          string
	entries       []provider.Entry
	blobs         map[string][]byte
	candidates    []string
	archive       []byte
	snapshot      provider.Snapshot
	expectedLines int
	expectedFiles int
}

type benchmarkFile struct {
	path string
	data []byte
}

func benchmarkFixtures() []benchmarkFixture {
	return []benchmarkFixture{
		newBenchmarkFixture("many-small-files", benchmarkManySmallFiles()),
		newBenchmarkFixture("large-text-many-matches", []benchmarkFile{{
			path: "src/large.txt",
			data: benchmarkLargeText(),
		}}),
	}
}

func benchmarkManySmallFiles() []benchmarkFile {
	const count = 512
	files := make([]benchmarkFile, 0, count)
	for i := 0; i < count; i++ {
		marker := "ordinary"
		if i%2 == 0 {
			marker = "needle"
		}
		files = append(files, benchmarkFile{
			path: fmt.Sprintf("pkg/file-%04d.txt", i),
			data: []byte(fmt.Sprintf("file=%04d marker=%s payload=%s\n", i, marker, benchmarkPayload(i, 64))),
		})
	}
	return files
}

func benchmarkLargeText() []byte {
	const lines = 4096
	const payloadSize = 224
	var text strings.Builder
	text.Grow(lines * (payloadSize + 32))
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&text, "needle line=%04d payload=%s\n", i, benchmarkPayload(i+lines, payloadSize))
	}
	return []byte(text.String())
}

func benchmarkPayload(seed, size int) string {
	var payload strings.Builder
	input := []byte(fmt.Sprintf("git-rg-benchmark-%d", seed))
	for payload.Len() < size {
		digest := sha1.Sum(input)
		payload.WriteString(hex.EncodeToString(digest[:]))
		input = digest[:]
	}
	return payload.String()[:size]
}

func newBenchmarkFixture(name string, files []benchmarkFile) benchmarkFixture {
	entries := make([]provider.Entry, 0, len(files))
	blobs := make(map[string][]byte, len(files))
	candidates := make([]string, 0, len(files))
	expectedLines := 0
	expectedFiles := 0
	for _, file := range files {
		data := append([]byte(nil), file.data...)
		oid := benchmarkGitObjectOID("blob", data)
		entries = append(entries, provider.Entry{Path: file.path, OID: oid, Mode: "100644", Size: int64(len(data))})
		blobs[oid] = data
		if bytes.Contains(data, []byte("needle")) {
			candidates = append(candidates, file.path)
			lines := strings.Count(string(data), "needle")
			expectedLines += lines
			expectedFiles++
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	sort.Strings(candidates)
	snapshot := provider.Snapshot{
		Repository: provider.Repository{
			Provider: "benchmark",
			Host:     "benchmark.invalid",
			Project:  "runner-fixtures",
			WebURL:   "https://benchmark.invalid/runner-fixtures",
			APIBase:  "https://benchmark.invalid/api",
		},
		RequestedRef: "main",
		ResolvedRef:  "main",
		Commit:       benchmarkCommitOID,
		TreeOID:      benchmarkTreeOID,
	}
	return benchmarkFixture{
		name:          name,
		entries:       entries,
		blobs:         blobs,
		candidates:    candidates,
		archive:       benchmarkArchive(files),
		snapshot:      snapshot,
		expectedLines: expectedLines,
		expectedFiles: expectedFiles,
	}
}

const (
	benchmarkCommitOID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	benchmarkTreeOID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func benchmarkGitObjectOID(kind string, payload []byte) string {
	hash := sha1.New()
	fmt.Fprintf(hash, "%s %d\x00", kind, len(payload))
	_, _ = hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil))
}

func benchmarkArchive(files []benchmarkFile) []byte {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, file := range files {
		header := &tar.Header{
			Name:     "fixture/" + file.path,
			Mode:     0o644,
			Size:     int64(len(file.data)),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			panic(err)
		}
		if _, err := tarWriter.Write(file.data); err != nil {
			panic(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		panic(err)
	}
	if err := gzipWriter.Close(); err != nil {
		panic(err)
	}
	return compressed.Bytes()
}

type benchmarkProvider struct {
	fixture  benchmarkFixture
	requests atomic.Int64
}

func (p *benchmarkProvider) Name() string { return "benchmark" }

func (p *benchmarkProvider) Resolve(context.Context, provider.Repository, string) (provider.Snapshot, error) {
	return p.fixture.snapshot, nil
}

func (p *benchmarkProvider) ListTree(ctx context.Context, _ provider.Snapshot, _ bool) ([]provider.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	p.requests.Add(1)
	return append([]provider.Entry(nil), p.fixture.entries...), true, nil
}

func (p *benchmarkProvider) OpenBlob(ctx context.Context, _ provider.Snapshot, entry provider.Entry) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.requests.Add(1)
	data, ok := p.fixture.blobs[entry.OID]
	if !ok {
		return nil, fmt.Errorf("benchmark blob %q not found", entry.OID)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (p *benchmarkProvider) OpenArchive(ctx context.Context, _ provider.Snapshot) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.requests.Add(1)
	return io.NopCloser(bytes.NewReader(p.fixture.archive)), nil
}

func (p *benchmarkProvider) SearchCandidates(ctx context.Context, _ provider.Snapshot, _ string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.requests.Add(1)
	return append([]string(nil), p.fixture.candidates...), nil
}

func (p *benchmarkProvider) RequestStats() provider.RequestStats {
	return provider.RequestStats{Requests: int(p.requests.Load())}
}

var _ provider.Provider = (*benchmarkProvider)(nil)
