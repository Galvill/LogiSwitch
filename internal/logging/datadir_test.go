package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeepInDataDir(t *testing.T) {
	for name, want := range map[string]bool{
		"config.json": true, "logiswitch.log": true, "logiswitch.log.1": true,
		"CONFIG.JSON": true, // Windows names are case-insensitive
		"spike.log":   false, "config.json.bak": false, "logiswitch.log.2": false, "x": false,
	} {
		if got := keepInDataDir(name); got != want {
			t.Errorf("keepInDataDir(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSecureDataDirPrunesChildren(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "LogiSwitch")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(dir, "sub"), filepath.Join(dir, "empty")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(outside, "secret")
	write(filepath.Join(dir, "config.json"), "{}")
	write(filepath.Join(dir, "logiswitch.log"), "log\n")
	write(filepath.Join(dir, "spike.log"), "junk")
	write(filepath.Join(dir, "sub", "f"), "x")
	if err := os.Symlink(outside, filepath.Join(dir, "logiswitch.log.1")); err != nil {
		t.Skip(err)
	}
	warns, err := SecureDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left[e.Name()] = true
	}
	if !left["config.json"] || !left["logiswitch.log"] {
		t.Fatalf("kept files removed: %v", left)
	}
	for _, gone := range []string{"spike.log", "logiswitch.log.1", "empty"} {
		if left[gone] {
			t.Errorf("%s not removed", gone)
		}
	}
	if !left["sub"] || len(warns) != 1 || !strings.Contains(warns[0], "sub") {
		t.Fatalf("non-empty subdir: left=%v warns=%q", left, warns)
	}
	if b, _ := os.ReadFile(outside); string(b) != "secret" {
		t.Fatalf("link target modified: %q", b)
	}
}

func TestSecureDataDirRemovesLinkedConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "LogiSwitch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "evil.json")
	if err := os.WriteFile(target, []byte(`{"logLevel":"debug"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "config.json")); err != nil {
		t.Skip(err)
	}
	warns, err := SecureDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("linked config.json kept: %v", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "removed, using defaults") {
		t.Fatalf("warns = %q", warns)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target removed: %v", err)
	}
}

func TestReadTrustedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if _, err := ReadTrustedFile(p); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadTrustedFile(p); err != nil || string(b) != "{}" {
		t.Fatalf("got %q %v", b, err)
	}
	l := filepath.Join(dir, "link.json")
	if err := os.Symlink(p, l); err != nil {
		t.Skip(err)
	}
	if _, err := ReadTrustedFile(l); err == nil {
		t.Fatal("read through a symlink")
	}
}
