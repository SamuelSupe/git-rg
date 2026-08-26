package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: checksumEncodedSize + 3}

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
	checksumInfo, err := os.Stat(c.checksumPath(newKey))
	if err != nil {
		t.Fatalf("Stat(new checksum) error = %v", err)
	}
	if got := checksumInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("new checksum mode = %#o, want 0600", got)
	}
	info, err := os.Stat(newFile)
	if err != nil {
		t.Fatalf("Stat(new) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("new entry mode = %#o, want 0600", got)
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

func TestOpenRejectsTamperedContentOrChecksumAndRemovesEntry(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, c *Cache, key string)
	}{
		{
			name: "content",
			edit: func(t *testing.T, c *Cache, key string) {
				data, err := os.ReadFile(c.path(key))
				if err != nil {
					t.Fatalf("ReadFile(content) error = %v", err)
				}
				data[0] ^= 0xff
				if err := os.WriteFile(c.path(key), data, 0o600); err != nil {
					t.Fatalf("WriteFile(content) error = %v", err)
				}
			},
		},
		{
			name: "checksum",
			edit: func(t *testing.T, c *Cache, key string) {
				if err := os.WriteFile(c.checksumPath(key), []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
					t.Fatalf("WriteFile(checksum) error = %v", err)
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
			if _, err := os.Stat(c.checksumPath(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tampered checksum stat error = %v, want removed", err)
			}
		})
	}
}

func TestOpenContextCanceledChecksumKeepsValidEntry(t *testing.T) {
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
	if _, err := os.Stat(c.checksumPath(key)); err != nil {
		t.Fatalf("valid cache checksum after canceled checksum = %v, want preserved", err)
	}
}

func TestPutRejectsSingleEntryOverCapacity(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: checksumEncodedSize + 4}
	key := Key("too-large")
	if _, written, err := c.Put(key, strings.NewReader("12345")); !errors.Is(err, ErrEntryTooLarge) || written != 0 {
		t.Fatalf("Put() = written %d, error %v; want entry-too-large before publish", written, err)
	}
	if _, hit, err := c.Open(key); err != nil || hit {
		t.Fatalf("Open(too-large) = hit %v, error %v; want miss", hit, err)
	}
	if _, err := os.Stat(c.checksumPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("too-large checksum stat error = %v, want not exists", err)
	}
}

func TestPruneRemovesStaleTemporaryAndOrphanChecksum(t *testing.T) {
	c := &Cache{root: t.TempDir(), enabled: true, maxSize: DefaultMaxSize}
	key := Key("orphan")
	directory := filepath.Dir(c.path(key))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	stale := filepath.Join(directory, ".tmp-stale")
	orphan := c.checksumPath(key)
	for _, path := range []string{stale, orphan} {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
		when := time.Now().Add(-staleTemporaryAge - time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("Chtimes(%q) error = %v", path, err)
		}
	}
	if err := c.Prune(); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	for _, path := range []string{stale, orphan} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale path %q stat error = %v, want removed", path, err)
		}
	}
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
