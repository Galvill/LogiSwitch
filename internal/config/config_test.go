package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c != Default() {
		t.Fatalf("got %+v want default", c)
	}
	d := Default()
	if d.MouseSlotFor != [3]int{0, 1, 2} || d.PollInterval != 500*time.Millisecond || d.LogLevel != slog.LevelInfo {
		t.Fatalf("bad default %+v", d)
	}
}

func TestLoadPartial(t *testing.T) {
	c, err := Load(writeCfg(t, `{"logLevel":"debug"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.LogLevel = slog.LevelDebug
	if c != want {
		t.Fatalf("got %+v want %+v", c, want)
	}
}

func TestLoadMappingConvertsToZeroBased(t *testing.T) {
	c, err := Load(writeCfg(t, `{"mouseSlotForKeyboardSlot":{"2":3},"pollIntervalMs":300}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MouseSlotFor != [3]int{0, 2, 2} {
		t.Fatalf("got %v", c.MouseSlotFor)
	}
	if c.PollInterval != 300*time.Millisecond {
		t.Fatalf("poll %v", c.PollInterval)
	}
}

func TestLoadRejectsBadSlot(t *testing.T) {
	for _, body := range []string{
		`{"mouseSlotForKeyboardSlot":{"4":1}}`,
		`{"mouseSlotForKeyboardSlot":{"0":1}}`,
		`{"mouseSlotForKeyboardSlot":{"x":1}}`,
		`{"mouseSlotForKeyboardSlot":{"1":4}}`,
		`{"mouseSlotForKeyboardSlot":{"1":0}}`,
		`{"logLevel":"loud"}`,
	} {
		if _, err := Load(writeCfg(t, body)); err == nil {
			t.Errorf("%s: expected error", body)
		}
	}
	_, err := Load(writeCfg(t, `{"mouseSlotForKeyboardSlot":{"4":1}}`))
	if err == nil || !strings.Contains(err.Error(), `"4"`) {
		t.Fatalf("error should name key: %v", err)
	}
}

func TestLoadRejectsShortPoll(t *testing.T) {
	if _, err := Load(writeCfg(t, `{"pollIntervalMs":199}`)); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Load(writeCfg(t, `{"pollIntervalMs":200}`)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	if _, err := Load(writeCfg(t, `{"bogus":1}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("ProgramData", filepath.Join("x", "PD"))
	want := filepath.Join("x", "PD", "LogiSwitch", "config.json")
	if got := DefaultPath(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
