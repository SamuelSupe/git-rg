package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamuelSupe/git-rg/internal/search"
)

// BenchmarkCLI measures the real Run entry point, including repository
// resolution, cache maintenance, HTTP transport, Runner, and NDJSON writes.
// The HTTP server is local and deterministic; no external network latency or
// process startup is included. Output is written to a temporary file rather
// than discarded so the measured path includes actual writer I/O.
func BenchmarkCLI(b *testing.B) {
	for _, fixture := range cliBenchmarkFixtures() {
		fixture := fixture
		server := newCLIBenchmarkServer(fixture)
		b.Run(fixture.name+"/exact/cold-cache", func(b *testing.B) {
			benchmarkCLIQuery(b, fixture, server, "exact", false, 0)
		})
		b.Run(fixture.name+"/exact/hot-cache", func(b *testing.B) {
			benchmarkCLIQuery(b, fixture, server, "exact", true, 0)
		})
		b.Run(fixture.name+"/auto/cold-cache", func(b *testing.B) {
			benchmarkCLIQuery(b, fixture, server, "auto", false, 0)
		})
		b.Run(fixture.name+"/auto/hot-cache", func(b *testing.B) {
			benchmarkCLIQuery(b, fixture, server, "auto", true, 0)
		})
		if fixture.name == "many-small-files" {
			for _, noiseEntries := range []int{1000, 10000} {
				noiseEntries := noiseEntries
				b.Run(fmt.Sprintf("%s/exact/hot-cache/cache-%d-entries", fixture.name, noiseEntries), func(b *testing.B) {
					benchmarkCLIQuery(b, fixture, server, "exact", true, noiseEntries)
				})
			}
		}
		server.Close()
	}
}

func benchmarkCLIQuery(b *testing.B, fixture cliBenchmarkFixture, server *cliBenchmarkServer, mode string, hotCache bool, cacheNoiseEntries int) {
	b.Helper()
	cacheRoot := b.TempDir()
	cacheDir, err := setCLIBenchmarkCacheEnv(b, cacheRoot)
	if err != nil {
		b.Fatalf("resolve benchmark cache directory = %v", err)
	}
	outputFile, err := os.OpenFile(filepath.Join(b.TempDir(), "stdout.ndjson"), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		b.Fatalf("open benchmark output = %v", err)
	}
	defer outputFile.Close()
	sink := &cliBenchmarkSink{file: outputFile, firstMatchNS: -1}
	args := cliBenchmarkArgs(server.URL(), mode)
	if hotCache {
		warmArgs := cliBenchmarkArgs(server.URL(), "exact")
		if _, err := runCLIForBenchmark(b, fixture, warmArgs, sink, server); err != nil {
			b.Fatalf("warm-up Run() error = %v", err)
		}
		if cacheNoiseEntries > 0 {
			if err := seedCLIBenchmarkCache(cacheDir, cacheNoiseEntries); err != nil {
				b.Fatalf("seed cache with %d noise entries = %v", cacheNoiseEntries, err)
			}
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	var totalFirstMatchNS int64
	var totalRequests int64
	var totalResponseBytes int64
	var totalOutputBytes int64
	var totalMatches int64
	var totalSummaryDurationMS int64
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if !hotCache {
			if err := clearCLIBenchmarkCache(cacheDir); err != nil {
				b.Fatalf("clear cold cache = %v", err)
			}
		}
		if err := sink.reset(); err != nil {
			b.Fatalf("reset benchmark output = %v", err)
		}
		server.resetStats()
		b.StartTimer()
		started := time.Now()
		sink.started = started
		status := Run(args, sink, io.Discard)
		b.StopTimer()
		if status != 0 {
			b.Fatalf("Run() status = %d, output=%q", status, sink.preview())
		}
		result, err := validateCLIBenchmarkOutput(outputFile, fixture)
		if err != nil {
			b.Fatalf("validate Run() output = %v; output=%q", err, sink.preview())
		}
		if sink.firstMatchNS < 0 {
			b.Fatal("Run() emitted no match before summary")
		}
		totalFirstMatchNS += sink.firstMatchNS
		totalRequests += server.requests.Load()
		totalResponseBytes += server.responseBytes.Load()
		totalOutputBytes += sink.bytes
		totalMatches += int64(result.matches)
		totalSummaryDurationMS += result.summary.DurationMS
	}
	b.ReportMetric(float64(totalFirstMatchNS)/float64(b.N), "first-match-ns/op")
	b.ReportMetric(float64(totalRequests)/float64(b.N), "remote-requests/op")
	b.ReportMetric(float64(totalResponseBytes)/float64(b.N), "remote-response-B/op")
	b.ReportMetric(float64(totalOutputBytes)/float64(b.N), "output-B/op")
	b.ReportMetric(float64(totalMatches)/float64(b.N), "matches/op")
	b.ReportMetric(float64(totalSummaryDurationMS)/float64(b.N), "summary-duration-ms/op")
}

func cliBenchmarkArgs(apiBase, mode string) []string {
	return []string{
		"--api-base", apiBase,
		"--auth", "env",
		"--mode", mode,
		"--max-results", "0",
		"--max-requests", "1000",
		"--timeout", "1m",
		"--format", "ndjson",
		"needle", "github:octocat/Hello-World",
	}
}

func setCLIBenchmarkCacheEnv(b *testing.B, root string) (string, error) {
	b.Helper()
	b.Setenv("HOME", root)
	b.Setenv("USERPROFILE", root)
	b.Setenv("XDG_CACHE_HOME", root)
	b.Setenv("LocalAppData", root)
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "git-rg"), nil
}

func clearCLIBenchmarkCache(cacheRoot string) error {
	if err := os.RemoveAll(cacheRoot); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Dir(cacheRoot), 0o700)
}

func seedCLIBenchmarkCache(cacheRoot string, entries int) error {
	for index := 0; index < entries; index++ {
		shard := filepath.Join(cacheRoot, fmt.Sprintf("%02x", index%256))
		if err := os.MkdirAll(shard, 0o700); err != nil {
			return err
		}
		path := filepath.Join(shard, fmt.Sprintf("noise-%05d.entry", index))
		if err := os.WriteFile(path, []byte("noise"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func runCLIForBenchmark(b *testing.B, fixture cliBenchmarkFixture, args []string, sink *cliBenchmarkSink, server *cliBenchmarkServer) (*cliBenchmarkResult, error) {
	b.Helper()
	if err := sink.reset(); err != nil {
		return nil, err
	}
	server.resetStats()
	sink.started = time.Now()
	if status := Run(args, sink, io.Discard); status != 0 {
		return nil, fmt.Errorf("Run() status = %d, output=%q", status, sink.preview())
	}
	return validateCLIBenchmarkOutput(sink.file, fixture)
}

type cliBenchmarkSink struct {
	file         *os.File
	started      time.Time
	firstMatchNS int64
	bytes        int64
}

func (s *cliBenchmarkSink) reset() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := s.file.Truncate(0); err != nil {
		return err
	}
	s.firstMatchNS = -1
	s.bytes = 0
	return nil
}

func (s *cliBenchmarkSink) Write(data []byte) (int, error) {
	s.bytes += int64(len(data))
	if s.firstMatchNS < 0 && bytes.Contains(data, []byte(`"type":"match"`)) {
		s.firstMatchNS = time.Since(s.started).Nanoseconds()
	}
	return s.file.Write(data)
}

func (s *cliBenchmarkSink) preview() string {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Sprintf("<seek output: %v>", err)
	}
	data, err := io.ReadAll(io.LimitReader(s.file, 1024))
	if err != nil {
		return fmt.Sprintf("<read output: %v>", err)
	}
	return string(data)
}

type cliBenchmarkResult struct {
	summary search.Summary
	matches int
}

func validateCLIBenchmarkOutput(file *os.File, fixture cliBenchmarkFixture) (*cliBenchmarkResult, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(file)
	result := &cliBenchmarkResult{}
	var sawMeta, sawSummary bool
	for {
		var event search.Event
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		switch event.Type {
		case "meta":
			sawMeta = true
		case "match":
			result.matches++
		case "summary":
			if event.Summary == nil {
				return nil, fmt.Errorf("summary event has no summary")
			}
			result.summary = *event.Summary
			sawSummary = true
		}
	}
	if !sawMeta || !sawSummary {
		return nil, fmt.Errorf("output has meta=%t summary=%t", sawMeta, sawSummary)
	}
	if result.matches != fixture.expectedLines || result.summary.MatchedLines != fixture.expectedLines || result.summary.MatchedFiles != fixture.expectedFiles || !result.summary.Complete || result.summary.Truncated {
		return nil, fmt.Errorf("result matches=%d summary=%#v, want %d lines in %d files complete", result.matches, result.summary, fixture.expectedLines, fixture.expectedFiles)
	}
	return result, nil
}

type cliBenchmarkFixture struct {
	name          string
	files         []cliBenchmarkFile
	byOID         map[string][]byte
	candidates    []string
	archive       []byte
	expectedLines int
	expectedFiles int
}

type cliBenchmarkFile struct {
	path string
	oid  string
	data []byte
}

func cliBenchmarkFixtures() []cliBenchmarkFixture {
	return []cliBenchmarkFixture{
		newCLIBenchmarkFixture("many-small-files", cliBenchmarkManySmallFiles()),
		newCLIBenchmarkFixture("large-text-many-matches", []cliBenchmarkFile{{path: "src/large.txt", data: cliBenchmarkLargeText()}}),
	}
}

func cliBenchmarkManySmallFiles() []cliBenchmarkFile {
	const count = 512
	files := make([]cliBenchmarkFile, 0, count)
	for i := 0; i < count; i++ {
		marker := "ordinary"
		if i%2 == 0 {
			marker = "needle"
		}
		data := []byte(fmt.Sprintf("file=%04d marker=%s payload=%s\n", i, marker, cliBenchmarkPayload(i, 64)))
		files = append(files, cliBenchmarkFile{path: fmt.Sprintf("pkg/file-%04d.txt", i), data: data})
	}
	return files
}

func cliBenchmarkLargeText() []byte {
	const lines = 4096
	const payloadSize = 224
	var text strings.Builder
	text.Grow(lines * (payloadSize + 32))
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&text, "needle line=%04d payload=%s\n", i, cliBenchmarkPayload(i+lines, payloadSize))
	}
	return []byte(text.String())
}

func cliBenchmarkPayload(seed, size int) string {
	var payload strings.Builder
	input := []byte(fmt.Sprintf("git-rg-benchmark-%d", seed))
	for payload.Len() < size {
		digest := sha1.Sum(input)
		payload.WriteString(hex.EncodeToString(digest[:]))
		input = digest[:]
	}
	return payload.String()[:size]
}

func newCLIBenchmarkFixture(name string, files []cliBenchmarkFile) cliBenchmarkFixture {
	fixture := cliBenchmarkFixture{
		name:  name,
		files: append([]cliBenchmarkFile(nil), files...),
		byOID: make(map[string][]byte, len(files)),
	}
	for index := range fixture.files {
		fixture.files[index].data = append([]byte(nil), fixture.files[index].data...)
		fixture.files[index].oid = cliBenchmarkBlobOID(fixture.files[index].data)
		fixture.byOID[fixture.files[index].oid] = fixture.files[index].data
		if bytes.Contains(fixture.files[index].data, []byte("needle")) {
			fixture.candidates = append(fixture.candidates, fixture.files[index].path)
			fixture.expectedLines += strings.Count(string(fixture.files[index].data), "needle")
			fixture.expectedFiles++
		}
	}
	fixture.archive = cliBenchmarkArchive(fixture.files)
	return fixture
}

func cliBenchmarkBlobOID(data []byte) string {
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func cliBenchmarkArchive(files []cliBenchmarkFile) []byte {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, file := range files {
		header := &tar.Header{Name: "fixture/" + file.path, Mode: 0o644, Size: int64(len(file.data)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
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

const (
	cliBenchmarkCommitOID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cliBenchmarkTreeOID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type cliBenchmarkServer struct {
	fixture       cliBenchmarkFixture
	server        *httptest.Server
	requests      atomic.Int64
	responseBytes atomic.Int64
}

func newCLIBenchmarkServer(fixture cliBenchmarkFixture) *cliBenchmarkServer {
	benchmarkServer := &cliBenchmarkServer{fixture: fixture}
	benchmarkServer.server = httptest.NewServer(http.HandlerFunc(benchmarkServer.handler))
	return benchmarkServer
}

func (s *cliBenchmarkServer) URL() string { return s.server.URL }

func (s *cliBenchmarkServer) Close() { s.server.Close() }

func (s *cliBenchmarkServer) resetStats() {
	s.requests.Store(0)
	s.responseBytes.Store(0)
}

func (s *cliBenchmarkServer) handler(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	counted := &cliBenchmarkResponseWriter{ResponseWriter: w, bytes: &s.responseBytes}
	switch r.URL.EscapedPath() {
	case "/repos/octocat/Hello-World":
		writeCLIBenchmarkJSON(counted, map[string]any{"default_branch": "main"})
	case "/repos/octocat/Hello-World/commits/main":
		writeCLIBenchmarkJSON(counted, map[string]any{
			"sha":    cliBenchmarkCommitOID,
			"commit": map[string]any{"tree": map[string]any{"sha": cliBenchmarkTreeOID}},
		})
	case "/repos/octocat/Hello-World/git/trees/" + cliBenchmarkTreeOID:
		tree := make([]map[string]any, 0, len(s.fixture.files))
		for _, file := range s.fixture.files {
			tree = append(tree, map[string]any{"path": file.path, "mode": "100644", "type": "blob", "sha": file.oid, "size": len(file.data)})
		}
		writeCLIBenchmarkJSON(counted, map[string]any{"truncated": false, "tree": tree})
	case "/repos/octocat/Hello-World/tarball/" + cliBenchmarkCommitOID:
		_, _ = counted.Write(s.fixture.archive)
	case "/search/code":
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				page = parsed
			}
		}
		start := (page - 1) * 100
		if start > len(s.fixture.candidates) {
			start = len(s.fixture.candidates)
		}
		end := min(start+100, len(s.fixture.candidates))
		items := make([]map[string]string, 0, end-start)
		for _, path := range s.fixture.candidates[start:end] {
			items = append(items, map[string]string{"path": path})
		}
		writeCLIBenchmarkJSON(counted, map[string]any{"items": items})
	default:
		const blobPrefix = "/repos/octocat/Hello-World/git/blobs/"
		if strings.HasPrefix(r.URL.EscapedPath(), blobPrefix) {
			oid := strings.TrimPrefix(r.URL.EscapedPath(), blobPrefix)
			data, ok := s.fixture.byOID[oid]
			if !ok {
				http.NotFound(counted, r)
				return
			}
			_, _ = counted.Write(data)
			return
		}
		http.NotFound(counted, r)
	}
}

type cliBenchmarkResponseWriter struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w *cliBenchmarkResponseWriter) Write(data []byte) (int, error) {
	written, err := w.ResponseWriter.Write(data)
	w.bytes.Add(int64(written))
	return written, err
}

func writeCLIBenchmarkJSON(w io.Writer, value any) {
	_ = json.NewEncoder(w).Encode(value)
}
