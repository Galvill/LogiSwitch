//go:build !windows

package logging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// secureDir creates dir (mode 0700) and refuses a symlink. The Windows
// build applies a protected DACL instead.
func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("data dir: %s is not a directory (or is a link); refusing", dir)
	}
	return os.Chmod(dir, 0o700)
}

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

// trustedFile reports whether path is a regular file (no owner check off
// Windows).
func trustedFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errNotPlain
	}
	return nil
}

// ReadTrustedFile reads path if trustedFile accepts it.
func ReadTrustedFile(path string) ([]byte, error) {
	if err := trustedFile(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// resetToInherited is a no-op off Windows.
func resetToInherited(string) error { return nil }

// removeName removes the directory entry path; a missing path is fine.
func removeName(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// openLog opens path for writing (created 0600), replacing anything that is
// not a regular file first. The offset is not moved.
func openLog(path string) (*os.File, error) {
	if err := CheckFile(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
}
