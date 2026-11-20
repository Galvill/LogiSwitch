// Package logging provides a size-capped rotating log file writer.
package logging

import (
	"io"
	"os"
	"path/filepath"
	"sync"
)

// DefaultPath returns %ProgramData%\LogiSwitch\logiswitch.log.
func DefaultPath() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "LogiSwitch", "logiswitch.log")
}

type rotator struct {
	mu     sync.Mutex
	path   string
	max    int64
	f      *os.File
	size   int64
	closed bool
}

// OpenRotating opens (appending) a log file that is rotated to path+".1"
// when a write would push it past maxBytes. One old file is kept.
func OpenRotating(path string, maxBytes int64) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &rotator{path: path, max: maxBytes}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotator) open() error { return r.openFlags(0) }

// openFlags opens the log file, first replacing anything at path that is
// not a regular file (symlink, directory, device) with a fresh file.
func (r *rotator) openFlags(extra int) error {
	if fi, err := os.Lstat(r.path); err == nil && !fi.Mode().IsRegular() {
		if err := os.Remove(r.path); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|extra, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotator) rotate() error {
	// Drop the handle regardless of Close's result so a failed Close
	// cannot leave a dead writer behind.
	_ = r.f.Close()
	r.f = nil
	old := r.path + ".1"
	if err := os.Rename(r.path, old); err != nil {
		_ = os.Remove(old)
		if err := os.Rename(r.path, old); err != nil {
			// Could not rotate; keep appending, but truncate once the file
			// reaches twice the cap so it cannot grow without bound.
			extra := 0
			if r.size >= 2*r.max {
				extra = os.O_TRUNC
			}
			return r.openFlags(extra)
		}
	}
	return r.open()
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
