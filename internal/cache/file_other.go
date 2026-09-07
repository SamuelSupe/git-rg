//go:build !windows

package cache

import (
	"os"
	"time"
)

func openEntryFile(name string) (*os.File, error) {
	return os.Open(name)
}

func touchEntryFile(file *os.File, now time.Time) error {
	return os.Chtimes(file.Name(), now, now)
}
