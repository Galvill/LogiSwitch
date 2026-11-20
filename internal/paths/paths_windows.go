//go:build windows

package paths

import "golang.org/x/sys/windows"

// ProgramData returns the ProgramData known folder (not the environment
// variable, which a caller's environment could change), falling back to
// C:\ProgramData.
func ProgramData() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0); err == nil && p != "" {
		return p
	}
	return `C:\ProgramData`
}

// ProgramFiles returns the Program Files known folder.
func ProgramFiles() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
}
