package switcher

import (
	"fmt"
	"slices"
	"time"

	"logiswitch/internal/hidpp"
)

const (
	// pendingTTL is how long a mouse switch is retried for an asleep mouse.
	pendingTTL = 2 * time.Minute
	// mouseIOTimeout bounds every mouse write and probe so an unreachable
	// mouse costs little; they also run off the owner goroutine.
	mouseIOTimeout = 200 * time.Millisecond
	// keyboardWriteTimeout bounds the keyboard setHost write.
	keyboardWriteTimeout = hidpp.DefaultTimeout
	// probeAfter: a mouse still registered this long after a successful
	// setHost write is asked for its host (finding 5: a switching mouse's
	// collection closes within ~50-400 ms).
	probeAfter = time.Second
	// deliveryWindow: a mouse whose conn closes within this long after a
	// successful setHost write is taken to have switched.
	deliveryWindow = 2 * time.Second
	// leavingGrace: how long a keyboard we switched may stay registered
	// before we assume it did not leave and resume upkeep on it.
	leavingGrace = 5 * time.Second
)

// pendingSwitch is a mouse switch not yet known to be delivered. It survives
// the keyboard leaving; a new switch replaces it.
type pendingSwitch struct {
	gen     int // distinguishes results of a replaced switch
	slot    int // mouse target, 0-based
	expires time.Time
	// kbPaths are the keyboards this switch moved away. Re-committing one of
	// them cancels the switch (ruling 9); if none was moved, any keyboard
	// commit cancels it.
	kbPaths map[string]bool
	busy    map[*device]bool      // mouse has a write or probe in flight
	sent    map[*device]time.Time // mouse took a setHost write at this time
}

type sendResult struct {
	gen int
	d   *device
	at  time.Time
	err error
}

type probeResult struct {
	gen int
	d   *device
	cur int
	err error
}

// onKeyboardEvent handles a REPROG_CONTROLS_V4 notification from kb. A press
// acts on the first host-switch CID it lists; a release (no CIDs) is ignored.
func (s *Switcher) onKeyboardEvent(kb *device, m hidpp.Message) {
	cids := hidpp.DivertedCIDs(m)
	if len(cids) == 0 {
		return
	}
	k := -1
	for _, c := range cids {
		if i := slices.Index(hidpp.HostSwitchCIDs[:], c); i >= 0 {
			k = i
			break
		}
	}
	if k < 0 {
		s.log.Debug("keyboard event without host key", "path", kb.path, "cids", fmtCIDs(cids))
		return
	}
	if k == kb.host {
		// setHost to its own slot would make the keyboard reconnect.
		s.log.Debug("own slot pressed; ignoring", "path", kb.path, "slot", k+1)
		return
	}
	ms := s.cfg.MouseSlotFor[k]
	s.startSwitch([]*device{kb}, k, ms)
	s.log.Info(fmt.Sprintf("switch to slot %d", k+1), "mouseSlot", ms+1, "path", kb.path, "cids", fmtCIDs(cids))
}

// startSwitch records a pending mouse switch to mouseSlot (replacing any
// earlier one), starts setHost writes to every registered mouse on workers,
// then sends setHost(kbSlot) to each of kbs without waiting for the mice.
// Slots are 0-based. Owner goroutine only.
// It returns how many keyboard setHost writes succeeded.
func (s *Switcher) startSwitch(kbs []*device, kbSlot, mouseSlot int) (sent int) {
	now := s.clk.Now()
	s.gen++
	s.pend = &pendingSwitch{
		gen: s.gen, slot: mouseSlot, expires: now.Add(pendingTTL),
		busy: map[*device]bool{}, sent: map[*device]time.Time{},
		kbPaths: map[string]bool{},
	}
	p := s.pend
	for _, kb := range kbs {
		p.kbPaths[kb.path] = true
	}
	s.upkeepPending(now)
	for _, kb := range kbs {
		if err := kb.conn.SetHost(kb.chIdx, kbSlot, keyboardWriteTimeout); err != nil {
			s.log.Warn("keyboard setHost failed", "path", kb.path, "slot", kbSlot+1, "err", err)
			continue
		}
		kb.host = kbSlot // it is leaving for kbSlot
		kb.leavingSince = now
		sent++
	}
	return sent
}

// keyboardLeaving reports whether kb is within leavingGrace of a host change,
// so its divert check is skipped. When the grace ends with kb still here
// (the write did not make it leave), the flag is cleared and its host
// forgotten so the normal check re-reads it.
func (s *Switcher) keyboardLeaving(kb *device, now time.Time) bool {
	if kb.leavingSince.IsZero() {
		return false
	}
	if now.Sub(kb.leavingSince) < leavingGrace {
		return true
	}
	kb.leavingSince = time.Time{}
	kb.host = -1
	return false
}

// upkeepPending expires the pending switch or moves it along on each
// registered mouse: send setHost if not yet taken, probe if it was taken at
// least probeAfter ago and the mouse is still here.
func (s *Switcher) upkeepPending(now time.Time) {
	p := s.pend
	if p == nil || s.released {
		return
	}
	if !now.Before(p.expires) {
		s.log.Info("pending mouse switch expired", "slot", p.slot+1)
		s.pend = nil
		return
	}
	for _, d := range s.mice() {
		if s.pend != p {
			return
		}
		if p.busy[d] {
			continue
		}
		if d.host == p.slot {
			// Already there; setHost to its own slot would reconnect it.
			s.delivered(p, d, "already on target")
			return
		}
		t, sent := p.sent[d]
		switch {
		case !sent:
			s.startSend(p, d)
		case now.Sub(t) >= probeAfter:
			s.startProbe(p, d)
		}
	}
}

// mice returns the registered mice in path order.
func (s *Switcher) mice() []*device {
	var out []*device
	for _, d := range s.devs {
		if d.role == roleMouse {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b *device) int {
		if a.path < b.path {
			return -1
		}
		if a.path > b.path {
			return 1
		}
		return 0
	})
	return out
}

func (s *Switcher) startSend(p *pendingSwitch, d *device) {
	p.busy[d] = true
	gen, slot := p.gen, p.slot
	s.goWork(func() any {
		err := d.conn.SetHost(d.chIdx, slot, mouseIOTimeout)
		return sendResult{gen: gen, d: d, at: s.clk.Now(), err: err}
	})
}

func (s *Switcher) startProbe(p *pendingSwitch, d *device) {
	p.busy[d] = true
	gen := p.gen
	s.goWork(func() any {
		r, err := d.conn.Request(d.chIdx, 0, nil, mouseIOTimeout) // getHostInfo
		return probeResult{gen: gen, d: d, cur: int(r.Params[1]), err: err}
	})
}

// handleSend commits a mouse setHost write. A failed write is retried by
// the next Tick (or when the mouse is reopened). A successful write whose
// mouse has already been dropped counts as delivered.
func (s *Switcher) handleSend(r sendResult) {
	p := s.pend
	if p == nil || p.gen != r.gen {
		return
	}
	delete(p.busy, r.d)
	switch {
	case r.err != nil:
		s.log.Debug("mouse setHost failed; will retry", "path", r.d.path, "slot", p.slot+1, "err", r.err)
	case !s.current(r.d):
		s.delivered(p, r.d, "left during setHost")
	default:
		p.sent[r.d] = r.at
		s.log.Debug("mouse setHost sent", "path", r.d.path, "slot", p.slot+1)
	}
}

// handleProbe commits a HostInfo probe of a mouse that took setHost but is
// still registered: no answer or the target host means it switched; another
// host means the write was lost, so resend.
func (s *Switcher) handleProbe(r probeResult) {
	p := s.pend
	if p == nil || p.gen != r.gen {
		return
	}
	delete(p.busy, r.d)
	if !s.current(r.d) {
		return // mouseDropped already judged it
	}
	switch {
	case r.err != nil:
		s.delivered(p, r.d, "stopped answering")
	case r.cur == p.slot:
		r.d.host = r.cur
		s.delivered(p, r.d, "reports target host")
	default:
		r.d.host = r.cur
		delete(p.sent, r.d)
		if !s.released && s.clk.Now().Before(p.expires) {
			s.log.Info("mouse still here after setHost; resending", "path", r.d.path, "slot", p.slot+1)
			s.startSend(p, r.d)
		}
	}
}

// mouseDropped judges a pending switch when mouse d's conn closes or its path
// vanishes: within deliveryWindow of a successful write it has switched.
func (s *Switcher) mouseDropped(d *device) {
	p := s.pend
	if p == nil {
		return
	}
	t, ok := p.sent[d]
	if !ok {
		return
	}
	delete(p.sent, d)
	if s.clk.Now().Sub(t) <= deliveryWindow {
		s.delivered(p, d, "left after setHost")
	}
}

func (s *Switcher) delivered(p *pendingSwitch, d *device, how string) {
	s.log.Info("mouse switched", "path", d.path, "slot", p.slot+1, "how", how)
	if s.pend == p {
		s.pend = nil
	}
}
