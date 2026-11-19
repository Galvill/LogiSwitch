package hidpp

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"logiswitch/internal/hid"
)

const (
	requestSWID  = 0x07
	readTimeout  = 100 * time.Millisecond
	writeTimeout = time.Second
	notifBuffer  = 16
)

// ErrTimeout is returned when no matching reply arrives in time.
var ErrTimeout = errors.New("hidpp: timeout waiting for reply")

// DeviceError is an error reply from the device (HID++ 2.0 feature 0xFF or HID++ 1.0 0x8F).
type DeviceError struct{ Code byte }

func (e DeviceError) Error() string { return fmt.Sprintf("hidpp: device error 0x%02x", e.Code) }

type pending struct {
	feat, fnSW byte
	ch         chan result // buffered 1
}

type result struct {
	msg Message
	err error
}

// Conn is a HID++ connection to one device index over a hid.Device.
// It allows one request in flight at a time.
type Conn struct {
	dev   hid.Device
	index byte

	reqMu sync.Mutex // serialises requests

	mu      sync.Mutex // guards pend, err
	pend    *pending
	err     error // sticky; set on read failure or Close
	notifs  chan Message
	stop    chan struct{}
	done    chan struct{}
	closeMu sync.Once
}

// NewConn starts the reader goroutine for device index dev.
func NewConn(d hid.Device, dev byte) *Conn {
	c := &Conn{
		dev:    d,
		index:  dev,
		notifs: make(chan Message, notifBuffer),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Notifications returns unsolicited messages (SWID 0). It is closed when the
// connection fails or is closed.
func (c *Conn) Notifications() <-chan Message { return c.notifs }

// fail records the first terminal error and fails any pending request.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	err = c.err
	p := c.pend
	c.pend = nil
	c.mu.Unlock()
	if p != nil {
		p.ch <- result{err: err}
	}
}

func (c *Conn) readLoop() {
	defer close(c.done)
	defer close(c.notifs)
	buf := make([]byte, 64)
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		n, err := c.dev.Read(buf, readTimeout)
		if err != nil {
			c.fail(err)
			return
		}
		if n == 0 {
			continue
		}
		m, ok := Decode(buf[:n])
		if !ok || m.Dev != c.index {
			continue
		}
		c.dispatch(m)
	}
}

func (c *Conn) dispatch(m Message) {
	if m.SWID == 0 && m.Feature != 0xFF && m.Feature != 0x8F {
		c.notify(m)
		return
	}
	c.mu.Lock()
	p := c.pend
	var res result
	match := false
	if p != nil {
		switch {
		case (m.Feature == 0xFF || m.Feature == 0x8F) && m.Params[0] == p.feat && m.Params[1] == p.fnSW:
			res, match = result{err: DeviceError{Code: m.Params[2]}}, true
		case m.Feature == p.feat && m.Function<<4|m.SWID == p.fnSW:
			res, match = result{msg: m}, true
		}
		if match {
			c.pend = nil
		}
	}
	c.mu.Unlock()
	if match {
		p.ch <- res
	}
}

// notify delivers m, dropping the oldest queued notification when full.
// Only the reader goroutine sends, so a drop always makes room.
func (c *Conn) notify(m Message) {
	for {
		select {
		case c.notifs <- m:
			return
		default:
		}
		select {
		case <-c.notifs:
		default:
		}
	}
}

// Request sends a function call and waits for its reply.
func (c *Conn) Request(feat, fn byte, params []byte, timeout time.Duration) (Message, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	m := Message{Dev: c.index, Feature: feat, Function: fn, SWID: requestSWID}
	copy(m.Params[:], params)
	p := &pending{feat: feat, fnSW: fn<<4 | requestSWID, ch: make(chan result, 1)}

	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return Message{}, err
	}
	c.pend = p
	c.mu.Unlock()

	if err := c.dev.Write(Encode(m), writeTimeout); err != nil {
		c.clearPending(p)
		return Message{}, err
	}

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-p.ch:
		return r.msg, r.err
	case <-t.C:
		c.clearPending(p)
		// A result may have been delivered just before clearing.
		select {
		case r := <-p.ch:
			return r.msg, r.err
		default:
		}
		return Message{}, ErrTimeout
	}
}

func (c *Conn) clearPending(p *pending) {
	c.mu.Lock()
	if c.pend == p {
		c.pend = nil
	}
	c.mu.Unlock()
}

// Send writes a function call without waiting for a reply. timeout is the
// write timeout. It is serialised with Request.
func (c *Conn) Send(feat, fn byte, params []byte, timeout time.Duration) error {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return err
	}
	m := Message{Dev: c.index, Feature: feat, Function: fn, SWID: requestSWID}
	copy(m.Params[:], params)
	return c.dev.Write(Encode(m), timeout)
}

// Close stops the reader and closes the device.
func (c *Conn) Close() error {
	var err error
	c.closeMu.Do(func() {
		c.fail(hid.ErrClosed)
		close(c.stop)
		<-c.done
		err = c.dev.Close()
	})
	return err
}
