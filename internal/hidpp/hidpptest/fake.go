// Package hidpptest provides a simulated HID++ 2.0 device for tests.
package hidpptest

import (
	"errors"
	"sync"
	"time"

	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp"
)

const (
	featChangeHost = 0x1814
	featReprog     = 0x1B04
	outQueue       = 256
)

type cidInfo struct {
	cid, tid uint16
	flags    byte
}

// Fake is a simulated HID++ device implementing hid.Device. It is safe for
// concurrent use.
type Fake struct {
	mu           sync.Mutex
	chIdx        byte // CHANGE_HOST feature index
	rpIdx        byte // REPROG feature index
	cids         []cidInfo
	host         int
	setHostCalls []int
	diverted     map[uint16]bool
	connected    bool
	closed       bool
	delay        time.Duration
	// timeouts: a Write whose delay exceeds its timeout returns at the
	// timeout with hid.ErrWriteTimeout, yet still takes effect (cancelled
	// but delivered).
	timeouts bool

	out    chan []byte
	closeC chan struct{}
}

func newFake(chIdx, rpIdx byte, host int, cids []cidInfo) *Fake {
	return &Fake{
		chIdx: chIdx, rpIdx: rpIdx, cids: cids, host: host,
		diverted:  map[uint16]bool{},
		connected: true,
		out:       make(chan []byte, outQueue),
		closeC:    make(chan struct{}),
	}
}

// NewKeyboard returns a fake keyboard with CHANGE_HOST at index 9 and
// REPROG_CONTROLS_V4 at index 8 exposing host-switch CIDs D1-D3.
func NewKeyboard(currentHost int) *Fake {
	return newFake(9, 8, currentHost, []cidInfo{
		{0xD1, 0x00D1, 0x64}, {0xD2, 0x00D2, 0x64}, {0xD3, 0x00D3, 0x64},
	})
}

// NewMouse returns a fake mouse with CHANGE_HOST at index 10 and REPROG at
// index 9 exposing no host-switch CIDs.
func NewMouse(currentHost int) *Fake {
	return newFake(10, 9, currentHost, nil)
}

func (f *Fake) drain() {
	for {
		select {
		case <-f.out:
		default:
			return
		}
	}
}

// Disconnect makes the device unreachable: writes fail, reads time out.
func (f *Fake) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnectLocked()
}

func (f *Fake) disconnectLocked() {
	f.connected = false
	f.drain()
}

// Reconnect makes the device reachable again and clears diversion.
func (f *Fake) Reconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = true
	f.diverted = map[uint16]bool{}
}

// SetWriteDelay sets a delay applied to every Write.
func (f *Fake) SetWriteDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}

// SetWriteTimeouts makes a Write whose delay exceeds its timeout return
// hid.ErrWriteTimeout at the timeout while still applying the write, as a
// cancelled report that the device nevertheless received.
func (f *Fake) SetWriteTimeouts(on bool) {
	f.mu.Lock()
	f.timeouts = on
	f.mu.Unlock()
}

// Press emits press and release notifications for cid if it is diverted.
func (f *Fake) Press(cid uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected || f.closed || !f.diverted[cid] {
		return
	}
	for _, c := range []uint16{cid, 0} {
		m := hidpp.Message{Dev: 0xFF, Feature: f.rpIdx}
		m.Params[0], m.Params[1] = byte(c>>8), byte(c)
		f.enqueue(m)
	}
}

// Diverted reports whether cid is currently diverted.
func (f *Fake) Diverted(cid uint16) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diverted[cid]
}

// Host returns the current host slot.
func (f *Fake) Host() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.host
}

// SetHostCalls returns every slot passed to setHost, in order.
func (f *Fake) SetHostCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.setHostCalls...)
}

func (f *Fake) enqueue(m hidpp.Message) {
	select {
	case f.out <- hidpp.Encode(m):
	default:
	}
}

// Read implements hid.Device.
func (f *Fake) Read(buf []byte, timeout time.Duration) (int, error) {
	f.mu.Lock()
	closed, connected := f.closed, f.connected
	f.mu.Unlock()
	if closed {
		return 0, hid.ErrClosed
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	if !connected {
		select {
		case <-t.C:
			return 0, nil
		case <-f.closeC:
			return 0, hid.ErrClosed
		}
	}
	select {
	case b := <-f.out:
		return copy(buf, b), nil
	case <-t.C:
		return 0, nil
	case <-f.closeC:
		return 0, hid.ErrClosed
	}
}

// Write implements hid.Device.
func (f *Fake) Write(report []byte, timeout time.Duration) error {
	f.mu.Lock()
	d, honour := f.delay, f.timeouts
	f.mu.Unlock()
	timedOut := false
	if honour && timeout >= 0 && d > timeout {
		d, timedOut = timeout, true
	}
	if d > 0 {
		select {
		case <-time.After(d):
		case <-f.closeC:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return hid.ErrClosed
	}
	if !f.connected {
		return errors.New("hidpptest: device disconnected")
	}
	m, ok := hidpp.Decode(report)
	if !ok {
		return errors.New("hidpptest: malformed report")
	}
	f.handle(m)
	if timedOut {
		return hid.ErrWriteTimeout
	}
	return nil
}

// Close implements hid.Device.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.closeC)
	}
	return nil
}

func (f *Fake) reply(req hidpp.Message, params ...byte) {
	r := hidpp.Message{Dev: req.Dev, Feature: req.Feature, Function: req.Function, SWID: req.SWID}
	copy(r.Params[:], params)
	f.enqueue(r)
}

func (f *Fake) errReply(req hidpp.Message, code byte) {
	r := hidpp.Message{Dev: req.Dev, Feature: 0xFF, Function: 0, SWID: req.SWID}
	r.Params[0] = req.Feature
	r.Params[1] = req.Function<<4 | req.SWID
	r.Params[2] = code
	f.enqueue(r)
}

func (f *Fake) cidByIndex(i int) (cidInfo, bool) {
	if i < 0 || i >= len(f.cids) {
		return cidInfo{}, false
	}
	return f.cids[i], true
}

func (f *Fake) hasCID(cid uint16) bool {
	for _, c := range f.cids {
		if c.cid == cid {
			return true
		}
	}
	return false
}

func (f *Fake) handle(m hidpp.Message) {
	p := m.Params
	cid := uint16(p[0])<<8 | uint16(p[1])
	switch m.Feature {
	case 0:
		if m.Function != 0 {
			f.errReply(m, 0x07)
			return
		}
		var idx byte
		switch uint16(p[0])<<8 | uint16(p[1]) {
		case featChangeHost:
			idx = f.chIdx
		case featReprog:
			idx = f.rpIdx
		}
		f.reply(m, idx, 0, 1)
	case f.chIdx:
		switch m.Function {
		case 0:
			f.reply(m, 3, byte(f.host))
		case 1:
			f.host = int(p[0])
			f.setHostCalls = append(f.setHostCalls, f.host)
			f.disconnectLocked()
		default:
			f.errReply(m, 0x07)
		}
	case f.rpIdx:
		switch m.Function {
		case 0:
			f.reply(m, byte(len(f.cids)))
		case 1:
			c, ok := f.cidByIndex(int(p[0]))
			if !ok {
				f.errReply(m, 0x02)
				return
			}
			f.reply(m, byte(c.cid>>8), byte(c.cid), byte(c.tid>>8), byte(c.tid), c.flags)
		case 2:
			if !f.hasCID(cid) {
				f.errReply(m, 0x02)
				return
			}
			var fl byte
			if f.diverted[cid] {
				fl = 1
			}
			f.reply(m, p[0], p[1], fl)
		case 3:
			if !f.hasCID(cid) {
				f.errReply(m, 0x02)
				return
			}
			if p[2]&2 != 0 { // divert-valid
				f.diverted[cid] = p[2]&1 != 0
			}
			f.reply(m, p[0], p[1], p[2])
		default:
			f.errReply(m, 0x07)
		}
	default:
		f.errReply(m, 0x02)
	}
}
