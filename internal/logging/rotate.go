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
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// OpenRotating opens (appending) a log file that is rotated to path+".1"
// when a write would push it past maxBytes. One old file is kept.
func OpenRotating(path string, maxBytes int64) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &rotator{path: path, max: maxBytes}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotator) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	if err := r.f.Close(); err != nil {
		return err
	}
	r.f = nil
	old := r.path + ".1"
	if err := os.Rename(r.path, old); err != nil {
		_ = os.Remove(old)
		if err := os.Rename(r.path, old); err != nil {
			// Could not rotate; reopen and keep appending.
			if oerr := r.open(); oerr != nil {
				return oerr
			}
			return nil
		}
	}
	return r.open()
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
