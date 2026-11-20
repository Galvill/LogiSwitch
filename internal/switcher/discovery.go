package switcher

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp"
)

var errNotTarget = errors.New("switcher: device lacks CHANGE_HOST")

// openResult is posted by an open worker: a ready device, or why not.
type openResult struct {
	info  hid.Info
	d     *device // nil on failure; conn already closed then
	what  string  // failure stage, for the log
	err   error   // errNotTarget: ignore the path for good
	stale bool    // the cached classification no longer matched
}

// startOpen opens, classifies and (for a keyboard) diverts in.Path on a
// worker goroutine. The owner commits the result in commitOpen.
func (s *Switcher) startOpen(in hid.Info) {
	s.opening[in.Path] = true
	cached, ok := s.known[in.Path]
	s.goWork(func() any { return s.openWorker(in, cached, ok) })
}

// openWorker runs off the owner goroutine: it must touch only its arguments,
// the opener and the logger.
func (s *Switcher) openWorker(in hid.Info, cached class, isCached bool) openResult {
	r := openResult{info: in}
	hd, err := s.opener.Open(in.Path)
	if err != nil {
		r.what, r.err = "open failed", err
		return r
	}
	conn := hidpp.NewConn(hd, bleDevIdx)
	c, stale, err := classify(conn, cached, isCached)
	r.stale = stale
	if stale {
		s.log.Info("device changed; re-classifying", "path", in.Path)
	}
	if err != nil {
		conn.Close()
		r.what, r.err = "classify failed", err
		return r
	}
	d := &device{path: in.Path, product: in.Product, class: c, conn: conn, host: -1}
	switch d.role {
	case roleMouse:
		if _, cur, err := conn.HostInfo(c.chIdx); err == nil {
			d.host = cur
		} else {
			s.log.Debug("mouse host unknown", "path", in.Path, "err", err)
		}
	case roleKeyboard:
		took, err := ensureDiverted(d)
		if err != nil {
			conn.Close()
			r.what, r.err = "divert failed", err
			return r
		}
		if took {
			s.logTakeover(d)
		}
	}
	r.d = d
	return r
}

// commitOpen registers a device posted by an open worker. A result for a
// path that vanished meanwhile, or that arrives after Release began, is
// closed (Release still registers it so the keyboard gets undiverted).
func (s *Switcher) commitOpen(r openResult) {
	p := r.info.Path
	delete(s.opening, p)
	if r.stale || errors.Is(r.err, errNotTarget) {
		delete(s.known, p)
	}
	if errors.Is(r.err, errNotTarget) {
		s.ignored[p] = true
		s.log.Info("ignoring device without CHANGE_HOST", "path", p, "product", r.info.Product)
		return
	}
	if r.err != nil {
		s.failed(p, r.what, r.err)
		return
	}
	d := r.d
	delete(s.retries, p)
	s.known[p] = d.class
	if !s.released && (!s.present[p] || s.devs[p] != nil) {
		d.conn.Close()
		s.log.Debug(d.role.String()+" open discarded; path gone", "path", p)
		return
	}
	s.devs[p] = d
	s.wg.Add(1)
	go s.forward(d)
	s.log.Info(d.role.String()+" opened", "path", d.path, "product", d.product,
		"changeHostIdx", d.chIdx, "reprogIdx", d.rpIdx)
	if d.role == roleKeyboard && s.pend != nil && !s.released {
		// The keyboard is back on this PC after the press, so the user did
		// not move on; the mouse stays.
		s.log.Info("pending mouse switch cancelled: keyboard returned", "path", d.path)
		s.pend = nil
	}
	if d.role == roleMouse && !s.released {
		// A mouse that wakes up while a switch is pending is sent on at once.
		s.upkeepPending(s.clk.Now())
	}
}

// failed records a transient failure on path (the caller has closed any
// handle). The path is retried after retryInterval. The first failure in a
// row logs at warn, repeats at debug.
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

// classify determines the role of the device on conn. A path classified
// earlier (cached) is only re-verified by its feature indexes: REPROG and
// CHANGE_HOST for a keyboard, CHANGE_HOST for a mouse. stale reports that
// the cache did not match and a full classification was done.
func classify(conn *hidpp.Conn, cached class, isCached bool) (c class, stale bool, err error) {
	if isCached {
		ok, err := verify(conn, cached)
		if err != nil {
			return class{}, false, err
		}
		if ok {
			return cached, false, nil
		}
		stale = true
	}
	ch, err := conn.FeatureIndex(hidpp.FeatChangeHost)
	if errors.Is(err, hidpp.ErrNoFeature) {
		return class{}, stale, errNotTarget
	}
	if err != nil {
		return class{}, stale, err
	}
	rp, err := conn.FeatureIndex(hidpp.FeatReprogV4)
	if errors.Is(err, hidpp.ErrNoFeature) {
		return class{role: roleMouse, chIdx: ch}, stale, nil
	}
	if err != nil {
		return class{}, stale, err
	}
	ctrls, err := conn.Controls(rp)
	if err != nil {
		return class{}, stale, err
	}
	if hasHostKeys(ctrls) {
		return class{role: roleKeyboard, chIdx: ch, rpIdx: rp}, stale, nil
	}
	return class{role: roleMouse, chIdx: ch, rpIdx: rp}, stale, nil
}

// verify reports whether the device still has the cached feature indexes.
func verify(conn *hidpp.Conn, c class) (bool, error) {
	check := func(feat uint16, want byte) (bool, error) {
		fi, err := conn.FeatureIndex(feat)
		if errors.Is(err, hidpp.ErrNoFeature) {
			return false, nil
		}
		return err == nil && fi == want, err
	}
	if c.role == roleKeyboard {
		if ok, err := check(hidpp.FeatReprogV4, c.rpIdx); !ok || err != nil {
			return ok, err
		}
	}
	return check(hidpp.FeatChangeHost, c.chIdx)
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
// yet known (a fresh handle on a keyboard that kept its diversion). took
// reports that diversion had to be (re)applied. It touches only kb, so an
// open worker may call it before kb is committed.
func ensureDiverted(kb *device) (took bool, err error) {
	on, err := kb.conn.Diverted(kb.rpIdx, hidpp.HostSwitchCIDs[0])
	if err != nil {
		return false, err
	}
	kb.diverted = on
	if !on {
		for _, cid := range hidpp.HostSwitchCIDs {
			if err := kb.conn.SetDiverted(kb.rpIdx, cid, true); err != nil {
				return false, err
			}
		}
		kb.diverted = true
	}
	if on && kb.host >= 0 {
		return false, nil
	}
	_, cur, err := kb.conn.HostInfo(kb.chIdx)
	if err != nil {
		return false, err
	}
	kb.host = cur
	return !on, nil
}

func (s *Switcher) logTakeover(kb *device) {
	s.log.Info("keys taken over", "path", kb.path, "product", kb.product, "slot", kb.host+1)
}
