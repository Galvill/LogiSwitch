package switcher

import (
	"slices"
	"time"

	"logiswitch/internal/hidpp"
)

const (
	// suspendWait caps how long Suspend blocks its caller: Windows allows a
	// service only a couple of seconds to handle PBT_APMSUSPEND.
	suspendWait = 3 * time.Second
	// suspendHold is how long upkeep stays paused after Suspend when no
	// Resume follows (e.g. the sleep was aborted). Without the pause the
	// next poll, before the PC actually sleeps, would re-divert the keys.
	suspendHold = time.Minute
)

// Release undiverts D1-D3 on open keyboards and closes every device. It
// first waits for in-flight workers so that a keyboard still being opened is
// released too. It must be called on the owner goroutine (Run does so
// itself); it is safe to call more than once, and Tick and Settle do nothing
// afterwards.
func (s *Switcher) Release() {
	if s.released {
		return
	}
	s.released = true
	s.releaseDevices()
	close(s.quit)
	s.wg.Wait()
}

// Suspend asks Run to give the keys back and close every device because the
// PC is about to sleep or hibernate (Fast Startup shutdown included): a
// diverted keyboard whose host sleeps would have dead Easy-Switch keys. The
// pending mouse switch is dropped. Run keeps going but pauses upkeep until
// Resume, or for suspendHold. Suspend may be called from any goroutine; it
// blocks until Run has done so, at most suspendWait, and reports whether it
// finished in time. It returns false at once if Run has already returned.
func (s *Switcher) Suspend() bool {
	done := make(chan struct{})
	t := time.NewTimer(suspendWait)
	defer t.Stop()
	select {
	case s.suspendReq <- done:
	case <-s.quit:
		return false
	case <-t.C:
		return false
	}
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// Resume tells Run the PC has woken: upkeep restarts at once and
// rediscovers and re-diverts the devices. It may be called from any
// goroutine and never blocks.
func (s *Switcher) Resume() {
	select {
	case s.resumeReq <- struct{}{}:
	default:
	}
}

// suspend is Suspend's work on the owner goroutine.
func (s *Switcher) suspend() {
	if s.released {
		return
	}
	s.pend = nil
	s.releaseDevices()
	s.heldUntil = s.clk.Now().Add(suspendHold)
	s.log.Info("suspending: keys released, devices closed until resume")
}

// resume is Resume's work on the owner goroutine.
func (s *Switcher) resume() {
	if s.released || s.heldUntil.IsZero() {
		return
	}
	s.heldUntil = time.Time{}
	s.enumerated = false // devices are gone for now; do not log them missing
	s.log.Info("resumed; rediscovering devices")
	s.Tick()
}

// held reports whether upkeep is paused after a suspend, ending the pause
// once suspendHold has passed.
func (s *Switcher) held() bool {
	if s.heldUntil.IsZero() {
		return false
	}
	if s.clk.Now().Before(s.heldUntil) {
		return true
	}
	s.heldUntil = time.Time{}
	s.enumerated = false
	s.log.Info("no resume after suspend; resuming upkeep")
	return false
}

// releaseDevices waits for in-flight workers (so a keyboard still being
// opened is released too), undiverts D1-D3 on every keyboard, then closes
// and forgets every device and the pending switch. Owner goroutine only.
func (s *Switcher) releaseDevices() {
	for s.inflight > 0 {
		s.handleResult(<-s.results)
	}
	s.pend = nil
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
		if err := undivert(d); err != nil {
			// Expected when the keyboard has already gone.
			s.log.Info("undivert failed", "path", p, "err", err)
		} else {
			s.log.Info("keys released", "path", p)
		}
	}
	for _, d := range s.devs {
		d.conn.Close()
	}
	s.devs = map[string]*device{}
	s.pend = nil
}

// undivert returns D1-D3 to the keyboard firmware. It stops at the first
// failed write: a keyboard that has gone would fail the rest too, each
// after a full timeout.
func undivert(d *device) error {
	for _, cid := range hidpp.HostSwitchCIDs {
		if err := d.conn.SetDiverted(d.rpIdx, cid, false); err != nil {
			return err
		}
	}
	return nil
}

// logPresence logs "no keyboard found" / "keyboard found" (and the same for
// the mouse) when whether one is registered changes, and once when
// discovery first settles. It waits while opens are in flight, so a device
// still being classified is not reported missing.
func (s *Switcher) logPresence() {
	if !s.enumerated || len(s.opening) > 0 {
		return
	}
	for _, r := range []role{roleKeyboard, roleMouse} {
		have := false
		for _, d := range s.devs {
			if d.role == r {
				have = true
				break
			}
		}
		if was, ok := s.seen[r]; ok && was == have {
			continue
		}
		s.seen[r] = have
		if have {
			s.log.Info(r.String() + " found")
		} else {
			s.log.Info("no " + r.String() + " found")
		}
	}
}
