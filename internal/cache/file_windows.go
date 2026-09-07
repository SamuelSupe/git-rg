package cache

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func openEntryFile(name string) (*os.File, error) {
	return openSharedEntryFile(name, syscall.GENERIC_READ)
}

func openSharedEntryFile(name string, access uint32) (*os.File, error) {
	fullPath, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	// Preserve os.Open's support for long local and UNC cache paths.
	switch {
	case strings.HasPrefix(fullPath, `\\?\`), strings.HasPrefix(fullPath, `\\.\`):
	case strings.HasPrefix(fullPath, `\\`):
		fullPath = `\\?\UNC\` + fullPath[2:]
	default:
		fullPath = `\\?\` + fullPath
	}
	path, err := syscall.UTF16PtrFromString(fullPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	// Readers and timestamp updates must both allow replacement of the entry.
	handle, err := syscall.CreateFile(path, access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(handle), name), nil
}

func touchEntryFile(file *os.File, now time.Time) error {
	attributes, err := openSharedEntryFile(file.Name(), syscall.FILE_WRITE_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer attributes.Close()
	stamp := syscall.NsecToFiletime(now.UnixNano())
	return syscall.SetFileTime(syscall.Handle(attributes.Fd()), nil, &stamp, &stamp)
}
