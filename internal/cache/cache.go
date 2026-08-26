package cache

import (
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
	"time"
)

const DefaultMaxSize int64 = 512 << 20
const staleTemporaryAge = 24 * time.Hour
const checksumEncodedSize int64 = sha256.Size*2 + 1

var ErrEntryTooLarge = errors.New("cache entry exceeds cache capacity")

type Cache struct {
	root    string
	enabled bool
	maxSize int64
}

type Transaction struct {
	file        *os.File
	temporary   string
	destination string
	hash        hash.Hash
	written     int64
	maxSize     int64
	finished    bool
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
	file, err := os.Open(c.path(key))
	if errors.Is(err, os.ErrNotExist) {
		if removeErr := os.Remove(c.checksumPath(key)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, false, fmt.Errorf("remove orphan cache checksum: %w", removeErr)
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open cache entry: %w", err)
	}
	expected, err := readChecksum(c.checksumPath(key))
	if err != nil {
		file.Close()
		removeErr := c.Remove(key)
		if removeErr != nil {
			return nil, false, fmt.Errorf("verify cache entry: %v; remove invalid entry: %w", err, removeErr)
		}
		return nil, false, fmt.Errorf("verify cache entry: %w", err)
	}
	actual := sha256.New()
	if _, err := copyContext(ctx, actual, file); err != nil {
		file.Close()
		return nil, false, fmt.Errorf("verify cache entry: %w", err)
	}
	if !equalBytes(actual.Sum(nil), expected) {
		file.Close()
		removeErr := c.Remove(key)
		if removeErr != nil {
			return nil, false, fmt.Errorf("verify cache entry: checksum mismatch; remove invalid entry: %w", removeErr)
		}
		return nil, false, errors.New("verify cache entry: checksum mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, false, fmt.Errorf("rewind cache entry: %w", err)
	}
	now := time.Now()
	_ = os.Chtimes(file.Name(), now, now)
	_ = os.Chtimes(c.checksumPath(key), now, now)
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
	maxSize -= checksumEncodedSize
	if maxSize <= 0 {
		temporary.Close()
		os.Remove(temporary.Name())
		return nil, errors.New("cache capacity is too small for an entry checksum")
	}
	return &Transaction{file: temporary, temporary: temporary.Name(), destination: destination, hash: sha256.New(), maxSize: maxSize}, nil
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
	if err := t.file.Sync(); err != nil {
		return fmt.Errorf("sync cache entry: %w", err)
	}
	if err := t.file.Close(); err != nil {
		return fmt.Errorf("close cache entry: %w", err)
	}
	checksumTemporary := t.temporary + ".sha256"
	if err := writeChecksum(checksumTemporary, t.hash.Sum(nil)); err != nil {
		return err
	}
	if err := os.Rename(t.temporary, t.destination); err != nil {
		if _, statErr := os.Stat(t.destination); statErr == nil {
			os.Remove(t.temporary)
			os.Remove(checksumTemporary)
			t.finished = true
			return nil
		}
		os.Remove(checksumTemporary)
		return fmt.Errorf("publish cache entry: %w", err)
	}
	if err := os.Rename(checksumTemporary, t.destination+".sha256"); err != nil {
		os.Remove(t.destination)
		os.Remove(t.destination + ".sha256")
		os.Remove(checksumTemporary)
		return fmt.Errorf("publish cache checksum: %w", err)
	}
	t.finished = true
	return nil
}

func (t *Transaction) Abort() {
	if t == nil || t.finished {
		return
	}
	t.finished = true
	t.file.Close()
	os.Remove(t.temporary)
	os.Remove(t.temporary + ".sha256")
}

func (c *Cache) Remove(key string) error {
	if !c.Enabled() {
		return nil
	}
	var result error
	for _, target := range []string{c.path(key), c.checksumPath(key)} {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (c *Cache) Prune() error {
	return c.PruneContext(context.Background())
}

func (c *Cache) PruneContext(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	type entry struct {
		path    string
		size    int64
		modTime time.Time
	}
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
		if checksumInfo, err := os.Stat(path + ".sha256"); err == nil {
			size += checksumInfo.Size()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total += size
		entries = append(entries, entry{path: path, size: size, modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect cache: %w", err)
	}
	if total <= c.maxSize {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modTime.Before(entries[j].modTime) })
	for _, item := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if total <= c.maxSize {
			break
		}
		key := filepath.Base(item.path)
		if err := c.Remove(key); err == nil {
			total -= item.size
		}
	}
	if total > c.maxSize {
		return fmt.Errorf("cache remains above %d bytes after pruning", c.maxSize)
	}
	return nil
}

func (c *Cache) path(key string) string {
	if len(key) < 2 {
		key = Key(key)
	}
	return filepath.Join(c.root, key[:2], key)
}

func (c *Cache) checksumPath(key string) string {
	return c.path(key) + ".sha256"
}

func writeChecksum(path string, checksum []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create cache checksum: %w", err)
	}
	remove := true
	defer func() {
		file.Close()
		if remove {
			os.Remove(path)
		}
	}()
	if _, err := fmt.Fprintf(file, "%x\n", checksum); err != nil {
		return fmt.Errorf("write cache checksum: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync cache checksum: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close cache checksum: %w", err)
	}
	remove = false
	return nil
}

func readChecksum(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, sha256.Size*2+2))
	if err != nil || len(encoded) > sha256.Size*2+1 {
		return nil, errors.New("invalid cache checksum")
	}
	value := strings.TrimSpace(string(encoded))
	if len(value) != sha256.Size*2 {
		return nil, errors.New("invalid cache checksum")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, errors.New("invalid cache checksum")
	}
	return decoded, nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
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
