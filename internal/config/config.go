// Package config loads the optional LogiSwitch config file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds runtime settings.
type Config struct {
	MouseSlotFor [3]int // 0-based; index = keyboard slot (0-based)
	PollInterval time.Duration
	LogLevel     slog.Level
}

// Default returns identity mapping, 500ms polling and info logging.
func Default() Config {
	return Config{
		MouseSlotFor: [3]int{0, 1, 2},
		PollInterval: 500 * time.Millisecond,
		LogLevel:     slog.LevelInfo,
	}
}

// DefaultPath returns %ProgramData%\LogiSwitch\config.json.
func DefaultPath() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "LogiSwitch", "config.json")
}

type fileFormat struct {
	MouseSlotForKeyboardSlot map[string]int `json:"mouseSlotForKeyboardSlot"`
	PollIntervalMs           *int           `json:"pollIntervalMs"`
	LogLevel                 *string        `json:"logLevel"`
}

// Load reads path. A missing file yields Default().
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
		return c, fmt.Errorf("config: %w", err)
	}
	var f fileFormat
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Default(), fmt.Errorf("config: %w", err)
	}
	for k, v := range f.MouseSlotForKeyboardSlot {
		n, err := strconv.Atoi(k)
		if err != nil || n < 1 || n > 3 {
			return Default(), fmt.Errorf("config: mouseSlotForKeyboardSlot key %q: slot must be 1-3", k)
		}
		if v < 1 || v > 3 {
			return Default(), fmt.Errorf("config: mouseSlotForKeyboardSlot key %q: value %d: slot must be 1-3", k, v)
		}
		c.MouseSlotFor[n-1] = v - 1
	}
	if f.PollIntervalMs != nil {
		if *f.PollIntervalMs < 200 {
			return Default(), fmt.Errorf("config: pollIntervalMs %d: must be >= 200", *f.PollIntervalMs)
		}
		c.PollInterval = time.Duration(*f.PollIntervalMs) * time.Millisecond
	}
	if f.LogLevel != nil {
		switch *f.LogLevel {
		case "debug":
			c.LogLevel = slog.LevelDebug
		case "info":
			c.LogLevel = slog.LevelInfo
		case "warn":
			c.LogLevel = slog.LevelWarn
		case "error":
			c.LogLevel = slog.LevelError
		default:
			return Default(), fmt.Errorf("config: logLevel %q: must be debug, info, warn or error", *f.LogLevel)
		}
	}
	return c, nil
}
