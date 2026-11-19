//go:build !windows

package hid

// SystemOpener is the platform HID backend. On this platform it is a stub.
type SystemOpener struct{}

// System is the platform HID backend.
var System Opener = SystemOpener{}

// Enumerate always returns ErrUnsupported on this platform.
func (SystemOpener) Enumerate(uint16) ([]Info, error) { return nil, ErrUnsupported }

// Open always returns ErrUnsupported on this platform.
func (SystemOpener) Open(string) (Device, error) { return nil, ErrUnsupported }
