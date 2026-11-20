// Package switcher finds the Logitech keyboard and mouse, keeps the
// keyboard's Easy-Switch keys diverted to software, and moves the mouse to
// follow them.
//
// All switcher state is owned by one goroutine: Run, or the test calling Tick
// directly. Per-device reader goroutines only forward notifications and
// "connection closed" events into channels that the owner drains.
package switcher

import (
	"context"
	"errors"
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

// device is one open, classified HID++ device.
type device struct {
	path, product string
	class
	conn     *hidpp.Conn
	host     int  // current host, 0-based; -1 unknown
	diverted bool // keyboard: D1-D3 diverted as of the last check
}

type event struct {
	d *device
	m hidpp.Message
}

type retry struct {
	at       time.Time // no reopen before this
	failures int
}

// Switcher is the device registry and upkeep loop.
type Switcher struct {
	opener hid.Opener
	cfg    config.Config
	clk    Clock
	log    *slog.Logger

	devs    map[string]*device // open devices by path
	known   map[string]class   // classification cache, process lifetime
	ignored map[string]bool    // paths that answer but lack the features
	retries map[string]retry   // throttled paths

	events chan event   // notifications from reader goroutines
	gone   chan *device // reader goroutines report their conn closed
	quit   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
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
		events:  make(chan event, 64),
		gone:    make(chan *device, 16),
		quit:    make(chan struct{}),
	}
}

// Run ticks every PollInterval and handles device events until ctx is done,
// then calls Release.
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
		case <-t.C:
			s.Tick()
		}
	}
}

// Tick drains pending events, then does one upkeep pass: close devices that
// vanished, re-check diversion on open keyboards, open and classify new paths.
func (s *Switcher) Tick() {
	s.drain()
	infos, err := s.opener.Enumerate(logitechVID)
	if err != nil {
		s.log.Error("enumerate failed", "err", err)
		return
	}
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
	for p, d := range s.devs {
		if _, ok := present[p]; !ok {
			s.drop(d, "removed")
		}
	}
	slices.Sort(paths)
	now := s.clk.Now()
	for _, p := range paths {
		if d := s.devs[p]; d != nil {
			if d.role == roleKeyboard {
				if err := s.ensureDiverted(d); err != nil {
					s.drop(d, fmt.Sprintf("divert check failed: %v", err))
				}
			}
			continue
		}
		if s.ignored[p] {
			continue
		}
		if r, ok := s.retries[p]; ok && now.Before(r.at) {
			continue
		}
		s.open(present[p])
	}
}

// Release undiverts D1-D3 on open keyboards and closes every device. It is
// safe to call more than once.
func (s *Switcher) Release() {
	s.once.Do(func() {
		paths := make([]string, 0, len(s.devs))
		for p := range s.devs {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, p := range paths {
			d := s.devs[p]
			if d.role != roleKeyboard {
				continue
			}
			var errs []error
			for _, cid := range hidpp.HostSwitchCIDs {
				if err := d.conn.SetDiverted(d.rpIdx, cid, false); err != nil {
					errs = append(errs, err)
				}
			}
			if err := errors.Join(errs...); err != nil {
				// Expected when the keyboard has already gone.
				s.log.Info("undivert failed", "path", p, "err", err)
			} else {
				s.log.Info("keys released", "path", p)
			}
		}
		close(s.quit)
		for _, d := range s.devs {
			d.conn.Close()
		}
		s.devs = map[string]*device{}
		s.wg.Wait()
	})
}

func isCandidate(in hid.Info) bool {
	return in.VendorID == logitechVID &&
		(in.UsagePage == 0xFF43 && in.Usage == 0x0202 || in.UsagePage == 0xFF00 && in.Usage == 0x0002)
}

// drain handles every queued event and closure without blocking.
func (s *Switcher) drain() {
	for {
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

func (s *Switcher) current(d *device) bool { return s.devs[d.path] == d }

func (s *Switcher) handleEvent(e event) {
	d, m := e.d, e.m
	if !s.current(d) {
		return
	}
	if d.role == roleKeyboard && m.Feature == d.rpIdx {
		s.onKeyboardEvent(d, m)
		return
	}
	s.log.Debug("ignoring notification", "path", d.path, "feature", m.Feature, "fn", m.Function)
}

// onKeyboardEvent receives REPROG_CONTROLS_V4 notifications from a keyboard.
// Press handling is added in switch.go.
func (s *Switcher) onKeyboardEvent(kb *device, m hidpp.Message) {
	s.log.Debug("keyboard event", "path", kb.path, "cids", fmtCIDs(hidpp.DivertedCIDs(m)))
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
}

// open opens and classifies a path. On a transient failure the path is
// retried after retryInterval; a device lacking the features is ignored for
// the process lifetime.
func (s *Switcher) open(in hid.Info) {
	hd, err := s.opener.Open(in.Path)
	if err != nil {
		s.failed(in.Path, "open failed", err)
		return
	}
	conn := hidpp.NewConn(hd, bleDevIdx)
	c, err := s.classify(conn, in.Path)
	if errors.Is(err, errNotTarget) {
		conn.Close()
		s.ignored[in.Path] = true
		s.log.Info("ignoring device without CHANGE_HOST", "path", in.Path, "product", in.Product)
		return
	}
	if err != nil {
		conn.Close()
		s.failed(in.Path, "classify failed", err)
		return
	}
	delete(s.retries, in.Path)
	s.known[in.Path] = c
	d := &device{path: in.Path, product: in.Product, class: c, conn: conn, host: -1}
	if d.role == roleMouse {
		if _, cur, err := conn.HostInfo(c.chIdx); err == nil {
			d.host = cur
		} else {
			s.log.Debug("mouse host unknown", "path", in.Path, "err", err)
		}
	}
	s.devs[in.Path] = d
	s.wg.Add(1)
	go s.forward(d)
	s.log.Info(d.role.String()+" opened", "path", d.path, "product", d.product,
		"changeHostIdx", d.chIdx, "reprogIdx", d.rpIdx)
	if d.role == roleKeyboard {
		if err := s.ensureDiverted(d); err != nil {
			s.drop(d, fmt.Sprintf("divert failed: %v", err))
		}
	}
}

// failed records a transient failure on path and closes nothing (the caller
// already has). The first failure in a row logs at warn, repeats at debug.
func (s *Switcher) failed(path, what string, err error) {
	r := s.retries[path]
	r.failures++
	r.at = s.clk.Now().Add(retryInterval)
	s.retries[path] = r
	lvl := slog.LevelDebug
	if r.failures == 1 {
		lvl = slog.LevelWarn
	}
	s.log.Log(context.Background(), lvl, what+"; will retry", "path", path, "err", err, "failures", r.failures)
}

var errNotTarget = errors.New("switcher: device lacks CHANGE_HOST")

// classify determines the role of the device on conn. A path classified
// earlier is only re-verified by its role's key feature index.
func (s *Switcher) classify(conn *hidpp.Conn, path string) (class, error) {
	if c, ok := s.known[path]; ok {
		feat, want := uint16(hidpp.FeatReprogV4), c.rpIdx
		if c.role == roleMouse {
			feat, want = hidpp.FeatChangeHost, c.chIdx
		}
		fi, err := conn.FeatureIndex(feat)
		if err != nil && !errors.Is(err, hidpp.ErrNoFeature) {
			return class{}, err
		}
		if err == nil && fi == want {
			return c, nil
		}
		s.log.Info("device changed; re-classifying", "path", path)
		delete(s.known, path)
	}
	ch, err := conn.FeatureIndex(hidpp.FeatChangeHost)
	if errors.Is(err, hidpp.ErrNoFeature) {
		return class{}, errNotTarget
	}
	if err != nil {
		return class{}, err
	}
	rp, err := conn.FeatureIndex(hidpp.FeatReprogV4)
	if errors.Is(err, hidpp.ErrNoFeature) {
		return class{role: roleMouse, chIdx: ch}, nil
	}
	if err != nil {
		return class{}, err
	}
	ctrls, err := conn.Controls(rp)
	if err != nil {
		return class{}, err
	}
	if hasHostKeys(ctrls) {
		return class{role: roleKeyboard, chIdx: ch, rpIdx: rp}, nil
	}
	return class{role: roleMouse, chIdx: ch, rpIdx: rp}, nil
}

// hasHostKeys reports whether D1-D3 are all present and temp-divertable.
func hasHostKeys(ctrls []hidpp.Control) bool {
	for _, cid := range hidpp.HostSwitchCIDs {
		if !slices.ContainsFunc(ctrls, func(c hidpp.Control) bool {
			return c.CID == cid && c.Flags&flagTempDivertable != 0
		}) {
			return false
		}
	}
	return true
}

// ensureDiverted checks D1 and, if it is not diverted, diverts D1-D3 and
// caches the keyboard's current host. The host is also read when it is not
// yet known (a fresh handle on a keyboard that kept its diversion).
func (s *Switcher) ensureDiverted(kb *device) error {
	on, err := kb.conn.Diverted(kb.rpIdx, hidpp.HostSwitchCIDs[0])
	if err != nil {
		return err
	}
	kb.diverted = on
	if !on {
		for _, cid := range hidpp.HostSwitchCIDs {
			if err := kb.conn.SetDiverted(kb.rpIdx, cid, true); err != nil {
				return err
			}
		}
		kb.diverted = true
	}
	if on && kb.host >= 0 {
		return nil
	}
	_, cur, err := kb.conn.HostInfo(kb.chIdx)
	if err != nil {
		return err
	}
	kb.host = cur
	if !on {
		s.log.Info("keys taken over", "path", kb.path, "product", kb.product, "slot", cur+1)
	}
	return nil
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
