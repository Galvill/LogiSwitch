// Package switcher finds the Logitech keyboard and mouse, keeps the
// keyboard's Easy-Switch keys diverted to software, and moves the mouse to
// follow them.
//
// All switcher state is owned by one goroutine: Run, or the caller of Tick,
// Settle and Release. Other goroutines never touch it:
//   - per-device reader goroutines forward notifications and "connection
//     closed" into channels the owner drains;
//   - worker goroutines do slow I/O (opening and classifying a new path,
//     mouse setHost writes, mouse HostInfo probes) and post one result each
//     back to the owner, which commits it.
//
// So a keyboard press is never queued behind mouse or unknown-device I/O.
package switcher

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"logiswitch/internal/config"
	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp"
)

const (
	logitechVID = 0x046D
	bleDevIdx   = 0xFF
	// retryInterval throttles reopening a path whose open or classification
	// failed, so a flapping device does not spin.
	retryInterval = 2 * time.Second
	// flagTempDivertable is the REPROG_CONTROLS_V4 control flag for
	// temporary diversion.
	flagTempDivertable = 0x20
	// divertRecheck is how often an open, diverted keyboard is asked over
	// BLE whether it is still diverted. A reconnect is normally caught
	// sooner, by its path vanishing or its handle failing; this catches one
	// that kept the handle.
	divertRecheck = 5 * time.Second
)

// Clock supplies the current time; tests inject a fake.
type Clock interface{ Now() time.Time }

type role int

const (
	roleMouse role = iota + 1
	roleKeyboard
)

func (r role) String() string {
	switch r {
	case roleKeyboard:
		return "keyboard"
	case roleMouse:
		return "mouse"
	}
	return "unknown"
}

// class is the cached classification of a HID path.
type class struct {
	role         role
	chIdx, rpIdx byte // CHANGE_HOST, REPROG_CONTROLS_V4 indexes (rpIdx 0 if absent)
}

// device is one open, classified HID++ device. Once committed its mutable
// fields belong to the owner goroutine; workers only use conn and the
// immutable path and class.
type device struct {
	path, product string
	class
	conn *hidpp.Conn
	// host is the current host, 0-based; -1 unknown. For a leaving keyboard
	// it stays the host it is leaving (see leavingTo).
	host     int
	diverted bool // keyboard: D1-D3 diverted as of the last check
	// divertedBefore: keyboard: D1 was already diverted when this handle
	// first checked it (another program, or an instance that died, holds
	// the diversion).
	divertedBefore bool
	checkedAt      time.Time // keyboard: last successful divert check
	// leavingSince is when we changed the keyboard's host (zero if not): it
	// is on its way out to leavingTo (0-based), so divert upkeep pauses for
	// leavingGrace.
	leavingSince time.Time
	leavingTo    int
}

type event struct {
	d *device
	m hidpp.Message
}

type retry struct {
	at       time.Time // no reopen before this
	failures int
}

// Switcher is the device registry, upkeep loop and switch logic.
type Switcher struct {
	opener hid.Opener
	cfg    config.Config
	clk    Clock
	log    *slog.Logger

	devs    map[string]*device // open devices by path
	known   map[string]class   // classification cache, process lifetime
	ignored map[string]bool    // paths that answer but lack the features
	retries map[string]retry   // throttled paths
	present map[string]bool    // candidate paths seen by the last Enumerate
	opening map[string]bool    // paths with an open worker in flight
	pend    *pendingSwitch     // mouse switch not yet delivered
	gen     int                // pendingSwitch generation counter
	// seen records, per role, whether one was registered when presence was
	// last logged; enumerated is set once the first Tick has enumerated.
	seen       map[role]bool
	enumerated bool

	events   chan event   // notifications from reader goroutines
	gone     chan *device // reader goroutines report their conn closed
	results  chan any     // worker results: openResult, sendResult, probeResult
	inflight int          // workers started whose result is not yet handled
	quit     chan struct{}
	wg       sync.WaitGroup // readers and workers
	released bool

	suspendReq chan chan struct{} // Suspend -> Run; closed when done
	resumeReq  chan struct{}      // Resume -> Run (buffered 1)
	heldUntil  time.Time          // upkeep paused after a suspend until then
}

// New returns a Switcher; it does nothing until Tick or Run.
func New(o hid.Opener, cfg config.Config, clk Clock, log *slog.Logger) *Switcher {
	return &Switcher{
		opener:  o,
		cfg:     cfg,
		clk:     clk,
		log:     log,
		devs:    map[string]*device{},
		known:   map[string]class{},
		ignored: map[string]bool{},
		retries: map[string]retry{},
		present: map[string]bool{},
		opening: map[string]bool{},
		seen:    map[role]bool{},
		events:  make(chan event, 64),
		gone:    make(chan *device, 16),
		results: make(chan any, 16),
		quit:    make(chan struct{}),

		suspendReq: make(chan chan struct{}),
		resumeReq:  make(chan struct{}, 1),
	}
}

// Run ticks every PollInterval and handles device events, worker results and
// Suspend/Resume requests until ctx is done, then calls Release.
func (s *Switcher) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	s.Tick()
	for {
		select {
		case <-ctx.Done():
			s.Release()
			return
		case e := <-s.events:
			s.handleEvent(e)
		case d := <-s.gone:
			s.handleGone(d)
		case r := <-s.results:
			s.handleResult(r)
		case <-t.C:
			s.Tick()
		case done := <-s.suspendReq:
			s.suspend()
			close(done)
		case <-s.resumeReq:
			s.resume()
		}
	}
}

// Tick drains queued events and results, then does one upkeep pass: close
// devices that vanished, re-check diversion on open keyboards, start opening
// new paths, and push any pending mouse switch along. It does not wait for
// the workers it starts (see Settle). After Release, and while paused after
// a suspend, it only drains (or does nothing at all).
func (s *Switcher) Tick() {
	if s.released {
		return
	}
	s.drain()
	if s.held() {
		return
	}
	s.logPresence()
	infos, err := s.opener.Enumerate(logitechVID)
	if err != nil {
		s.log.Error("enumerate failed", "err", err)
		return
	}
	s.enumerated = true
	present := map[string]hid.Info{}
	var paths []string
	for _, in := range infos {
		if isCandidate(in) {
			if _, dup := present[in.Path]; !dup {
				paths = append(paths, in.Path)
			}
			present[in.Path] = in
		}
	}
	s.present = map[string]bool{}
	for p := range present {
		s.present[p] = true
	}
	for p, d := range s.devs {
		if !s.present[p] {
			s.drop(d, "removed")
		}
	}
	slices.Sort(paths)
	now := s.clk.Now()
	for _, p := range paths {
		if d := s.devs[p]; d != nil {
			if d.role == roleKeyboard && !s.keyboardLeaving(d, now) && divertDue(d, now) {
				took, err := ensureDiverted(d, now)
				if err != nil {
					s.drop(d, fmt.Sprintf("divert check failed: %v", err))
				} else if took {
					s.logTakeover(d)
				}
			}
			continue
		}
		if s.ignored[p] || s.opening[p] {
			continue
		}
		if r, ok := s.retries[p]; ok && now.Before(r.at) {
			continue
		}
		s.startOpen(present[p])
	}
	s.upkeepPending(now)
}

// Settle blocks until every worker started so far has posted its result and
// the result is committed (including workers those results start), then
// handles queued events. It is for one-shot commands that need a complete
// picture (list, set N) and for tests; Run never calls it. It must be called
// on the owner goroutine, and does nothing after Release.
func (s *Switcher) Settle() {
	for !s.released {
		s.drain()
		if s.inflight == 0 {
			return
		}
		s.handleResult(<-s.results)
	}
}

func isCandidate(in hid.Info) bool {
	return in.VendorID == logitechVID &&
		(in.UsagePage == 0xFF43 && in.Usage == 0x0202 || in.UsagePage == 0xFF00 && in.Usage == 0x0002)
}

// drain handles every queued result, event and closure without blocking.
func (s *Switcher) drain() {
	for {
		select {
		case r := <-s.results:
			s.handleResult(r)
			continue
		default:
		}
		select {
		case e := <-s.events:
			s.handleEvent(e)
			continue
		default:
		}
		select {
		case d := <-s.gone:
			s.handleGone(d)
		default:
			return
		}
	}
}

func (s *Switcher) drainEvents() {
	for {
		select {
		case e := <-s.events:
			s.handleEvent(e)
		default:
			return
		}
	}
}

// goWork runs f on a worker goroutine, which posts f's result to the owner.
// Every started worker posts exactly one result unless quit is closed, which
// only happens after Release has collected them all.
func (s *Switcher) goWork(f func() any) {
	s.inflight++
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		r := f()
		select {
		case s.results <- r:
		case <-s.quit:
			if o, ok := r.(openResult); ok && o.d != nil {
				o.d.conn.Close()
			}
		}
	}()
}

func (s *Switcher) handleResult(r any) {
	s.inflight--
	switch r := r.(type) {
	case openResult:
		s.commitOpen(r)
	case sendResult:
		s.handleSend(r)
	case probeResult:
		s.handleProbe(r)
	}
}

func (s *Switcher) current(d *device) bool { return s.devs[d.path] == d }

func (s *Switcher) handleEvent(e event) {
	d, m := e.d, e.m
	if !s.current(d) || s.released {
		return
	}
	if d.role == roleKeyboard && m.Feature == d.rpIdx {
		s.onKeyboardEvent(d, m)
		return
	}
	s.log.Debug("ignoring notification", "path", d.path, "feature", m.Feature, "fn", m.Function)
}

func (s *Switcher) handleGone(d *device) {
	// The reader sent all of d's events before reporting it gone.
	s.drainEvents()
	if s.current(d) {
		s.drop(d, "connection closed")
	}
}

// drop closes d and forgets it; the path is rediscovered on a later Tick.
func (s *Switcher) drop(d *device, reason string) {
	d.conn.Close()
	if s.current(d) {
		delete(s.devs, d.path)
	}
	s.log.Info(d.role.String()+" closed", "path", d.path, "reason", reason)
	if d.role == roleMouse {
		s.mouseDropped(d)
	}
}

// forward relays d's notifications to the owner goroutine, then reports the
// connection closed.
func (s *Switcher) forward(d *device) {
	defer s.wg.Done()
	for m := range d.conn.Notifications() {
		select {
		case s.events <- event{d, m}:
		case <-s.quit:
			return
		}
	}
	select {
	case s.gone <- d:
	case <-s.quit:
	}
}

func fmtCIDs(cids []uint16) string {
	parts := make([]string, len(cids))
	for i, c := range cids {
		parts[i] = fmt.Sprintf("0x%X", c)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
