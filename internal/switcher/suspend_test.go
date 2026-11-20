package switcher

import (
	"context"
	"strings"
	"testing"
	"time"

	"logiswitch/internal/config"
	"logiswitch/internal/hidpp/hidpptest"
)

// runRig starts Run on a keyboard and mouse with a fast poll and waits until
// both are ready. The returned stop cancels Run and waits for it.
func runRig(t *testing.T) (*harness, *hidpptest.Fake, *hidpptest.Fake, func()) {
	t.Helper()
	cfg := config.Default()
	cfg.PollInterval = 20 * time.Millisecond
	h := newHarness(t, cfg)
	kb, mouse := hidpptest.NewKeyboard(0), hidpptest.NewMouse(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.o.add(hidppInfo(mousePath, "MX_Master_3S"), mouse)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	waitFor(t, "ready", func() bool { return allDiverted(kb) && strings.Contains(h.log.String(), "mouse opened") })
	return h, kb, mouse, stop
}

func TestSuspendReleasesUntilResume(t *testing.T) {
	h, kb, _, stop := runRig(t)
	if !h.s.Suspend() {
		t.Fatal("Suspend = false, want true (Run is running)")
	}
	if !noneDiverted(kb) {
		t.Error("D1-D3 still diverted after Suspend")
	}
	if h.o.openHandles(kbPath) != 0 || h.o.openHandles(mousePath) != 0 {
		t.Error("handles left open after Suspend")
	}
	// Run keeps ticking but holds off: the PC is about to sleep.
	time.Sleep(150 * time.Millisecond)
	if !noneDiverted(kb) || h.o.openHandles(kbPath) != 0 {
		t.Fatal("re-diverted while suspended")
	}
	h.s.Resume()
	waitFor(t, "re-divert after resume", func() bool { return allDiverted(kb) })
	stop()
	if !noneDiverted(kb) {
		t.Error("not released at stop")
	}
}

func TestSuspendHoldExpires(t *testing.T) {
	h, kb, _, _ := runRig(t)
	h.s.Suspend()
	time.Sleep(100 * time.Millisecond)
	if !noneDiverted(kb) {
		t.Fatal("re-diverted during the hold")
	}
	// No resume notice (e.g. sleep aborted): upkeep resumes after the hold.
	h.clk.Advance(suspendHold)
	waitFor(t, "re-divert after hold", func() bool { return allDiverted(kb) })
}

func TestSuspendWithoutRunReturns(t *testing.T) {
	h := newHarness(t, config.Default())
	h.s.Release()
	start := time.Now()
	if h.s.Suspend() {
		t.Error("Suspend after Release = true")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Suspend after Release took %v", d)
	}
	h.s.Resume() // must not block
}

// On the owner goroutine: suspend clears the pending mouse switch, closes
// everything, and Tick re-diverts once resumed.
func TestSuspendClearsPendingAndTickRediverts(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, false) // mouse asleep
	r.stayKB()
	r.inject(0xD2)
	if r.s.pend == nil {
		t.Fatal("precondition: pending")
	}
	r.s.suspend()
	if r.s.pend != nil {
		t.Error("pending mouse switch kept across suspend")
	}
	if len(r.s.devs) != 0 || !noneDiverted(r.kb) || r.o.openHandles(kbPath) != 0 {
		t.Fatalf("after suspend: devs=%d diverted=%v handles=%d", len(r.s.devs), !noneDiverted(r.kb), r.o.openHandles(kbPath))
	}
	r.o.setPresent(mousePath, true)
	r.tick()
	if len(r.s.devs) != 0 {
		t.Fatal("Tick reopened devices while suspended")
	}
	r.s.resume()
	r.tick()
	if !allDiverted(r.kb) || r.s.devs[kbPath] == nil || r.s.devs[mousePath] == nil {
		t.Fatal("Tick did not rediscover and re-divert after resume")
	}
	if len(r.mouse.SetHostCalls()) != 0 {
		t.Errorf("mouse got setHost %v after suspend cleared the switch", r.mouse.SetHostCalls())
	}
}

// Status reports the diversion state found before LogiSwitch diverted, so a
// conflicting holder (Options+, a dead instance) is visible.
func TestStatusDivertedBefore(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.tick()
	st := h.s.Status()
	if len(st) != 1 || st[0].DivertedBefore {
		t.Fatalf("first instance Status = %+v, want DivertedBefore false", st)
	}
	// The first instance dies holding the diversion (its handle is torn
	// down so it no longer reads the shared fake); a second one starts.
	h.o.kill(kbPath)
	waitFor(t, "first reader gone", func() bool { return len(h.s.gone) == 1 })
	other := New(h.o, config.Default(), h.clk, h.s.log)
	other.Tick()
	other.Settle()
	st = other.Status()
	if len(st) != 1 || !st[0].DivertedBefore {
		t.Errorf("second instance Status = %+v, want DivertedBefore true", st)
	}
	other.Release()
	h.s.Release()
}
