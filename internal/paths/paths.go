// Package paths is the single source of LogiSwitch's well-known locations,
// so the installer (an elevated admin) and the service (LocalSystem) always
// agree on them.
package paths

import "path/filepath"

// Names of the files kept in the data directory.
const (
	ConfigName = "config.json"
	LogName    = "logiswitch.log"
	OldLogName = LogName + ".1"
)

// DataDir returns <ProgramData>\LogiSwitch.
func DataDir() string { return filepath.Join(ProgramData(), "LogiSwitch") }

// ConfigPath returns <ProgramData>\LogiSwitch\config.json.
func ConfigPath() string { return filepath.Join(DataDir(), ConfigName) }

// LogPath returns <ProgramData>\LogiSwitch\logiswitch.log.
func LogPath() string { return filepath.Join(DataDir(), LogName) }
