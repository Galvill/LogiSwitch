package switcher

import (
	"strings"
	"testing"
	"time"

	"logiswitch/internal/config"
	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp/hidpptest"
)

// A second press of the target slot while the keyboard is still within its
// leaving grace (it stayed connected) resends the keyboard setHost instead
// of being swallowed as the "own slot".
func TestRepressDuringLeavingResends(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.stayKB()
	r.inject(0xD2)
	r.s.Settle()
	r.clk.Advance(time.Second)
	r.inject(0xD2)
	r.s.Settle()
	if n := r.o.callCount(kbPath, kbCH, 1); n != 2 {
		t.Errorf("keyboard setHost writes = %d, want 2 (re-press resent)", n)
	}
	if r.logHas("own slot pressed") || !r.logHas("target pressed again while leaving") {
		t.Errorf("re-press of the target treated as own slot:\n%s", r.log)
	}
}

// Release stops undiverting a keyboard at its first failed write, so a gone
// keyboard costs one timeout, not three.
func TestReleaseStopsAtFirstUndivertError(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.tick()
	before := h.o.callCount(kbPath, kbRP, 3)
	kb.Disconnect()
	h.s.Release()
	if got := h.o.callCount(kbPath, kbRP, 3) - before; got != 1 {
		t.Errorf("undivert writes after the keyboard went = %d, want 1", got)
	}
}

// An open, diverted keyboard is re-checked over BLE only every
// divertRecheck, not every poll.
func TestDivertRecheckThrottled(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()
	h.tick()
	before := h.o.callCount(kbPath, kbRP, 2)
	for i := 0; i < 9; i++ { // 4.5 s of 500 ms polls
		h.clk.Advance(500 * time.Millisecond)
		h.tick()
	}
	if got := h.o.callCount(kbPath, kbRP, 2); got != before {
		t.Errorf("getCidReporting calls %d -> %d within 5s, want none", before, got)
	}
	h.clk.Advance(500 * time.Millisecond)
	h.tick()
	if got := h.o.callCount(kbPath, kbRP, 2); got != before+1 {
		t.Errorf("getCidReporting calls %d -> %d at 5s, want one re-check", before, got)
	}
	kb.Reconnect() // diversion lost, handle still fine
	h.clk.Advance(5 * time.Second)
	h.tick()
	if !allDiverted(kb) {
		t.Error("lost diversion not restored by the periodic re-check")
	}
}

// A mouse setHost write that times out but was delivered anyway: the mouse
// leaving shortly afterwards counts as delivered.
func TestTimedOutMouseWriteCountsAsSent(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.mouse.SetWriteDelay(time.Second)
	r.mouse.SetWriteTimeouts(true)
	r.press(0xD2)
	r.tick() // the mouse write times out at 200 ms, yet takes effect
	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Fatalf("mouse setHost = %v, want [1]", got)
	}
	p := r.s.pend
	if p == nil || len(p.sent) != 1 {
		t.Fatalf("pending = %+v, want the timed-out write recorded as sent", p)
	}
	r.clk.Advance(300 * time.Millisecond)
	r.o.kill(mousePath)
	r.waitDropped(mousePath)
	if r.s.pend != nil {
		t.Errorf("pending not delivered after the mouse left: %+v", r.s.pend)
	}
	if !r.logHas("left after setHost") {
		t.Errorf("want delivery on leave:\n%s", r.log)
	}
}

// A timed-out write to a mouse that stays and still reports its old host is
// resent after the probe.
func TestTimedOutMouseWriteNotDeliveredIsResent(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.o.setWriteErr(mousePath, mouseCH, 1, hid.ErrWriteTimeout) // cancelled, not delivered
	r.press(0xD2)
	r.tick()
	r.o.setWriteErr(mousePath, mouseCH, 1, nil)
	r.clk.Advance(500 * time.Millisecond)
	r.tick()
	if n := r.o.callCount(mousePath, mouseCH, 1); n != 1 {
		t.Fatalf("mouse setHost writes = %d before the probe, want 1 (timed out = sent, unconfirmed)", n)
	}
	r.clk.Advance(600 * time.Millisecond)
	r.tick() // probe: current 0 != 1, resend
	if n := r.o.callCount(mousePath, mouseCH, 1); n != 2 {
		t.Errorf("mouse setHost writes = %d, want 2 (resent after probe)", n)
	}
}

func countLog(h *harness, sub string) int { return strings.Count(h.log.String(), sub) }

// "no keyboard/mouse found" is logged once when discovery settles and again
// only when presence changes.
func TestPresenceLogged(t *testing.T) {
	h := newHarness(t, config.Default())
	h.o.add(hidppInfo(mousePath, "MX_Master_3S"), hidpptest.NewMouse(0))
	defer h.s.Release()
	for i := 0; i < 4; i++ {
		h.tick()
		h.clk.Advance(500 * time.Millisecond)
	}
	if n := countLog(h, `msg="no keyboard found"`); n != 1 {
		t.Errorf(`"no keyboard found" logged %d times, want 1:%s`, n, h.log)
	}
	if n := countLog(h, `msg="no mouse found"`); n != 0 {
		t.Errorf(`"no mouse found" logged with a mouse present`)
	}

	h.o.add(hidppInfo(kbPath, "MX_Keys"), hidpptest.NewKeyboard(0))
	for i := 0; i < 3; i++ {
		h.tick()
		h.clk.Advance(500 * time.Millisecond)
	}
	if n := countLog(h, `msg="keyboard found"`); n != 1 {
		t.Errorf(`"keyboard found" logged %d times, want 1`, n)
	}

	h.o.setPresent(kbPath, false)
	h.o.setPresent(mousePath, false)
	for i := 0; i < 3; i++ {
		h.tick()
		h.clk.Advance(500 * time.Millisecond)
	}
	if n := countLog(h, `msg="no keyboard found"`); n != 2 {
		t.Errorf(`"no keyboard found" logged %d times, want 2`, n)
	}
	if n := countLog(h, `msg="no mouse found"`); n != 1 {
		t.Errorf(`"no mouse found" logged %d times, want 1`, n)
	}
}
