package switcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"logiswitch/internal/config"
	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp"
	"logiswitch/internal/hidpp/hidpptest"
)

const (
	kbPath    = `\\?\hid#kb`
	mousePath = `\\?\hid#mouse`
)

// ---- fake clock ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- fake opener ----

// entry is one enumerable HID collection backed by a device that survives
// handles being opened and closed (as a real device survives its handles).
type entry struct {
	info    hid.Info
	dev     hid.Device
	present bool
	openErr error
	mute    bool              // swallow writes so requests time out
	swallow map[[2]byte]bool  // (feature, function) writes accepted but not delivered
	failW   map[[2]byte]error // (feature, function) writes failed with this error, not delivered
	calls   map[[2]byte]int   // (feature, function) -> writes seen
	handles []*handle
}

type fakeOpener struct {
	mu      sync.Mutex
	entries map[string]*entry
	order   []string
	opens   map[string]int
}

func newOpener() *fakeOpener {
	return &fakeOpener{entries: map[string]*entry{}, opens: map[string]int{}}
}

func hidppInfo(path, product string) hid.Info {
	return hid.Info{Path: path, Product: product, VendorID: 0x046D, UsagePage: 0xFF43, Usage: 0x0202, InputLen: 20, OutputLen: 20}
}

func (o *fakeOpener) add(info hid.Info, dev hid.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.entries[info.Path] = &entry{info: info, dev: dev, present: true, calls: map[[2]byte]int{}, swallow: map[[2]byte]bool{}, failW: map[[2]byte]error{}}
	o.order = append(o.order, info.Path)
}

func (o *fakeOpener) with(path string, f func(e *entry)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f(o.entries[path])
}

func (o *fakeOpener) setPresent(path string, p bool) { o.with(path, func(e *entry) { e.present = p }) }
func (o *fakeOpener) setMute(path string, m bool)    { o.with(path, func(e *entry) { e.mute = m }) }
func (o *fakeOpener) setSwallow(path string, feat, fn byte, on bool) {
	o.with(path, func(e *entry) { e.swallow[[2]byte{feat, fn}] = on })
}
func (o *fakeOpener) setWriteErr(path string, feat, fn byte, err error) {
	o.with(path, func(e *entry) { e.failW[[2]byte{feat, fn}] = err })
}
func (o *fakeOpener) setOpenErr(path string, err error) {
	o.with(path, func(e *entry) { e.openErr = err })
}

// kill makes every open handle on path fail its next read, as when Windows
// tears down a BLE collection.
func (o *fakeOpener) kill(path string) {
	o.with(path, func(e *entry) {
		for _, h := range e.handles {
			h.mu.Lock()
			h.broken = true
			h.mu.Unlock()
		}
	})
}

func (o *fakeOpener) openCount(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opens[path]
}

func (o *fakeOpener) callCount(path string, feat, fn byte) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.entries[path].calls[[2]byte{feat, fn}]
}

// openHandles returns how many handles on path are not closed.
func (o *fakeOpener) openHandles(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, h := range o.entries[path].handles {
		h.mu.Lock()
		if !h.closed {
			n++
		}
		h.mu.Unlock()
	}
	return n
}

func (o *fakeOpener) Enumerate(vid uint16) ([]hid.Info, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []hid.Info
	for _, p := range o.order {
		if e := o.entries[p]; e.present && e.info.VendorID == vid {
			out = append(out, e.info)
		}
	}
	return out, nil
}

func (o *fakeOpener) Open(path string) (hid.Device, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := o.entries[path]
	o.opens[path]++
	if e == nil || !e.present {
		return nil, errors.New("open: file not found")
	}
	if e.openErr != nil {
		return nil, e.openErr
	}
	h := &handle{o: o, e: e}
	e.handles = append(e.handles, h)
	return h, nil
}

type handle struct {
	o      *fakeOpener
	e      *entry
	mu     sync.Mutex
	closed bool
	broken bool
}

func (h *handle) state() (closed, broken bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed, h.broken
}

func (h *handle) Read(buf []byte, timeout time.Duration) (int, error) {
	if closed, broken := h.state(); closed {
		return 0, hid.ErrClosed
	} else if broken {
		return 0, errors.New("read: device not connected")
	}
	n, err := h.e.dev.Read(buf, timeout)
	if closed, broken := h.state(); closed || broken {
		return 0, hid.ErrClosed
	}
	return n, err
}

func (h *handle) Write(report []byte, timeout time.Duration) error {
	if closed, _ := h.state(); closed {
		return hid.ErrClosed
	}
	h.o.mu.Lock()
	mute := h.e.mute
	var failErr error
	if m, ok := hidpp.Decode(report); ok {
		h.e.calls[[2]byte{m.Feature, m.Function}]++
		mute = mute || h.e.swallow[[2]byte{m.Feature, m.Function}]
		failErr = h.e.failW[[2]byte{m.Feature, m.Function}]
	}
	h.o.mu.Unlock()
	if failErr != nil {
		return failErr
	}
	if mute {
		return nil
	}
	return h.e.dev.Write(report, timeout)
}

func (h *handle) Close() error {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	return nil
}

// bareDevice answers root getFeature with "not supported" for everything:
// a HID++ device that has neither CHANGE_HOST nor REPROG_CONTROLS_V4.
type bareDevice struct{ out chan []byte }

func newBare() *bareDevice { return &bareDevice{out: make(chan []byte, 16)} }

func (b *bareDevice) Read(buf []byte, timeout time.Duration) (int, error) {
	select {
	case r := <-b.out:
		return copy(buf, r), nil
	case <-time.After(timeout):
		return 0, nil
	}
}

func (b *bareDevice) Write(report []byte, _ time.Duration) error {
	m, ok := hidpp.Decode(report)
	if !ok {
		return errors.New("bad report")
	}
	r := hidpp.Message{Dev: m.Dev, Feature: m.Feature, Function: m.Function, SWID: m.SWID}
	if m.Feature != 0 {
		r = hidpp.Message{Dev: m.Dev, Feature: 0xFF, SWID: m.SWID}
		r.Params[0], r.Params[1], r.Params[2] = m.Feature, m.Function<<4|m.SWID, 0x02
	}
	b.out <- hidpp.Encode(r)
	return nil
}

func (b *bareDevice) Close() error { return nil }

// ---- harness ----

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t   *testing.T
	o   *fakeOpener
	clk *fakeClock
	log *syncBuf
	s   *Switcher
}

func newHarness(t *testing.T, cfg config.Config) *harness {
	t.Helper()
	h := &harness{t: t, o: newOpener(), clk: newClock(), log: &syncBuf{}}
	lg := slog.New(slog.NewTextHandler(io.MultiWriter(h.log, t.Output()), &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.s = New(h.o, cfg, h.clk, lg)
	return h
}

// tick runs one Tick and waits for the workers it started (opens, mouse
// writes, probes) to post their results, committing them.
func (h *harness) tick() {
	h.s.Tick()
	h.s.Settle()
}

func (h *harness) logHas(sub string) bool { return strings.Contains(h.log.String(), sub) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func allDiverted(f *hidpptest.Fake) bool {
	for _, c := range hidpp.HostSwitchCIDs {
		if !f.Diverted(c) {
			return false
		}
	}
	return true
}

func noneDiverted(f *hidpptest.Fake) bool {
	for _, c := range hidpp.HostSwitchCIDs {
		if f.Diverted(c) {
			return false
		}
	}
	return true
}

// ---- tests ----

func TestTickDivertsKeyboard(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(1)
	mouse := hidpptest.NewMouse(1)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.o.add(hidppInfo(mousePath, "MX_Master_3S"), mouse)
	defer h.s.Release()

	h.tick()

	if !allDiverted(kb) {
		t.Fatal("keyboard D1-D3 not diverted after Tick")
	}
	if !h.logHas("keys taken over") || !h.logHas("slot=2") {
		t.Errorf("want 'keys taken over' with 1-based slot=2 in log:\n%s", h.log)
	}
	kd := h.s.devs[kbPath]
	if kd == nil || kd.role != roleKeyboard || kd.chIdx != 9 || kd.rpIdx != 8 || kd.host != 1 {
		t.Fatalf("keyboard device = %+v", kd)
	}
	md := h.s.devs[mousePath]
	if md == nil || md.role != roleMouse || md.chIdx != 10 {
		t.Fatalf("mouse device = %+v", md)
	}
	if len(mouse.SetHostCalls()) != 0 || len(kb.SetHostCalls()) != 0 {
		t.Error("Tick must not switch hosts")
	}

	// Steady state: further Ticks re-check D1 only, no re-divert, no reopen.
	before := h.o.callCount(kbPath, 8, 3)
	h.tick()
	h.tick()
	if got := h.o.callCount(kbPath, 8, 3); got != before {
		t.Errorf("setCidReporting calls %d -> %d on an already-diverted keyboard", before, got)
	}
	if h.o.openCount(kbPath) != 1 || h.o.openCount(mousePath) != 1 {
		t.Errorf("opens kb=%d mouse=%d, want 1 each", h.o.openCount(kbPath), h.o.openCount(mousePath))
	}
	if strings.Count(h.log.String(), "keys taken over") != 1 {
		t.Errorf("'keys taken over' logged more than once:\n%s", h.log)
	}
}

func TestTickRedivertsAfterReconnect(t *testing.T) {
	t.Run("stays enumerated", func(t *testing.T) {
		h := newHarness(t, config.Default())
		kb := hidpptest.NewKeyboard(0)
		h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
		defer h.s.Release()

		h.tick()
		kb.Disconnect()
		kb.Reconnect() // clears diversion, same handle still usable
		if allDiverted(kb) {
			t.Fatal("precondition: Reconnect should clear diversion")
		}
		h.clk.Advance(divertRecheck) // the handle survived: periodic re-check
		h.tick()
		if !allDiverted(kb) {
			t.Fatal("not re-diverted after reconnect")
		}
		if strings.Count(h.log.String(), "keys taken over") != 2 {
			t.Errorf("want two 'keys taken over' lines:\n%s", h.log)
		}
	})

	t.Run("collection vanishes and returns", func(t *testing.T) {
		h := newHarness(t, config.Default())
		kb := hidpptest.NewKeyboard(0)
		h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
		defer h.s.Release()

		h.tick()
		controlsScans := h.o.callCount(kbPath, 8, 0)
		if controlsScans != 1 {
			t.Fatalf("first open: getCount calls = %d, want 1", controlsScans)
		}

		kb.Disconnect()
		h.o.setPresent(kbPath, false)
		h.tick()
		if n := h.o.openHandles(kbPath); n != 0 {
			t.Fatalf("handle not closed when path vanished (%d open)", n)
		}
		if _, ok := h.s.devs[kbPath]; ok {
			t.Fatal("vanished keyboard still registered")
		}

		kb.Reconnect()
		h.o.setPresent(kbPath, true)
		h.tick()
		if !allDiverted(kb) {
			t.Fatal("not re-diverted after collection returned")
		}
		if h.o.openCount(kbPath) != 2 {
			t.Errorf("opens = %d, want 2", h.o.openCount(kbPath))
		}
		if got := h.o.callCount(kbPath, 8, 0); got != controlsScans {
			t.Errorf("known path re-scanned Controls (getCount calls %d -> %d)", controlsScans, got)
		}
	})

	t.Run("I/O error drops and rediscovers", func(t *testing.T) {
		h := newHarness(t, config.Default())
		kb := hidpptest.NewKeyboard(0)
		h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
		defer h.s.Release()

		h.tick()
		kb.Disconnect() // writes now fail, path stays enumerated
		h.clk.Advance(divertRecheck)
		h.tick()
		if _, ok := h.s.devs[kbPath]; ok {
			t.Fatal("keyboard with failing I/O still registered")
		}
		if n := h.o.openHandles(kbPath); n != 0 {
			t.Fatalf("failed handle not closed (%d open)", n)
		}
		kb.Reconnect()
		h.clk.Advance(3 * time.Second)
		h.tick()
		if !allDiverted(kb) {
			t.Fatal("not re-diverted after I/O error")
		}
	})
}

func TestTickIgnoresNonTarget(t *testing.T) {
	h := newHarness(t, config.Default())
	// Wrong usage: never opened.
	other := hidpptest.NewKeyboard(0)
	h.o.add(hid.Info{Path: `\\?\hid#col01`, VendorID: 0x046D, UsagePage: 0x0001, Usage: 0x0006}, other)
	// Right usage, but no CHANGE_HOST: opened once, then ignored for good.
	h.o.add(hidppInfo(`\\?\hid#bare`, "Other"), newBare())
	// Wired long-report usage is a valid candidate.
	kb := hidpptest.NewKeyboard(0)
	wired := hid.Info{Path: kbPath, Product: "MX_Keys", VendorID: 0x046D, UsagePage: 0xFF00, Usage: 0x0002}
	h.o.add(wired, kb)
	defer h.s.Release()

	for i := 0; i < 3; i++ {
		h.tick()
		h.clk.Advance(10 * time.Second)
	}
	if n := h.o.openCount(`\\?\hid#col01`); n != 0 {
		t.Errorf("non-HID++ collection opened %d times", n)
	}
	if n := h.o.openCount(`\\?\hid#bare`); n != 1 {
		t.Errorf("featureless device opened %d times, want 1", n)
	}
	if n := h.o.openHandles(`\\?\hid#bare`); n != 0 {
		t.Errorf("featureless device left open")
	}
	if _, ok := h.s.devs[`\\?\hid#bare`]; ok {
		t.Error("featureless device registered")
	}
	if !allDiverted(kb) {
		t.Error("0xFF00/0x0002 keyboard not diverted")
	}
	if noneDiverted(other) == false {
		t.Error("non-target collection touched")
	}
}

func TestTickRetriesTimedOutDevice(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()

	h.o.setMute(kbPath, true)
	h.tick() // FeatureIndex times out
	if _, ok := h.s.devs[kbPath]; ok {
		t.Fatal("timed-out device registered")
	}
	if n := h.o.openHandles(kbPath); n != 0 {
		t.Fatalf("timed-out handle not closed")
	}
	h.o.setMute(kbPath, false)

	h.tick() // throttled: too soon after the failure
	if n := h.o.openCount(kbPath); n != 1 {
		t.Errorf("retried immediately: opens = %d, want 1", n)
	}

	h.clk.Advance(2 * time.Second)
	h.tick()
	if !allDiverted(kb) {
		t.Fatal("not diverted on retry after timeout")
	}
	if n := h.o.openCount(kbPath); n != 2 {
		t.Errorf("opens = %d, want 2", n)
	}
}

func TestTickRetriesFailedOpen(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()

	h.o.setOpenErr(kbPath, errors.New("open: file not found"))
	h.tick()
	h.clk.Advance(500 * time.Millisecond)
	h.tick()
	if n := h.o.openCount(kbPath); n != 1 {
		t.Errorf("opens = %d, want 1 (throttled)", n)
	}
	h.o.setOpenErr(kbPath, nil)
	h.clk.Advance(2 * time.Second)
	h.tick()
	if !allDiverted(kb) {
		t.Fatal("not diverted after open recovered")
	}
}

func TestTickDropsClosedConn(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()

	h.tick()
	old := h.s.devs[kbPath]
	h.o.kill(kbPath)
	kb.Reconnect() // device comes back with diversion cleared
	waitFor(t, "conn-closed event", func() bool { return len(h.s.gone) == 1 })

	h.tick() // drains the closure, then reopens the (still enumerated) path
	d := h.s.devs[kbPath]
	if d == nil || d == old {
		t.Fatalf("closed conn not replaced: %+v", d)
	}
	if !allDiverted(kb) {
		t.Fatal("not re-diverted after reopen")
	}
	if got := h.o.callCount(kbPath, 8, 0); got != 1 {
		t.Errorf("Controls rescanned on known path (getCount calls = %d)", got)
	}
}

func TestTickRoutesKeyboardEvents(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()

	h.tick()
	kb.Press(0xD2)
	waitFor(t, "press events", func() bool { return len(h.s.events) == 2 })
	h.tick()
	if len(h.s.events) != 0 {
		t.Error("Tick did not drain events")
	}
	if got := kb.SetHostCalls(); len(got) != 1 || got[0] != 1 {
		t.Errorf("keyboard setHost calls = %v, want [1] (press routed to the switch handler)", got)
	}
}

func TestReleaseUndiverts(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	mouse := hidpptest.NewMouse(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.o.add(hidppInfo(mousePath, "MX_Master_3S"), mouse)

	h.tick()
	if !allDiverted(kb) {
		t.Fatal("precondition: diverted")
	}
	h.s.Release()
	if !noneDiverted(kb) {
		t.Error("D1-D3 still diverted after Release")
	}
	if h.o.openHandles(kbPath) != 0 || h.o.openHandles(mousePath) != 0 {
		t.Error("handles left open after Release")
	}
	h.s.Release() // idempotent
}

func TestReleaseToleratesGoneKeyboard(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	h.tick()
	kb.Disconnect()
	h.s.Release() // undivert fails; must still close
	if h.o.openHandles(kbPath) != 0 {
		t.Error("handle left open")
	}
}

func TestRunTicksAndReleases(t *testing.T) {
	cfg := config.Default()
	cfg.PollInterval = 20 * time.Millisecond
	h := newHarness(t, cfg)
	kb := hidpptest.NewKeyboard(0)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()

	waitFor(t, "divert", func() bool { return allDiverted(kb) })
	kb.Reconnect() // diversion lost; Run's next re-check restores it
	h.clk.Advance(divertRecheck)
	waitFor(t, "re-divert", func() bool { return allDiverted(kb) })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	if !noneDiverted(kb) {
		t.Error("Run did not Release on ctx done")
	}
	if h.o.openHandles(kbPath) != 0 {
		t.Error("handle left open after Run")
	}
}

func TestReopenStillDivertedReadsHost(t *testing.T) {
	h := newHarness(t, config.Default())
	kb := hidpptest.NewKeyboard(2)
	h.o.add(hidppInfo(kbPath, "MX_Keys"), kb)
	defer h.s.Release()

	h.tick()
	h.o.kill(kbPath) // handle dies, keyboard keeps its diversion
	waitFor(t, "conn-closed event", func() bool { return len(h.s.gone) == 1 })
	h.tick()
	d := h.s.devs[kbPath]
	if d == nil || !d.diverted || d.host != 2 {
		t.Fatalf("reopened keyboard = %+v, want diverted with host 2", d)
	}
	if strings.Count(h.log.String(), "keys taken over") != 1 {
		t.Errorf("'keys taken over' re-logged for an already-diverted keyboard:\n%s", h.log)
	}
}
