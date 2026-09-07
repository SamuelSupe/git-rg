package cache

import (
	"os"
	"path/filepath"
	"time"
)

// Root operations share delete access and use POSIX replacement semantics,
// allowing active readers to retain the old entry while a new one is published.
func openEntryFile(name string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.Base(name))
}

func touchEntryFile(file *os.File, now time.Time) error {
	root, err := os.OpenRoot(filepath.Dir(file.Name()))
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Chtimes(filepath.Base(file.Name()), now, now)
}

func replaceEntryFile(temporary, destination string) error {
	// Begin creates the temporary file in the destination's cache shard.
	root, err := os.OpenRoot(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Rename(filepath.Base(temporary), filepath.Base(destination))
}
