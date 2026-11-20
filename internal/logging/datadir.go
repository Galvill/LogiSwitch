package logging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"logiswitch/internal/paths"
)

// errUntrustedOwner: the file is owned by someone other than SYSTEM or
// Administrators (Windows only).
var errUntrustedOwner = errors.New("not owned by SYSTEM/Administrators")

// errNotPlain: a reparse point, directory, or file with several hard links.
var errNotPlain = errors.New("not a plain file (link, hard link or directory)")

// keepInDataDir reports whether name is one of the files LogiSwitch keeps
// in its data directory. Windows names are case-insensitive.
func keepInDataDir(name string) bool {
	for _, k := range []string{paths.ConfigName, paths.LogName, paths.OldLogName} {
		if strings.EqualFold(name, k) {
			return true
		}
	}
	return false
}

// SecureDataDir creates or repairs the data directory (see secureDir), then
// prunes it: every entry other than config.json, logiswitch.log and
// logiswitch.log.1 is deleted; an untrusted config.json (not a plain file,
// or on Windows not owned by SYSTEM/Administrators) is deleted with a
// warning; unsafe log entries are removed (CheckFile); and the surviving
// files are reset to owner Administrators with only inherited ACEs. A
// non-empty subdirectory that cannot be removed is left with a warning (the
// service never looks inside it); any other failure is an error, on which
// callers must fail closed.
func SecureDataDir(dir string) (warnings []string, err error) {
	if err := secureDir(dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("data dir: list %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(dir, name)
		switch {
		case !keepInDataDir(name):
			if err := removeName(p); err != nil {
				if e.IsDir() {
					warnings = append(warnings, fmt.Sprintf("data dir: could not remove subdirectory %s: %v", p, err))
					continue
				}
				return warnings, fmt.Errorf("data dir: remove %s: %w", p, err)
			}
			continue
		case strings.EqualFold(name, paths.ConfigName):
			if err := trustedFile(p); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if rerr := removeName(p); rerr != nil {
					return warnings, fmt.Errorf("data dir: remove untrusted %s: %w", p, rerr)
				}
				if errors.Is(err, errUntrustedOwner) {
					warnings = append(warnings, "config.json not owned by SYSTEM/Administrators — removed, using defaults")
				} else {
					warnings = append(warnings, fmt.Sprintf("config.json rejected (%v) — removed, using defaults", err))
				}
				continue
			}
		default:
			if err := CheckFile(p); err != nil {
				return warnings, err
			}
		}
		if err := resetToInherited(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return warnings, fmt.Errorf("data dir: reset security of %s: %w", p, err)
		}
	}
	return warnings, nil
}
