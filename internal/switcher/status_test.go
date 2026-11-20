package switcher

import (
	"testing"
	"time"

	"logiswitch/internal/config"
	"logiswitch/internal/hidpp/hidpptest"
)

func TestStatusReportsDiverted(t *testing.T) {
	r := newRig(t, config.Default(), 0, 2, true)
	st := r.s.Status()
	if len(st) != 2 {
		t.Fatalf("Status = %+v, want 2 entries", st)
	}
	k, m := st[0], st[1] // sorted by Kind: keyboard, mouse
	if k.Kind != "keyboard" || k.Path != kbPath || k.Product != "MX_Keys" ||
		k.ChangeHostIdx != kbCH || k.ReprogIdx != kbRP || k.CurrentHost != 1 || !k.Diverted {
		t.Errorf("keyboard status = %+v", k)
	}
	if m.Kind != "mouse" || m.Path != mousePath || m.ChangeHostIdx != mouseCH ||
		m.CurrentHost != 3 || m.Diverted {
		t.Errorf("mouse status = %+v", m)
	}
}

func TestSetAllSendsMouseThenKeyboard(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	r.mouse.SetWriteDelay(time.Second)
	start := time.Now()
	seen := make(chan time.Duration, 1)
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
	if err := r.s.SetAll(2); err != nil {
		t.Fatal(err)
	}
	if d := <-seen; d < 0 || d > 100*time.Millisecond {
		t.Errorf("keyboard setHost after %v, want not delayed by the mouse", d)
	}
	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Errorf("mouse setHost = %v, want [1]", got)
	}
	if got := r.kb.SetHostCalls(); !eq(got, 1) {
		t.Errorf("keyboard setHost = %v, want [1]", got)
	}
}

func TestSetAllRejectsBadSlot(t *testing.T) {
	r := newRig(t, config.Default(), 0, 0, true)
	for _, n := range []int{0, 4, -1} {
		err := r.s.SetAll(n)
		if err == nil || err.Error() != "slot must be 1-3" {
			t.Errorf("SetAll(%d) = %v, want 'slot must be 1-3'", n, err)
		}
	}
	if len(r.kb.SetHostCalls())+len(r.mouse.SetHostCalls()) != 0 {
		t.Error("bad slot sent setHost")
	}
}

func TestSetAllNoDevices(t *testing.T) {
	h := newHarness(t, config.Default())
	t.Cleanup(h.s.Release)
	err := h.s.SetAll(1)
	if err == nil || err.Error() != "no devices found" {
		t.Errorf("SetAll = %v, want 'no devices found'", err)
	}
}

func TestSetAllDiscoversWithoutTick(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	t.Cleanup(h.s.Release)
	if err := h.s.SetAll(3); err != nil {
		t.Fatal(err)
	}
	if got := kb.SetHostCalls(); !eq(got, 2) {
		t.Errorf("keyboard setHost = %v, want [2]", got)
	}
}

func TestSetAllSkipsDeviceAlreadyOnSlot(t *testing.T) {
	r := newRig(t, config.Default(), 1, 0, true) // keyboard already on 2
	if err := r.s.SetAll(2); err != nil {
		t.Fatal(err)
	}
	if got := r.kb.SetHostCalls(); len(got) != 0 {
		t.Errorf("keyboard already on slot got setHost %v", got)
	}
	if got := r.mouse.SetHostCalls(); !eq(got, 1) {
		t.Errorf("mouse setHost = %v, want [1]", got)
	}
	r2 := newRig(t, config.Default(), 0, 1, true) // mouse already on 2
	if err := r2.s.SetAll(2); err != nil {
		t.Fatal(err)
	}
	if got := r2.mouse.SetHostCalls(); len(got) != 0 {
		t.Errorf("mouse already on slot got setHost %v", got)
	}
	if got := r2.kb.SetHostCalls(); !eq(got, 1) {
		t.Errorf("keyboard setHost = %v, want [1]", got)
	}
}
