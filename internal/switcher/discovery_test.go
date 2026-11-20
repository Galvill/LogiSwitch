package switcher

import (
	"testing"
	"time"

	"logiswitch/internal/config"
	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp"
	"logiswitch/internal/hidpp/hidpptest"
)

// remapCH presents an inner fake's CHANGE_HOST feature at a different index,
// as if the device's firmware had changed its feature table.
type remapCH struct {
	inner    hid.Device
	from, to byte // inner index, outer index
}

func (r *remapCH) Read(buf []byte, timeout time.Duration) (int, error) {
	n, err := r.inner.Read(buf, timeout)
	if n == 0 || err != nil {
		return n, err
	}
	m, ok := hidpp.Decode(buf[:n])
	if !ok {
		return n, err
	}
	switch {
	case m.Feature == r.from:
		m.Feature = r.to
	case m.Feature == 0 && m.Params[0] == r.from, m.Feature == 0xFF && m.Params[0] == r.from:
		m.Params[0] = r.to
	}
	return copy(buf, hidpp.Encode(m)), nil
}

func (r *remapCH) Write(report []byte, timeout time.Duration) error {
	if m, ok := hidpp.Decode(report); ok && m.Feature == r.to {
		m.Feature = r.from
		report = hidpp.Encode(m)
	}
	return r.inner.Write(report, timeout)
}

func (r *remapCH) Close() error { return r.inner.Close() }

func TestPressNotBlockedByOpeningDevice(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()
	h.tick()

	// A new collection that never answers: its classification times out.
	const slow = `\\?\hid#slow`
	h.o.add(hidppInfo(slow, "Slow"), hidpptest.NewMouse(0))
	h.o.setMute(slow, true)

	start := time.Now()
	h.s.Tick() // starts the slow open in a worker
	kb.Press(0xD2)
	waitFor(t, "press events", func() bool { return len(h.s.events) >= 2 })
	h.s.Tick()
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("Ticks took %v; owner blocked on the opening device", d)
	}
	if got := kb.SetHostCalls(); len(got) != 1 || got[0] != 1 {
		t.Errorf("keyboard setHost = %v, want [1] while the slow open is in flight", got)
	}
	h.s.Settle()
	if _, ok := h.s.devs[slow]; ok {
		t.Error("timed-out device registered")
	}
	if n := h.o.openHandles(slow); n != 0 {
		t.Errorf("timed-out handle left open (%d)", n)
	}
	h.s.Tick() // throttled
	h.s.Settle()
	if n := h.o.openCount(slow); n != 1 {
		t.Errorf("slow path opens = %d, want 1 (throttled)", n)
	}
}

func TestOpenResultForVanishedPathClosed(t *testing.T) {
	h := newHarness(t, config.Default())
	mouse := hidpptest.NewMouse(0)
	h.o.add(hidppInfo(mousePath, "MX_Master_3S"), mouse)
	defer h.s.Release()

	mouse.SetWriteDelay(100 * time.Millisecond)
	h.s.Tick() // open in flight
	waitFor(t, "worker opened", func() bool { return h.o.openCount(mousePath) == 1 })
	h.o.setPresent(mousePath, false)
	h.s.Tick() // sees the path gone while the worker is still busy
	h.s.Settle()
	if _, ok := h.s.devs[mousePath]; ok {
		t.Fatal("device registered for a vanished path")
	}
	if n := h.o.openHandles(mousePath); n != 0 {
		t.Fatalf("worker's handle for vanished path left open (%d)", n)
	}

	mouse.SetWriteDelay(0)
	h.o.setPresent(mousePath, true)
	h.tick() // not a failure: no throttle
	if h.s.devs[mousePath] == nil {
		t.Error("returning path not reopened")
	}
}

func TestDivertFailureAfterOpenThrottled(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()

	h.o.setSwallow(kbPath, kbRP, 3, true) // setCidReporting never answered
	h.tick()
	if _, ok := h.s.devs[kbPath]; ok {
		t.Fatal("keyboard registered although divert failed")
	}
	if n := h.o.openHandles(kbPath); n != 0 {
		t.Fatalf("handle left open after divert failure (%d)", n)
	}
	h.o.setSwallow(kbPath, kbRP, 3, false)
	h.tick()
	if n := h.o.openCount(kbPath); n != 1 {
		t.Errorf("opens = %d, want 1 (divert failure throttled)", n)
	}
	h.clk.Advance(retryInterval)
	h.tick()
	if !allDiverted(kb) {
		t.Error("not diverted on retry")
	}
}

func TestReopenVerifiesChangeHostIdx(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()
	h.tick()

	h.o.setPresent(kbPath, false)
	h.tick()
	// Same REPROG index, CHANGE_HOST moved from 9 to 12.
	kb.Reconnect()
	h.o.with(kbPath, func(e *entry) { e.dev = &remapCH{inner: kb, from: kbCH, to: 12} })
	h.o.setPresent(kbPath, true)
	h.tick()

	d := h.s.devs[kbPath]
	if d == nil || d.chIdx != 12 {
		t.Fatalf("reopened keyboard = %+v, want chIdx 12", d)
	}
	if !h.logHas("re-classifying") {
		t.Errorf("stale CHANGE_HOST index not detected:\n%s", h.log)
	}
	if got := h.o.callCount(kbPath, kbRP, 0); got != 2 {
		t.Errorf("Controls scans = %d, want 2 (full re-classification)", got)
	}
}

func TestTickAfterReleaseNoop(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.tick()
	h.s.Release()
	h.tick()
	if n := h.o.openCount(kbPath); n != 1 {
		t.Errorf("Tick after Release reopened (opens = %d)", n)
	}
	if h.o.openHandles(kbPath) != 0 || !noneDiverted(kb) {
		t.Error("Tick after Release revived the keyboard")
	}
}

func TestReleaseWaitsForInflightOpen(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	kb.SetWriteDelay(20 * time.Millisecond)
	h.s.Tick() // open + divert in flight
	h.s.Release()
	if !noneDiverted(kb) {
		t.Error("keyboard diverted by an in-flight open left diverted after Release")
	}
	if n := h.o.openHandles(kbPath); n != 0 {
		t.Errorf("handle left open after Release (%d)", n)
	}
}
