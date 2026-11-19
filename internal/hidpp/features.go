package hidpp

import (
	"errors"
	"time"
)

// HID++ feature IDs.
const (
	FeatChangeHost = 0x1814
	FeatReprogV4   = 0x1B04
)

// DefaultTimeout is the reply timeout for calls that take no timeout argument.
const DefaultTimeout = time.Second

// HostSwitchCIDs are the control IDs of the three Easy-Switch keys.
var HostSwitchCIDs = [3]uint16{0xD1, 0xD2, 0xD3}

// ErrNoFeature is returned when the device does not implement a feature.
var ErrNoFeature = errors.New("hidpp: feature not supported")

// Control describes one REPROG_CONTROLS_V4 control.
type Control struct {
	CID   uint16
	Flags byte
}

// FeatureIndex resolves a feature ID to its index via the root feature.
func (c *Conn) FeatureIndex(id uint16) (byte, error) {
	r, err := c.Request(0, 0, []byte{byte(id >> 8), byte(id)}, DefaultTimeout)
	if err != nil {
		return 0, err
	}
	if r.Params[0] == 0 {
		return 0, ErrNoFeature
	}
	return r.Params[0], nil
}

// HostInfo returns the number of hosts and the current (0-based) host.
func (c *Conn) HostInfo(fi byte) (n, current int, err error) {
	r, err := c.Request(fi, 0, nil, DefaultTimeout)
	if err != nil {
		return 0, 0, err
	}
	return int(r.Params[0]), int(r.Params[1]), nil
}

// SetHost switches the device to slot. The device usually drops the link
// without replying, so no reply is awaited; timeout bounds the write.
func (c *Conn) SetHost(fi byte, slot int, timeout time.Duration) error {
	return c.Send(fi, 1, []byte{byte(slot)}, timeout)
}

// Controls lists all controls in index order.
func (c *Conn) Controls(fi byte) ([]Control, error) {
	r, err := c.Request(fi, 0, nil, DefaultTimeout)
	if err != nil {
		return nil, err
	}
	n := int(r.Params[0])
	out := make([]Control, 0, n)
	for i := 0; i < n; i++ {
		r, err := c.Request(fi, 1, []byte{byte(i)}, DefaultTimeout)
		if err != nil {
			return nil, err
		}
		out = append(out, Control{CID: uint16(r.Params[0])<<8 | uint16(r.Params[1]), Flags: r.Params[4]})
	}
	return out, nil
}

// Diverted reports whether cid is currently diverted to software.
func (c *Conn) Diverted(fi byte, cid uint16) (bool, error) {
	r, err := c.Request(fi, 2, []byte{byte(cid >> 8), byte(cid)}, DefaultTimeout)
	if err != nil {
		return false, err
	}
	return r.Params[2]&1 != 0, nil
}

// SetDiverted turns diversion of cid on or off.
func (c *Conn) SetDiverted(fi byte, cid uint16, on bool) error {
	flags := byte(0x02)
	if on {
		flags = 0x03
	}
	_, err := c.Request(fi, 3, []byte{byte(cid >> 8), byte(cid), flags}, DefaultTimeout)
	return err
}

// DivertedCIDs returns the non-zero CIDs of a divertedButtonsEvent (fn 0,
// SWID 0). Callers must check m.Feature themselves.
func DivertedCIDs(m Message) []uint16 {
	if m.Function != 0 || m.SWID != 0 {
		return nil
	}
	var out []uint16
	for i := 0; i < 8; i += 2 {
		if cid := uint16(m.Params[i])<<8 | uint16(m.Params[i+1]); cid != 0 {
			out = append(out, cid)
		}
	}
	return out
}
