package paths

import (
	"path/filepath"
	"testing"
)

func TestPathsShareOneRoot(t *testing.T) {
	t.Setenv("ProgramData", "/pd")
	dir := filepath.Join("/pd", "LogiSwitch")
	if DataDir() != dir || ConfigPath() != filepath.Join(dir, "config.json") || LogPath() != filepath.Join(dir, "logiswitch.log") {
		t.Fatalf("got %q %q %q", DataDir(), ConfigPath(), LogPath())
	}
}
