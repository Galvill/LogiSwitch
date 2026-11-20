package switcher

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"logiswitch/internal/config"
	"logiswitch/internal/hidpp"
	"logiswitch/internal/hidpp/hidpptest"
)

// Fake indexes: keyboard CHANGE_HOST 9, REPROG 8; mouse CHANGE_HOST 10.
const (
	kbCH    = 9
	kbRP    = 8
	mouseCH = 10
)

// rig is a harness with a keyboard and (optionally enumerated) mouse.
type rig struct {
	*harness
	kb, mouse *hidpptest.Fake
}

func newRig(t *testing.T, cfg config.Config, kbHost, mouseHost int, mousePresent bool) *rig {
	t.Helper()
	r := &rig{harness: newHarness(t, cfg), kb: hidpptest.NewKeyboard(kbHost), mouse: hidpptest.NewMouse(mouseHost)}
	r.o.add(hidppInfo(kbPath, "MX_Keys"), r.kb)
	r.o.add(hidppInfo(mousePath, "MX_Master_3S"), r.mouse)
	r.o.setPresent(mousePath, mousePresent)
	t.Cleanup(r.s.Release)
	r.tick()
	if !allDiverted(r.kb) {
		t.Fatal("precondition: keyboard diverted")
	}
	return r
}

// press makes the fake keyboard emit press+release for cid and waits until
// both notifications are queued for the owner.
func (r *rig) press(cid uint16) {
	r.t.Helper()
	r.kb.Press(cid)
	waitFor(r.t, "press events", func() bool { return len(r.s.events) >= 2 })
}

// inject delivers a divertedButtonsEvent listing cids from the registered
// keyboard, as the owner goroutine would.
func (r *rig) inject(cids ...uint16) {
	r.t.Helper()
	kd := r.s.devs[kbPath]
	if kd == nil {
		r.t.Fatal("keyboard not registered")
	}
	m := hidpp.Message{Dev: 0xFF, Feature: kd.rpIdx}
	for i, c := range cids {
		m.Params[2*i], m.Params[2*i+1] = byte(c>>8), byte(c)
	}
	r.s.handleEvent(event{kd, m})
}

// waitDropped drains (as the owner) until the device on path is no longer
// registered. Other devices' stale closures may also be queued, so counting
// s.gone is not enough.
func (r *rig) waitDropped(path string) {
	r.t.Helper()
	waitFor(r.t, path+" dropped", func() bool {
		r.s.drain()
		return r.s.devs[path] == nil
	})
}

func eq(got []int, want ...int) bool { return slices.Equal(got, want) }

func TestPressSwitchesMouseThenKeyboard(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.press(0xD2)
	r.tick()

	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Errorf("mouse setHost = %v, want [1]", got)
	}
	if got := r.kb.SetHostCalls(); !eq(got, 1) {
		t.Errorf("keyboard setHost = %v, want [1]", got)
	}
	if !r.logHas("switch to slot 2") {
		t.Errorf("want 'switch to slot 2' in log:\n%s", r.log)
	}
	p := r.s.pend
	if p == nil || p.slot != 1 || len(p.sent) != 1 {
		t.Fatalf("pending = %+v, want slot 1 marked sent", p)
	}
	if !p.expires.Equal(r.clk.Now().Add(2 * time.Minute)) {
		t.Errorf("expires = %v, want now+2m", p.expires)
	}
}

func TestPressOwnSlotNoop(t *testing.T) {
	r := newRig(t, config.Default(), 1, 1, true)
	r.press(0xD2) // slot 2 = this computer
	r.tick()
	if len(r.kb.SetHostCalls()) != 0 || len(r.mouse.SetHostCalls()) != 0 {
		t.Errorf("own-slot press sent setHost: kb=%v mouse=%v", r.kb.SetHostCalls(), r.mouse.SetHostCalls())
	}
	if r.s.pend != nil {
		t.Errorf("own-slot press left a pending switch: %+v", r.s.pend)
	}
	if !r.logHas("own slot") {
		t.Errorf("want own-slot debug line:\n%s", r.log)
	}
}

func TestPressUsesMapping(t *testing.T) {
	cfg := config.Default()
	cfg.MouseSlotFor = [3]int{0, 2, 1}
	r := newRig(t, cfg, 0, 0, true)
	r.press(0xD2)
	r.tick()
	if got := r.kb.SetHostCalls(); !eq(got, 1) {
		t.Errorf("keyboard setHost = %v, want [1]", got)
	}
	if got := r.mouse.SetHostCalls(); !eq(got, 2) {
		t.Errorf("mouse setHost = %v, want [2] (mapped)", got)
	}
}

func TestPressMouseAlreadyOnTarget(t *testing.T) {
	cfg := config.Default()
	cfg.MouseSlotFor = [3]int{0, 0, 2} // keyboard slot 2 keeps the mouse here
	r := newRig(t, cfg, 0, 0, true)
	r.press(0xD2)
	r.tick()
	if got := r.kb.SetHostCalls(); !eq(got, 1) {
		t.Errorf("keyboard setHost = %v, want [1]", got)
	}
	if got := r.mouse.SetHostCalls(); len(got) != 0 {
		t.Errorf("mouse already on target got setHost %v (would reconnect it)", got)
	}
	if r.s.pend != nil {
		t.Errorf("pending not cleared: %+v", r.s.pend)
	}
}

func TestPressMultiCID(t *testing.T) {
	t.Run("first host CID wins", func(t *testing.T) {
		r := newRig(t, config.Default(), 0, 0, true)
		r.inject(0xD2, 0xD1)
		r.s.Settle()
		if got := r.kb.SetHostCalls(); !eq(got, 1) {
			t.Errorf("keyboard setHost = %v, want [1]", got)
		}
		if got := r.mouse.SetHostCalls(); !eq(got, 1) {
			t.Errorf("mouse setHost = %v, want [1]", got)
		}
	})
	t.Run("non-host CID skipped", func(t *testing.T) {
		r := newRig(t, config.Default(), 0, 0, true)
		r.inject(0x50, 0xD3)
		r.s.Settle()
		if got := r.kb.SetHostCalls(); !eq(got, 2) {
			t.Errorf("keyboard setHost = %v, want [2]", got)
		}
	})
	t.Run("release ignored", func(t *testing.T) {
		r := newRig(t, config.Default(), 0, 0, true)
		r.inject()
		r.inject(0x50)
		r.s.Settle()
		if len(r.kb.SetHostCalls())+len(r.mouse.SetHostCalls()) != 0 || r.s.pend != nil {
			t.Errorf("release/non-host event switched: kb=%v mouse=%v", r.kb.SetHostCalls(), r.mouse.SetHostCalls())
		}
	})
}

func TestSlowMouseDoesNotDelayKeyboard(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.mouse.SetWriteDelay(time.Second)
	r.press(0xD2)

	// Watch for the keyboard write while Tick runs: Tick goes on to drop the
	// departing keyboard afterwards, which is not part of the latency.
	seen := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		for time.Since(start) < 3*time.Second {
			if len(r.kb.SetHostCalls()) == 1 {
				seen <- time.Since(start)
				return
			}
			time.Sleep(time.Millisecond)
		}
		seen <- -1
	}()
	r.s.Tick()
	tickTook := time.Since(start)
	if d := <-seen; d < 0 || d > 50*time.Millisecond {
		t.Errorf("keyboard setHost after %v, want within 50ms", d)
	}
	if tickTook > 200*time.Millisecond {
		t.Errorf("Tick blocked %v on the slow mouse", tickTook)
	}
	r.s.Settle()
	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Errorf("mouse setHost = %v, want [1]", got)
	}
}

func TestNoMouseKeyboardStillSwitches(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, false)
	r.press(0xD3)
	r.tick()
	if got := r.kb.SetHostCalls(); !eq(got, 2) {
		t.Errorf("keyboard setHost = %v, want [2]", got)
	}
	if !r.logHas("switch to slot 3") {
		t.Errorf("want 'switch to slot 3':\n%s", r.log)
	}
	if p := r.s.pend; p == nil || p.slot != 2 {
		t.Errorf("pending = %+v, want slot 2", p)
	}
}

func TestPendingDeliveredOnMouseReconnect(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, false) // mouse asleep
	r.press(0xD3)
	r.tick()
	if got := r.kb.SetHostCalls(); !eq(got, 2) {
		t.Fatalf("keyboard setHost = %v, want [2]", got)
	}

	// The keyboard leaves; the pending switch survives it.
	r.o.setPresent(kbPath, false)
	r.clk.Advance(5 * time.Second)
	r.tick()
	if _, ok := r.s.devs[kbPath]; ok {
		t.Fatal("departed keyboard still registered")
	}
	if r.s.pend == nil {
		t.Fatal("pending switch lost when the keyboard left")
	}

	// The mouse wakes up on this computer and is sent on at once.
	r.o.setPresent(mousePath, true)
	r.clk.Advance(30 * time.Second)
	r.tick()
	if got := r.mouse.SetHostCalls(); !eq(got, 2) {
		t.Fatalf("woken mouse setHost = %v, want [2]", got)
	}

	// Its collection closes (finding 5): delivered.
	r.clk.Advance(300 * time.Millisecond)
	r.o.kill(mousePath)
	r.waitDropped(mousePath)
	r.tick()
	if r.s.pend != nil {
		t.Fatalf("pending not cleared on delivery: %+v", r.s.pend)
	}
	if !r.logHas("mouse switched") {
		t.Errorf("want 'mouse switched' in log:\n%s", r.log)
	}

	// Later reconnects of the mouse do not resend.
	r.mouse.Reconnect()
	r.clk.Advance(5 * time.Second)
	r.tick()
	r.clk.Advance(5 * time.Second)
	r.tick()
	if got := r.mouse.SetHostCalls(); !eq(got, 2) {
		t.Errorf("mouse setHost = %v after delivery, want [2]", got)
	}
}

func TestPendingExpires(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, false)
	r.press(0xD2)
	r.tick()
	r.clk.Advance(2*time.Minute + time.Second)
	r.o.setPresent(mousePath, true)
	r.tick()
	r.tick()
	if got := r.mouse.SetHostCalls(); len(got) != 0 {
		t.Errorf("expired pending still sent mouse setHost %v", got)
	}
	if r.s.pend != nil {
		t.Errorf("pending not cleared on expiry")
	}
	if !r.logHas("expired") {
		t.Errorf("want expiry logged:\n%s", r.log)
	}
}

func TestPendingSupersededByNewPress(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, false)
	r.press(0xD2)
	r.tick()
	if p := r.s.pend; p == nil || p.slot != 1 {
		t.Fatalf("pending = %+v, want slot 1", p)
	}

	// The keyboard leaves and comes back (user switched back to this
	// computer; the pending switch is cancelled) and the user presses slot 3
	// before the mouse has woken.
	r.o.setPresent(kbPath, false)
	r.tick()
	r.kb.Reconnect()
	r.o.setPresent(kbPath, true)
	r.clk.Advance(3 * time.Second)
	r.tick()
	if d := r.s.devs[kbPath]; d == nil || d.host != 1 {
		t.Fatalf("keyboard after reconnect = %+v, want host 1", d)
	}
	r.press(0xD3)
	r.tick()
	if p := r.s.pend; p == nil || p.slot != 2 {
		t.Fatalf("pending = %+v, want slot 2", p)
	}

	r.o.setPresent(mousePath, true)
	r.tick()
	if got := r.mouse.SetHostCalls(); !eq(got, 2) {
		t.Errorf("mouse setHost = %v, want [2] only", got)
	}
}

func TestPendingResentWhenMouseStays(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.o.setSwallow(mousePath, mouseCH, 1, true) // setHost accepted, ignored
	hostInfos := func() int { return r.o.callCount(mousePath, mouseCH, 0) }

	r.press(0xD2)
	r.tick()
	if n := r.o.callCount(mousePath, mouseCH, 1); n != 1 {
		t.Fatalf("mouse setHost writes = %d, want 1", n)
	}
	probes := hostInfos()

	r.clk.Advance(500 * time.Millisecond)
	r.tick()
	if hostInfos() != probes || r.o.callCount(mousePath, mouseCH, 1) != 1 {
		t.Fatal("probed or resent before 1s")
	}

	r.clk.Advance(600 * time.Millisecond)
	r.tick() // still here, answers current=0 != 1: resend
	if hostInfos() != probes+1 {
		t.Errorf("HostInfo probes = %d, want %d", hostInfos(), probes+1)
	}
	if n := r.o.callCount(mousePath, mouseCH, 1); n != 2 {
		t.Fatalf("mouse setHost writes = %d, want 2 (resent)", n)
	}

	r.o.setSwallow(mousePath, mouseCH, 1, false)
	r.clk.Advance(1100 * time.Millisecond)
	r.tick() // resend goes through this time
	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Fatalf("mouse setHost = %v, want [1]", got)
	}
	r.mouse.Reconnect() // still enumerated and answering, now on slot 1
	r.clk.Advance(1100 * time.Millisecond)
	r.tick()
	if r.s.pend != nil {
		t.Errorf("pending not cleared when mouse reports target host")
	}
}

func TestPendingClearedWhenMouseStopsAnswering(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.press(0xD2)
	r.tick()
	if r.s.pend == nil {
		t.Fatal("precondition: pending")
	}
	r.o.setMute(mousePath, true) // stays enumerated, answers nothing
	r.clk.Advance(1100 * time.Millisecond)
	start := time.Now()
	r.tick()
	if d := time.Since(start); d > 600*time.Millisecond {
		t.Errorf("probe took %v, want <=200ms timeout", d)
	}
	if r.s.pend != nil {
		t.Errorf("pending not cleared after mouse stopped answering")
	}
	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Errorf("mouse setHost = %v, want [1]", got)
	}
}

func TestPendingRetriesFailedWrite(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.mouse.Disconnect() // enumerated, writes fail
	r.press(0xD2)
	r.tick() // a fast failure may already be retried by this Tick's upkeep
	attempts := r.o.callCount(mousePath, mouseCH, 1)
	if attempts < 1 || len(r.mouse.SetHostCalls()) != 0 {
		t.Fatalf("precondition: failed mouse write (attempts %d)", attempts)
	}
	r.clk.Advance(500 * time.Millisecond)
	r.tick()
	if n := r.o.callCount(mousePath, mouseCH, 1); n <= attempts {
		t.Errorf("mouse setHost attempts %d -> %d, want a retry on Tick", attempts, n)
	}
	r.mouse.Reconnect()
	r.clk.Advance(500 * time.Millisecond)
	r.tick()
	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Errorf("mouse setHost = %v, want [1]", got)
	}
}

func TestPendingDeliveredWhenMouseDropsDuringWrite(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.mouse.SetWriteDelay(time.Second)
	r.press(0xD2)
	r.s.Tick()
	waitFor(t, "mouse write started", func() bool { return r.o.callCount(mousePath, mouseCH, 1) == 1 })
	r.o.kill(mousePath)
	r.waitDropped(mousePath) // while its write is still in flight
	r.s.Settle()
	if r.s.pend != nil {
		t.Errorf("pending not delivered: write succeeded and the mouse left")
	}
	if !r.logHas("left during setHost") {
		t.Errorf("want delivery via the in-flight write:\n%s", r.log)
	}
}

func TestRunHandlesPress(t *testing.T) {
	cfg := config.Default()
	cfg.PollInterval = 20 * time.Millisecond
	h := newHarness(t, cfg)
	kb, mouse := hidpptest.NewKeyboard(0), hidpptest.NewMouse(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.o.add(hidppInfo(mousePath, "MX_Master_3S"), mouse)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, "ready", func() bool { return allDiverted(kb) && strings.Contains(h.log.String(), "mouse opened") })
	kb.Press(0xD3)
	waitFor(t, "switch", func() bool { return len(kb.SetHostCalls()) == 1 && len(mouse.SetHostCalls()) == 1 })
	if !eq(kb.SetHostCalls(), 2) || !eq(mouse.SetHostCalls(), 2) {
		t.Errorf("kb=%v mouse=%v, want [2] each", kb.SetHostCalls(), mouse.SetHostCalls())
	}
}

func TestPendingClearedWhenKeyboardReturns(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, false) // mouse asleep
	r.press(0xD2)
	r.tick()
	if r.s.pend == nil {
		t.Fatal("no pending switch after press")
	}
	// The keyboard leaves, then comes back to this PC.
	r.o.setPresent(kbPath, false)
	r.clk.Advance(5 * time.Second)
	r.tick()
	r.kb.Reconnect()
	r.o.setPresent(kbPath, true)
	r.clk.Advance(5 * time.Second)
	r.tick()
	if r.s.devs[kbPath] == nil {
		t.Fatal("keyboard not reopened")
	}
	if r.s.pend != nil {
		t.Fatalf("pending not cleared: %+v", r.s.pend)
	}
	if !r.logHas("pending mouse switch cancelled: keyboard returned") {
		t.Errorf("want cancel log:\n%s", r.log)
	}
	r.o.setPresent(mousePath, true)
	r.clk.Advance(5 * time.Second)
	r.tick()
	if got := r.mouse.SetHostCalls(); len(got) != 0 {
		t.Errorf("mouse got setHost %v after the keyboard returned", got)
	}
}

func TestNoDivertCheckAfterKeyboardLeaves(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	kd := r.s.devs[kbPath]
	r.inject(0xD2) // switches the keyboard; it is still registered until its conn closes
	if !kd.leaving {
		t.Fatal("keyboard not marked leaving after its host changed")
	}
	before := r.o.callCount(kbPath, kbRP, 2)
	r.s.Tick()
	r.s.Settle()
	if after := r.o.callCount(kbPath, kbRP, 2); after != before {
		t.Errorf("getCidReporting calls %d -> %d, want none after the keyboard left", before, after)
	}
}
