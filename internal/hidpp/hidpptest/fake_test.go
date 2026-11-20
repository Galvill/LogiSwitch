package hidpptest

import (
	"errors"
	"testing"
	"time"

	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp"
)

const to = time.Second

func newConn(t *testing.T, f *Fake) *hidpp.Conn {
	t.Helper()
	c := hidpp.NewConn(f, 0xFF)
	t.Cleanup(func() { c.Close() })
	return c
}

func feat(t *testing.T, c *hidpp.Conn, id uint16) byte {
	t.Helper()
	r, err := c.Request(0, 0, []byte{byte(id >> 8), byte(id)}, to)
	if err != nil {
		t.Fatal(err)
	}
	return r.Params[0]
}

func TestFakeFeatures(t *testing.T) {
	c := newConn(t, NewKeyboard(1))
	if got := feat(t, c, 0x1814); got != 9 {
		t.Errorf("CHANGE_HOST index = %d", got)
	}
	if got := feat(t, c, 0x1B04); got != 8 {
		t.Errorf("REPROG index = %d", got)
	}
	if got := feat(t, c, 0x9999); got != 0 {
		t.Errorf("unknown index = %d", got)
	}
	r, err := c.Request(9, 0, nil, to)
	if err != nil || r.Params[0] != 3 || r.Params[1] != 1 {
		t.Errorf("hostinfo = %v %v", r.Params[:2], err)
	}
	m := newConn(t, NewMouse(0))
	if feat(t, m, 0x1814) != 10 || feat(t, m, 0x1B04) != 9 {
		t.Error("mouse indexes")
	}
}

func TestFakeUnknownFunctionErrors(t *testing.T) {
	c := newConn(t, NewKeyboard(0))
	_, err := c.Request(9, 5, nil, to)
	var de hidpp.DeviceError
	if !errors.As(err, &de) || de.Code != 0x07 {
		t.Errorf("err = %v", err)
	}
	_, err = c.Request(0x42, 0, nil, to)
	if !errors.As(err, &de) || de.Code != 0x02 {
		t.Errorf("err = %v", err)
	}
}

func TestFakeReprogInfo(t *testing.T) {
	c := newConn(t, NewKeyboard(0))
	r, _ := c.Request(8, 0, nil, to)
	if r.Params[0] != 3 {
		t.Fatalf("count = %d", r.Params[0])
	}
	r, err := c.Request(8, 1, []byte{1}, to)
	if err != nil || r.Params[0] != 0 || r.Params[1] != 0xD2 || r.Params[4] != 0x64 {
		t.Errorf("cidinfo = %v %v", r.Params[:5], err)
	}
	if n, _ := newConn(t, NewMouse(0)).Request(9, 0, nil, to); n.Params[0] != 0 {
		t.Error("mouse has host CIDs")
	}
}

func TestFakeDivertAndPress(t *testing.T) {
	f := NewKeyboard(0)
	c := newConn(t, f)
	if _, err := c.Request(8, 3, []byte{0x00, 0xD2, 0x03}, to); err != nil {
		t.Fatal(err)
	}
	if !f.Diverted(0xD2) || f.Diverted(0xD1) {
		t.Fatal("diverted state wrong")
	}
	r, err := c.Request(8, 2, []byte{0x00, 0xD2}, to)
	if err != nil || r.Params[1] != 0xD2 || r.Params[2]&1 != 1 {
		t.Errorf("cidreporting = %v %v", r.Params[:3], err)
	}
	f.Press(0xD1) // not diverted: nothing
	f.Press(0xD2)
	for i, want := range []byte{0xD2, 0} {
		select {
		case m := <-c.Notifications():
			if m.Feature != 8 || m.Function != 0 || m.SWID != 0 || m.Params[0] != 0 || m.Params[1] != want {
				t.Errorf("notif %d = %+v", i, m)
			}
		case <-time.After(to):
			t.Fatalf("no notif %d", i)
		}
	}
	select {
	case m := <-c.Notifications():
		t.Errorf("extra notif %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestFakeReconnectClearsDivert(t *testing.T) {
	f := NewKeyboard(0)
	c := newConn(t, f)
	c.Request(8, 3, []byte{0x00, 0xD2, 0x03}, to)
	f.Disconnect()
	f.Reconnect()
	if f.Diverted(0xD2) {
		t.Error("divert survived reconnect")
	}
}

func TestFakeSetHost(t *testing.T) {
	f := NewKeyboard(0)
	c := newConn(t, f)
	if err := c.Send(9, 1, []byte{2}, to); err != nil {
		t.Fatal(err)
	}
	if f.Host() != 2 || len(f.SetHostCalls()) != 1 || f.SetHostCalls()[0] != 2 {
		t.Errorf("host=%d calls=%v", f.Host(), f.SetHostCalls())
	}
	if err := c.Send(9, 0, nil, to); err == nil {
		t.Error("write should fail while disconnected")
	}
	f.Reconnect()
	if r, err := c.Request(9, 0, nil, to); err != nil || r.Params[1] != 2 {
		t.Errorf("after reconnect: %v %v", r.Params[:2], err)
	}
}

func TestFakeDisconnectedAndDelayAndClose(t *testing.T) {
	f := NewMouse(0)
	f.SetWriteDelay(50 * time.Millisecond)
	f.Disconnect()
	start := time.Now()
	if err := f.Write(hidpp.Encode(hidpp.Message{Dev: 0xFF}), to); err == nil {
		t.Error("expected error")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("delay not applied")
	}
	start = time.Now()
	buf := make([]byte, 64)
	if n, err := f.Read(buf, 30*time.Millisecond); n != 0 || err != nil {
		t.Errorf("read = %d %v", n, err)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Error("read returned early")
	}
	f.Close()
	if _, err := f.Read(buf, time.Millisecond); err != hid.ErrClosed {
		t.Errorf("read after close = %v", err)
	}
	if err := f.Write(make([]byte, 20), time.Millisecond); err != hid.ErrClosed {
		t.Errorf("write after close = %v", err)
	}
}

func TestFakeWriteTimeoutStillDelivers(t *testing.T) {
	f := NewMouse(0)
	c := newConn(t, f)
	f.SetWriteDelay(time.Second)
	f.SetWriteTimeouts(true)
	start := time.Now()
	err := c.SetHost(10, 2, 50*time.Millisecond)
	if !errors.Is(err, hid.ErrWriteTimeout) {
		t.Fatalf("SetHost err = %v, want hid.ErrWriteTimeout", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("write returned after %v, want at the 50ms timeout", d)
	}
	if got := f.SetHostCalls(); len(got) != 1 || got[0] != 2 {
		t.Errorf("SetHostCalls = %v, want [2] (cancelled but delivered)", got)
	}
}
