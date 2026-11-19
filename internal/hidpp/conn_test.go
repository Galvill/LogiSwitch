package hidpp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"logiswitch/internal/hid"
)

// stubDevice is a tiny scripted hid.Device. Each Write may queue replies.
type stubDevice struct {
	mu      sync.Mutex
	inbox   [][]byte
	readErr error
	onWrite func(report []byte) [][]byte
	closed  bool
}

func (s *stubDevice) push(b ...[]byte) {
	s.mu.Lock()
	s.inbox = append(s.inbox, b...)
	s.mu.Unlock()
}

func (s *stubDevice) Read(buf []byte, timeout time.Duration) (int, error) {
	s.mu.Lock()
	if s.readErr != nil {
		err := s.readErr
		s.mu.Unlock()
		return 0, err
	}
	if len(s.inbox) > 0 {
		b := s.inbox[0]
		s.inbox = s.inbox[1:]
		s.mu.Unlock()
		return copy(buf, b), nil
	}
	s.mu.Unlock()
	time.Sleep(time.Millisecond)
	return 0, nil
}

func (s *stubDevice) Write(report []byte, _ time.Duration) error {
	if s.onWrite != nil {
		s.push(s.onWrite(report)...)
	}
	return nil
}

func (s *stubDevice) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

var _ hid.Device = (*stubDevice)(nil)

func reply(req []byte, params ...byte) []byte {
	m, _ := Decode(req)
	copy(m.Params[:], params)
	return Encode(m)
}

func TestRequestSkipsInterleavedNotification(t *testing.T) {
	d := &stubDevice{}
	notif := Message{Dev: 1, Feature: 0x1B, Function: 0, SWID: 0}
	notif.Params[1] = 0xD1
	d.onWrite = func(req []byte) [][]byte { return [][]byte{Encode(notif), reply(req, 0x42)} }
	c := NewConn(d, 1)
	defer c.Close()
	m, err := c.Request(0x05, 1, []byte{9}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m.Params[0] != 0x42 || m.Feature != 0x05 || m.Function != 1 {
		t.Fatalf("bad reply %+v", m)
	}
	select {
	case n := <-c.Notifications():
		if n != notif {
			t.Fatalf("notification %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}
}

func TestRequestDeviceError(t *testing.T) {
	d := &stubDevice{}
	d.onWrite = func(req []byte) [][]byte {
		m, _ := Decode(req)
		e := Message{Dev: m.Dev, Feature: 0xFF, Function: 0, SWID: 0}
		e.Params[0], e.Params[1], e.Params[2] = m.Feature, m.Function<<4|m.SWID, 0x05
		return [][]byte{Encode(e)}
	}
	c := NewConn(d, 1)
	defer c.Close()
	_, err := c.Request(0x05, 2, nil, time.Second)
	var de DeviceError
	if !errors.As(err, &de) || de.Code != 0x05 {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestDeviceErrorHidpp1(t *testing.T) {
	d := &stubDevice{}
	d.onWrite = func(req []byte) [][]byte {
		m, _ := Decode(req)
		e := Message{Dev: m.Dev, Feature: 0x8F}
		e.Params[0], e.Params[1], e.Params[2] = m.Feature, m.Function<<4|m.SWID, 0x02
		return [][]byte{Encode(e)}
	}
	c := NewConn(d, 1)
	defer c.Close()
	_, err := c.Request(0x05, 2, nil, time.Second)
	if de, ok := err.(DeviceError); !ok || de.Code != 0x02 {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestTimeout(t *testing.T) {
	c := NewConn(&stubDevice{}, 1)
	defer c.Close()
	if _, err := c.Request(0x05, 0, nil, 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadErrorFailsRequests(t *testing.T) {
	boom := errors.New("boom")
	d := &stubDevice{readErr: boom}
	c := NewConn(d, 1)
	defer c.Close()
	if _, err := c.Request(0x05, 0, nil, time.Second); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Request(0x05, 0, nil, time.Second); !errors.Is(err, boom) {
		t.Fatalf("later err = %v", err)
	}
	select {
	case _, ok := <-c.Notifications():
		if ok {
			t.Fatal("unexpected notification")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed")
	}
}

func TestSendAndClose(t *testing.T) {
	d := &stubDevice{}
	d.onWrite = func(req []byte) [][]byte { return [][]byte{reply(req)} }
	c := NewConn(d, 1)
	if err := c.Send(0x05, 0, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !d.closed {
		t.Fatal("device not closed")
	}
	if _, err := c.Request(0x05, 0, nil, time.Second); !errors.Is(err, hid.ErrClosed) {
		t.Fatalf("err = %v", err)
	}
}
