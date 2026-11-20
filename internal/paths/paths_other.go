//go:build !windows

package paths

import (
	"errors"
	"os"
)

// ProgramData returns $ProgramData (fallback C:\ProgramData); non-Windows
// builds only use it in tests and dev runs.
func ProgramData() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return pd
	}
	return `C:\ProgramData`
}

// ProgramFiles is only meaningful on Windows.
func ProgramFiles() (string, error) {
	return "", errors.New("paths: no Program Files on this platform")
}
