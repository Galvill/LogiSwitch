// Package logging provides a size-capped rotating log file writer and the
// hardening of the LogiSwitch data directory.
package logging

import (
	"io"
	"os"
	"path/filepath"
	"sync"

	"logiswitch/internal/paths"
)

// DataDir returns <ProgramData>\LogiSwitch (see package paths).
func DataDir() string { return paths.DataDir() }

// DefaultPath returns <ProgramData>\LogiSwitch\logiswitch.log.
func DefaultPath() string { return paths.LogPath() }

type rotator struct {
	mu     sync.Mutex
	path   string
	max    int64
	f      *os.File
	size   int64
	closed bool
}

// OpenRotating opens (appending) a log file that is rotated to path+".1"
// when a write would push it past maxBytes. One old file is kept. Anything
// at path that is not a plain file (link, hard link, directory) is removed
// first (see CheckFile), and on Windows the opened handle itself is checked
// before use.
func OpenRotating(path string, maxBytes int64) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// path+".1" is never opened, only renamed onto (which replaces a link
	// rather than writing through it), so cleaning it is best effort.
	_ = CheckFile(path + ".1")
	r := &rotator{path: path, max: maxBytes}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// open opens the log file through the platform hook and positions the
// writer at its end.
func (r *rotator) open() error {
	f, err := openLog(r.path)
	if err != nil {
		return err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, size
	return nil
}

// rotate renames the current file to path+".1" and opens a fresh one. If
// the rename fails it keeps writing to the open handle, truncating it once
// it reaches twice the cap so it cannot grow without bound. The file is
// never reopened by name with O_TRUNC, so a name swapped under us cannot
// redirect the truncation.
func (r *rotator) rotate() error {
	old := r.path + ".1"
	_ = CheckFile(old)
	err := os.Rename(r.path, old)
	if err != nil {
		_ = removeName(old)
		err = os.Rename(r.path, old)
	}
	if err != nil {
		if r.size < 2*r.max {
			return nil
		}
		if err := r.f.Truncate(0); err != nil {
			return err
		}
		if _, err := r.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		r.size = 0
		return nil
	}
	// Drop the handle regardless of Close's result so a failed Close
	// cannot leave a dead writer behind.
	_ = r.f.Close()
	r.f = nil
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
