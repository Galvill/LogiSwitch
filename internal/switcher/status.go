package switcher

import (
	"errors"
	"slices"
	"strings"
)

// DeviceStatus describes one registered device.
type DeviceStatus struct {
	Product, Path, Kind      string // Kind: "keyboard" | "mouse"
	ChangeHostIdx, ReprogIdx byte
	CurrentHost              int  // 1-based; 0 if unknown
	Diverted                 bool // keyboards: D1-D3 currently diverted; mice: false
	// DivertedBefore: keyboards: D1 was already diverted when this
	// Switcher opened the keyboard, i.e. before it diverted the keys itself
	// (Logi Options+ or another, possibly crashed, LogiSwitch holds them).
	DivertedBefore bool
}

// Status reports every registered device, sorted by Kind then Path. Owner
// goroutine only; call Tick and Settle first for a fresh picture.
func (s *Switcher) Status() []DeviceStatus {
	out := make([]DeviceStatus, 0, len(s.devs))
	for _, d := range s.devs {
		out = append(out, DeviceStatus{
			Product: d.product, Path: d.path, Kind: d.role.String(),
			ChangeHostIdx: d.chIdx, ReprogIdx: d.rpIdx,
			CurrentHost:    d.host + 1, // -1 (unknown) becomes 0
			Diverted:       d.role == roleKeyboard && d.diverted,
			DivertedBefore: d.role == roleKeyboard && d.divertedBefore,
		})
	}
	slices.SortFunc(out, func(a, b DeviceStatus) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// SetAll moves every device to slot (1-based, 1-3): the mice to the
// configured mouse slot for it and the keyboards to slot itself, through the
// same path as a key press (the keyboard never waits for the mice). Devices
// already there are skipped. It discovers devices first if none are
// registered, waits for the mouse writes, and does not Release. Owner
// goroutine only.
func (s *Switcher) SetAll(slot int) error {
	if slot < 1 || slot > 3 {
		return errors.New("slot must be 1-3")
	}
	if len(s.devs) == 0 {
		s.Tick()
		s.Settle()
	}
	if len(s.devs) == 0 {
		return errors.New("no devices found")
	}
	k := slot - 1
	var kbs []*device
	for _, d := range s.devs {
		if d.role == roleKeyboard && d.host != k {
			kbs = append(kbs, d)
		}
	}
	slices.SortFunc(kbs, func(a, b *device) int { return strings.Compare(a.path, b.path) })
	sent := s.startSwitch(kbs, k, s.cfg.MouseSlotFor[k])
	s.Settle()
	if len(kbs) > 0 && sent == 0 {
		return errors.New("keyboard setHost failed")
	}
	return nil
}
