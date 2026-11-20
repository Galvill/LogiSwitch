// Package hid defines the minimal HID device abstraction used by LogiSwitch.
package hid

import (
	"errors"
	"time"
)

// Info describes an enumerated HID interface.
type Info struct {
	Path, Product string
	VendorID      uint16
	ProductID     uint16
	UsagePage     uint16
	Usage         uint16
	InputLen      int
	OutputLen     int
}

// Device is an open HID interface.
type Device interface {
	// Read reads one input report; returns (0, nil) on timeout.
	Read(buf []byte, timeout time.Duration) (int, error)
	// Write sends one output report.
	Write(report []byte, timeout time.Duration) error
	Close() error
}

// Opener enumerates and opens HID interfaces.
type Opener interface {
	Enumerate(vid uint16) ([]Info, error)
	Open(path string) (Device, error)
}

var (
	// ErrUnsupported is returned on platforms without HID support.
	ErrUnsupported = errors.New("hid: unsupported on this platform")
	// ErrClosed is returned for operations on a closed device.
	ErrClosed = errors.New("hid: device closed")
	// ErrWriteTimeout is returned when a write did not complete in time. The
	// write was cancelled, but the report may still have been delivered.
	ErrWriteTimeout = errors.New("hid: write timeout")
)
