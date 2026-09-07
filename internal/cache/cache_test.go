package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testCacheEntryMagic      = "GITRG02\n"
	testCacheEntryHashOffset = len(testCacheEntryMagic)
	testCacheEntryHeaderSize = testCacheEntryHashOffset + sha256.Size
)

func TestPutFailureDoesNotPublishFinalEntry(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: DefaultMaxSize}
	key := Key("failed-entry")
	if _, written, err := c.Put(key, failingReader{}); err == nil || written == 0 {
		t.Fatalf("Put() = written %d, error %v; want partial write and error", written, err)
	}
	if _, hit, err := c.Open(key); err != nil || hit {
		t.Fatalf("Open(failed key) = hit %v, error %v; want cache miss", hit, err)
	}
	if _, err := os.Stat(c.path(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed final entry stat error = %v, want not exists", err)
	}
}

func TestPutOpenAndPrunePreserveSecureFilesAndEvictOldest(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: int64(testCacheEntryHeaderSize + 3)}

	oldKey := Key("old")
	oldFile := putAndClose(t, c, oldKey, "old")
	newKey := Key("new")
	newFile := putAndClose(t, c, newKey, "new")
	oldTime := time.Unix(100, 0)
	newTime := time.Unix(200, 0)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(old) error = %v", err)
	}
	if err := os.Chtimes(newFile, newTime, newTime); err != nil {
		t.Fatalf("Chtimes(new) error = %v", err)
	}
	info, err := os.Stat(newFile)
	if err != nil {
		t.Fatalf("Stat(new) error = %v", err)
	}
	assertPrivateFileMode(t, info, "new entry")
	if got := string(assertEntryPayload(t, c, newKey)); got != "new" {
		t.Fatalf("raw new entry payload = %q, want new", got)
	}

	reader, hit, err := c.Open(newKey)
	if err != nil || !hit {
		t.Fatalf("Open(new) = hit %v, error %v", hit, err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "new" {
		t.Fatalf("Open(new) data/error = %q/%v", data, err)
	}

	if err := c.Prune(); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if _, err := os.Stat(oldFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old entry stat error = %v, want pruned", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("new entry missing after prune: %v", err)
	}
}

func assertPrivateFileMode(t *testing.T, info os.FileInfo, label string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %#o, want 0600", label, got)
	}
}

func TestOpenRejectsTamperedContentOrChecksumAndRemovesEntry(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, c *Cache, key string)
	}{
		{
			name: "payload",
			edit: func(t *testing.T, c *Cache, key string) {
				data, err := os.ReadFile(c.path(key))
				if err != nil {
					t.Fatalf("ReadFile(content) error = %v", err)
				}
				data[testCacheEntryHeaderSize] ^= 0xff
				if err := os.WriteFile(c.path(key), data, 0o600); err != nil {
					t.Fatalf("WriteFile(content) error = %v", err)
				}
			},
		},
		{
			name: "checksum",
			edit: func(t *testing.T, c *Cache, key string) {
				data, err := os.ReadFile(c.path(key))
				if err != nil {
					t.Fatalf("ReadFile(checksum) error = %v", err)
				}
				data[testCacheEntryHashOffset] ^= 0xff
				if err := os.WriteFile(c.path(key), data, 0o600); err != nil {
					t.Fatalf("WriteFile(checksum) error = %v", err)
				}
			},
		},
		{
			name: "magic",
			edit: func(t *testing.T, c *Cache, key string) {
				data, err := os.ReadFile(c.path(key))
				if err != nil {
					t.Fatalf("ReadFile(magic) error = %v", err)
				}
				data[0] ^= 0xff
				if err := os.WriteFile(c.path(key), data, 0o600); err != nil {
					t.Fatalf("WriteFile(magic) error = %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Cache{root: t.TempDir(), enabled: true, maxSize: DefaultMaxSize}
			key := Key("tampered-" + tt.name)
			putAndClose(t, c, key, "immutable")
			tt.edit(t, c, key)
			if _, hit, err := c.Open(key); err == nil || hit {
				t.Fatalf("Open(tampered) = hit %v, error %v; want rejection", hit, err)
			}
			if _, err := os.Stat(c.path(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tampered content stat error = %v, want removed", err)
			}
		})
	}
}

func TestOpenContextCanceledPayloadKeepsValidEntry(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: DefaultMaxSize}
	key := Key("canceled-checksum")
	putAndClose(t, c, key, "immutable")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, hit, err := c.OpenContext(ctx, key); !errors.Is(err, context.Canceled) || hit {
		t.Fatalf("OpenContext(canceled) = hit %v, error %v; want canceled miss", hit, err)
	}
	if _, err := os.Stat(c.path(key)); err != nil {
		t.Fatalf("valid cache entry after canceled checksum = %v, want preserved", err)
	}
	if got := string(assertEntryPayload(t, c, key)); got != "immutable" {
		t.Fatalf("valid cache payload after canceled checksum = %q, want immutable", got)
	}
}

func TestPutRejectsSingleEntryOverCapacity(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: int64(testCacheEntryHeaderSize + 4)}
	key := Key("too-large")
	if _, written, err := c.Put(key, strings.NewReader("12345")); !errors.Is(err, ErrEntryTooLarge) || written != 0 {
		t.Fatalf("Put() = written %d, error %v; want entry-too-large before publish", written, err)
	}
	if _, hit, err := c.Open(key); err != nil || hit {
		t.Fatalf("Open(too-large) = hit %v, error %v; want miss", hit, err)
	}
	if _, err := os.Stat(c.path(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("too-large entry stat error = %v, want not exists", err)
	}
}

func TestPruneRemovesStaleTemporaryAndLegacyEntries(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: 1}
	legacyKey := Key("legacy")
	legacyPath := strings.TrimSuffix(c.path(legacyKey), ".entry")
	legacyChecksum := legacyPath + ".sha256"
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy"), 0o600); err != nil {
		t.Fatalf("WriteFile(legacy) error = %v", err)
	}
	if err := os.WriteFile(legacyChecksum, []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(legacy checksum) error = %v", err)
	}
	key := Key("orphan")
	directory := filepath.Dir(strings.TrimSuffix(c.path(key), ".entry"))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll(orphan) error = %v", err)
	}
	stale := filepath.Join(directory, ".tmp-stale")
	orphan := strings.TrimSuffix(c.path(key), ".entry") + ".sha256"
	for _, path := range []string{stale, orphan} {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
		when := time.Now().Add(-staleTemporaryAge - time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("Chtimes(%q) error = %v", path, err)
		}
	}
	if _, hit, err := c.Open(legacyKey); err != nil || hit {
		t.Fatalf("Open(legacy) = hit %v, error %v; want old format ignored", hit, err)
	}
	if err := c.Prune(); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	for _, path := range []string{stale, orphan, legacyPath, legacyChecksum} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale path %q stat error = %v, want removed", path, err)
		}
	}
}

func TestPruneIfNeededContextSkipsWithRecentMarker(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: int64(testCacheEntryHeaderSize + 3)}
	oldPath := writeSyntheticEntry(t, c, Key("recent-old"), "old")
	newPath := writeSyntheticEntry(t, c, Key("recent-new"), "new")
	setEntryTime(t, oldPath, time.Unix(100, 0))
	setEntryTime(t, newPath, time.Unix(200, 0))
	writePruneMarker(t, c, time.Now())

	if err := c.PruneIfNeededContext(context.Background()); err != nil {
		t.Fatalf("PruneIfNeededContext() error = %v", err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("recent-marker old entry stat error = %v, want skipped prune", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("recent-marker new entry stat error = %v", err)
	}
}

func TestPruneIfNeededContextTriggersForMissingOrStaleMarker(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stale bool
	}{
		{name: "missing"},
		{name: "stale", stale: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &Cache{root: t.TempDir(), enabled: true, maxSize: int64(testCacheEntryHeaderSize + 3)}
			oldPath := writeSyntheticEntry(t, c, Key("trigger-old"), "old")
			newPath := writeSyntheticEntry(t, c, Key("trigger-new"), "new")
			setEntryTime(t, oldPath, time.Unix(100, 0))
			setEntryTime(t, newPath, time.Unix(200, 0))
			if tt.stale {
				writePruneMarker(t, c, time.Now().Add(-6*time.Minute))
			}

			if err := c.PruneIfNeededContext(context.Background()); err != nil {
				t.Fatalf("PruneIfNeededContext() error = %v", err)
			}
			if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old entry stat error = %v, want triggered prune", err)
			}
			if _, err := os.Stat(newPath); err != nil {
				t.Fatalf("new entry stat error = %v", err)
			}
			if _, err := os.Stat(pruneMarkerPath(c)); err != nil {
				t.Fatalf("prune marker stat error = %v, want refreshed marker", err)
			}
		})
	}
}

func TestPruneIfNeededContextCommitTriggersCleanupWithRecentMarker(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: int64(testCacheEntryHeaderSize + 3)}
	oldPath := writeSyntheticEntry(t, c, Key("commit-old"), "old")
	setEntryTime(t, oldPath, time.Unix(100, 0))
	writePruneMarker(t, c, time.Now())
	newPath := putAndClose(t, c, Key("commit-new"), "new")

	if err := c.PruneIfNeededContext(context.Background()); err != nil {
		t.Fatalf("PruneIfNeededContext() error = %v", err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old entry stat error = %v, want commit-triggered prune", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new entry stat error = %v", err)
	}
}

func TestPruneIfNeededContextCancellationCanRetry(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: int64(testCacheEntryHeaderSize + 3)}
	oldPath := writeSyntheticEntry(t, c, Key("cancel-old"), "old")
	newPath := writeSyntheticEntry(t, c, Key("cancel-new"), "new")
	setEntryTime(t, oldPath, time.Unix(100, 0))
	setEntryTime(t, newPath, time.Unix(200, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.PruneIfNeededContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PruneIfNeededContext() error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("old entry after canceled prune = %v, want retained", err)
	}
	if _, err := os.Stat(pruneMarkerPath(c)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker after canceled prune = %v, want absent", err)
	}

	if err := c.PruneIfNeededContext(context.Background()); err != nil {
		t.Fatalf("retry PruneIfNeededContext() error = %v", err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old entry after retry = %v, want pruned", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new entry after retry = %v", err)
	}
}

func TestPruneIfNeededContextMarkerFailureCanRetry(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: int64(testCacheEntryHeaderSize + 3)}
	oldPath := writeSyntheticEntry(t, c, Key("failure-old"), "old")
	newPath := writeSyntheticEntry(t, c, Key("failure-new"), "new")
	setEntryTime(t, oldPath, time.Unix(100, 0))
	setEntryTime(t, newPath, time.Unix(200, 0))
	marker := pruneMarkerPath(c)
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatalf("Mkdir(marker) error = %v", err)
	}
	if err := os.Chtimes(marker, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatalf("Chtimes(marker) error = %v", err)
	}

	if err := c.PruneIfNeededContext(context.Background()); err == nil {
		t.Fatal("PruneIfNeededContext() unexpectedly succeeded with directory marker")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("Remove(marker directory) error = %v", err)
	}
	if err := c.PruneIfNeededContext(context.Background()); err != nil {
		t.Fatalf("retry after marker failure error = %v", err)
	}
	if _, err := os.Stat(pruneMarkerPath(c)); err != nil {
		t.Fatalf("marker after retry = %v", err)
	}
}

func TestPruneIfNeededContextConcurrentCallers(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: DefaultMaxSize}
	const callerCount = 8
	errs := make(chan error, callerCount)
	var callers sync.WaitGroup
	callers.Add(callerCount)
	for range callerCount {
		go func() {
			defer callers.Done()
			errs <- c.PruneIfNeededContext(context.Background())
		}()
	}
	callers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent PruneIfNeededContext() error = %v", err)
		}
	}
	if _, err := os.Stat(pruneMarkerPath(c)); err != nil {
		t.Fatalf("concurrent prune marker stat error = %v", err)
	}
}

func writeSyntheticEntry(t *testing.T, c *Cache, key, payload string) string {
	t.Helper()
	data := make([]byte, testCacheEntryHeaderSize+len(payload))
	copy(data, testCacheEntryMagic)
	digest := sha256.Sum256([]byte(payload))
	copy(data[testCacheEntryHashOffset:testCacheEntryHeaderSize], digest[:])
	copy(data[testCacheEntryHeaderSize:], payload)
	path := c.path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(entry) error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(entry) error = %v", err)
	}
	return path
}

func setEntryTime(t *testing.T, path string, stamp time.Time) {
	t.Helper()
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("Chtimes(%q) error = %v", path, err)
	}
}

func pruneMarkerPath(c *Cache) string {
	return filepath.Join(c.root, ".last-prune")
}

func writePruneMarker(t *testing.T, c *Cache, stamp time.Time) {
	t.Helper()
	path := pruneMarkerPath(c)
	if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(marker) error = %v", err)
	}
	setEntryTime(t, path, stamp)
}

func putAndClose(t *testing.T, c *Cache, key, value string) string {
	t.Helper()
	reader, written, err := c.Put(key, strings.NewReader(value))
	if err != nil {
		t.Fatalf("Put(%q) error = %v", key, err)
	}
	if written != int64(len(value)) {
		t.Fatalf("Put(%q) wrote %d, want %d", key, written, len(value))
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close Put(%q) reader: %v", key, err)
	}
	return c.path(key)
}

func assertEntryPayload(t *testing.T, c *Cache, key string) []byte {
	t.Helper()
	if got := filepath.Ext(c.path(key)); got != ".entry" {
		t.Fatalf("cache path extension = %q, want .entry", got)
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		t.Fatalf("ReadFile(entry) error = %v", err)
	}
	if len(data) < testCacheEntryHeaderSize {
		t.Fatalf("entry size = %d, want at least %d", len(data), testCacheEntryHeaderSize)
	}
	if got := string(data[:testCacheEntryHashOffset]); got != testCacheEntryMagic {
		t.Fatalf("entry magic = %q, want %q", got, testCacheEntryMagic)
	}
	want := sha256.Sum256(data[testCacheEntryHeaderSize:])
	if !bytes.Equal(data[testCacheEntryHashOffset:testCacheEntryHeaderSize], want[:]) {
		t.Fatalf("entry checksum does not match payload")
	}
	return append([]byte(nil), data[testCacheEntryHeaderSize:]...)
}

func readCachePayload(reader io.ReadCloser) (string, error) {
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return "", fmt.Errorf("read/close payload: %v/%v", readErr, closeErr)
	}
	return string(data), nil
}

func TestConcurrentPublishAndReplacementReadersSeeWholeEntries(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: DefaultMaxSize}
	key := Key("concurrent-publish-and-replace")
	const (
		readerCount = 8
		valuePrefix = "publish-"
	)
	format := func(iteration int) string { return fmt.Sprintf("%s%02d\n", valuePrefix, iteration) }
	allowed := make(map[string]struct{}, 17)
	for iteration := 0; iteration <= 16; iteration++ {
		allowed[format(iteration)] = struct{}{}
	}
	ready := make(chan struct{}, readerCount)
	start := make(chan struct{})
	done := make(chan struct{})
	readerErrors := make(chan error, readerCount)
	var readers sync.WaitGroup
	for range readerCount {
		readers.Add(1)
		go func() {
			defer readers.Done()
			ready <- struct{}{}
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}
				reader, _, err := c.Open(key)
				if err != nil {
					readerErrors <- err
					return
				}
				if reader == nil {
					continue
				}
				payload, err := readCachePayload(reader)
				if err != nil {
					readerErrors <- err
					return
				}
				if !strings.HasPrefix(payload, valuePrefix) {
					readerErrors <- fmt.Errorf("reader observed incomplete payload %q", payload)
					return
				}
				if _, ok := allowed[payload]; !ok {
					readerErrors <- fmt.Errorf("reader observed incomplete payload %q", payload)
					return
				}
			}
		}()
	}
	for range readerCount {
		<-ready
	}
	close(start)
	defer func() {
		close(done)
		readers.Wait()
		select {
		case err := <-readerErrors:
			t.Errorf("concurrent reader error: %v", err)
		default:
		}
	}()
	latest := format(0)
	first := latest
	putAndClose(t, c, key, latest)
	held, hit, err := c.Open(key)
	if err != nil || !hit {
		t.Fatalf("Open(held old entry) = hit %v, error %v; want hit", hit, err)
	}
	defer held.Close()
	for iteration := 1; iteration <= 16; iteration++ {
		latest = format(iteration)
		putAndClose(t, c, key, latest)
	}
	oldPayload, oldErr := readCachePayload(held)
	if oldErr != nil || oldPayload != first {
		t.Fatalf("held old reader payload/error = %q/%v, want %q and nil", oldPayload, oldErr, first)
	}
	reader, hit, err := c.Open(key)
	if err != nil || !hit {
		t.Fatalf("Open(final) = hit %v, error %v; want final hit", hit, err)
	}
	payload, readErr := readCachePayload(reader)
	if readErr != nil || payload != latest {
		t.Fatalf("final reader payload/error = %q/%v, want %q and nil", payload, readErr, latest)
	}
}

type failingReader struct {
	done bool
}

func (r failingReader) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	copy(buffer, "partial")
	return len("partial"), errors.New("source failed")
}

var _ io.Reader = failingReader{}
