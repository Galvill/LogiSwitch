package main

import (
	"path/filepath"
	"strings"
)

// samePath reports whether a and b name the same location on Windows,
// where paths are case-insensitive. Both are expected to be absolute and
// symlink-resolved; install uses it to skip copying the exe onto itself.
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
