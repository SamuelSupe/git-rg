package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultMaxSize int64 = 512 << 20
const staleTemporaryAge = 24 * time.Hour
const pruneInterval = 5 * time.Minute
const pruneMarker = ".last-prune"
const entryMagic = "GITRG02\n"
const entryHeaderSize int64 = int64(len(entryMagic)) + sha256.Size

var ErrEntryTooLarge = errors.New("cache entry exceeds cache capacity")

type Cache struct {
	root    string
	enabled bool
	maxSize int64
	dirty   atomic.Bool
}

type Transaction struct {
	file        *os.File
	temporary   string
	destination string
	hash        hash.Hash
	written     int64
	maxSize     int64
	finished    bool
	cache       *Cache
}

func New(disabled bool) (*Cache, error) {
	if disabled {
		return &Cache{}, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user cache directory: %w", err)
	}
	base = filepath.Join(base, "git-rg")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return nil, fmt.Errorf("secure cache directory: %w", err)
	}
	return &Cache{root: base, enabled: true, maxSize: DefaultMaxSize}, nil
}

func Disabled() *Cache { return &Cache{} }

func (c *Cache) Enabled() bool { return c != nil && c.enabled }

func Key(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		io.WriteString(hash, part)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *Cache) Open(key string) (io.ReadCloser, bool, error) {
	return c.OpenContext(context.Background(), key)
}

func (c *Cache) OpenContext(ctx context.Context, key string) (io.ReadCloser, bool, error) {
	if !c.Enabled() {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	file, err := openEntryFile(c.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open cache entry: %w", err)
	}
	invalid := func(err error) (io.ReadCloser, bool, error) {
		file.Close()
		if removeErr := c.Remove(key); removeErr != nil {
			return nil, false, fmt.Errorf("verify cache entry: %v; remove invalid entry: %w", err, removeErr)
		}
		return nil, false, fmt.Errorf("verify cache entry: %w", err)
	}
	var header [entryHeaderSize]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return invalid(err)
	}
	if string(header[:len(entryMagic)]) != entryMagic {
		return invalid(errors.New("invalid cache format"))
	}
	actual := sha256.New()
	if _, err := copyContext(ctx, actual, file); err != nil {
		file.Close()
		return nil, false, fmt.Errorf("verify cache entry: %w", err)
	}
	if !bytes.Equal(actual.Sum(nil), header[len(entryMagic):]) {
		return invalid(errors.New("checksum mismatch"))
	}
	if _, err := file.Seek(entryHeaderSize, io.SeekStart); err != nil {
		file.Close()
		return nil, false, fmt.Errorf("rewind cache entry: %w", err)
	}
	now := time.Now()
	_ = touchEntryFile(file, now)
	return file, true, nil
}

func (c *Cache) Put(key string, source io.Reader) (io.ReadCloser, int64, error) {
	return c.PutContext(context.Background(), key, source)
}

func (c *Cache) PutContext(ctx context.Context, key string, source io.Reader) (io.ReadCloser, int64, error) {
	if !c.Enabled() {
		return nil, 0, errors.New("cache is disabled")
	}
	transaction, err := c.Begin(key)
	if err != nil {
		return nil, 0, err
	}
	defer transaction.Abort()
	written, err := copyContext(ctx, transaction, source)
	if err != nil {
		return nil, written, fmt.Errorf("write cache entry: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, written, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, written, err
	}
	reader, hit, err := c.OpenContext(ctx, key)
	if err != nil {
		return nil, written, err
	}
	if !hit {
		return nil, written, errors.New("cache entry disappeared after commit")
	}
	return reader, written, nil
}

func (c *Cache) Begin(key string) (*Transaction, error) {
	if !c.Enabled() {
		return nil, errors.New("cache is disabled")
	}
	destination := c.path(key)
	directory := filepath.Dir(destination)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create cache shard: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("secure cache shard: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create cache temporary file: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		os.Remove(temporary.Name())
		return nil, fmt.Errorf("secure cache temporary file: %w", err)
	}
	maxSize := c.maxSize
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	maxSize -= entryHeaderSize
	if maxSize <= 0 {
		temporary.Close()
		os.Remove(temporary.Name())
		return nil, errors.New("cache capacity is too small for an entry checksum")
	}
	if _, err := temporary.Write(make([]byte, entryHeaderSize)); err != nil {
		temporary.Close()
		os.Remove(temporary.Name())
		return nil, fmt.Errorf("reserve cache header: %w", err)
	}
	return &Transaction{file: temporary, temporary: temporary.Name(), destination: destination, hash: sha256.New(), maxSize: maxSize, cache: c}, nil
}

func (t *Transaction) Write(data []byte) (int, error) {
	if t == nil || t.finished {
		return 0, errors.New("cache transaction is closed")
	}
	if t.written+int64(len(data)) > t.maxSize {
		return 0, fmt.Errorf("%w (%d bytes)", ErrEntryTooLarge, t.maxSize)
	}
	written, err := t.file.Write(data)
	if written > 0 {
		_, _ = t.hash.Write(data[:written])
		t.written += int64(written)
	}
	return written, err
}

func (t *Transaction) Commit() error {
	if t == nil || t.finished {
		return errors.New("cache transaction is closed")
	}
	// The checksum and payload become visible together, including to other processes.
	header := append([]byte(entryMagic), t.hash.Sum(nil)...)
	if _, err := t.file.WriteAt(header, 0); err != nil {
		return fmt.Errorf("write cache header: %w", err)
	}
	if err := t.file.Sync(); err != nil {
		return fmt.Errorf("sync cache entry: %w", err)
	}
	if err := t.file.Close(); err != nil {
		return fmt.Errorf("close cache entry: %w", err)
	}
	if err := os.Rename(t.temporary, t.destination); err != nil {
		return fmt.Errorf("publish cache entry: %w", err)
	}
	t.finished = true
	t.cache.dirty.Store(true)
	return nil
}

func (t *Transaction) Abort() {
	if t == nil || t.finished {
		return
	}
	t.finished = true
	t.file.Close()
	os.Remove(t.temporary)
}

func (c *Cache) Remove(key string) error {
	if !c.Enabled() {
		return nil
	}
	if err := os.Remove(c.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (c *Cache) Prune() error {
	return c.PruneContext(context.Background())
}

// Successful writes trigger pruning at command completion. Read-only commands
// share a timestamp so each new process need not enumerate the entire cache.
func (c *Cache) PruneIfNeededContext(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.dirty.Load() {
		info, err := os.Stat(filepath.Join(c.root, pruneMarker))
		if err == nil {
			age := time.Since(info.ModTime())
			if age >= 0 && age < pruneInterval {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect cache prune timestamp: %w", err)
		}
	}
	return c.PruneContext(ctx)
}

func (c *Cache) PruneContext(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	type entry struct {
		path    string
		size    int64
		modTime time.Time
		legacy  bool
	}
	c.dirty.Store(false)
	complete := false
	defer func() {
		if !complete {
			c.dirty.Store(true)
		}
	}()
	var total int64
	entries := make([]entry, 0)
	now := time.Now()
	err := filepath.WalkDir(c.root, func(path string, item os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			return nil
		}
		if path == filepath.Join(c.root, pruneMarker) {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if strings.HasPrefix(item.Name(), ".tmp-") {
			if now.Sub(info.ModTime()) >= staleTemporaryAge {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove stale cache temporary file: %w", err)
				}
			}
			return nil
		}
		if strings.HasSuffix(item.Name(), ".sha256") {
			primary := strings.TrimSuffix(path, ".sha256")
			if _, err := os.Stat(primary); errors.Is(err, os.ErrNotExist) && now.Sub(info.ModTime()) >= staleTemporaryAge {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove orphan cache checksum: %w", err)
				}
			}
			return nil
		}
		size := info.Size()
		legacy := !strings.HasSuffix(item.Name(), ".entry")
		if legacy {
			if checksumInfo, err := os.Stat(path + ".sha256"); err == nil {
				size += checksumInfo.Size()
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		total += size
		entries = append(entries, entry{path: path, size: size, modTime: info.ModTime(), legacy: legacy})
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect cache: %w", err)
	}
	if total > c.maxSize {
		sort.Slice(entries, func(i, j int) bool { return entries[i].modTime.Before(entries[j].modTime) })
	}
	for _, item := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if total <= c.maxSize {
			break
		}
		if err := os.Remove(item.path); err == nil || errors.Is(err, os.ErrNotExist) {
			if item.legacy {
				if err := os.Remove(item.path + ".sha256"); err != nil && !errors.Is(err, os.ErrNotExist) {
					continue
				}
			}
			total -= item.size
		}
	}
	if total > c.maxSize {
		return fmt.Errorf("cache remains above %d bytes after pruning", c.maxSize)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.root, pruneMarker), []byte{'\n'}, 0o600); err != nil {
		return fmt.Errorf("record cache prune timestamp: %w", err)
	}
	complete = true
	return nil
}

func (c *Cache) path(key string) string {
	if len(key) < 2 {
		key = Key(key)
	}
	return filepath.Join(c.root, key[:2], key+".entry")
}

var copyBuffers = sync.Pool{New: func() any { return new([64 << 10]byte) }}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := copyBuffers.Get().(*[64 << 10]byte)
	defer copyBuffers.Put(buffer)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer[:])
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
		if read == 0 {
			return total, io.ErrNoProgress
		}
	}
}
