//go:build !windows

package logging

import (
	"errors"
	"io/fs"
	"os"
)

// SecureDataDir creates dir (mode 0700). The Windows build also applies a
// protected DACL; elsewhere a private mode is enough for tests and dev use.
func SecureDataDir(dir string) error { return os.MkdirAll(dir, 0o700) }

// CheckFile removes path if it exists and is not a regular file (symlink,
// directory, device), so that a later open cannot be redirected. A missing
// path is not an error.
func CheckFile(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().IsRegular() {
		return nil
	}
	return os.Remove(path)
}

// openLog opens path for writing (created 0600), replacing anything that is
// not a regular file first. The offset is not moved.
func openLog(path string) (*os.File, error) {
	if err := CheckFile(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
}
