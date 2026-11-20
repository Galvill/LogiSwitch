//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"logiswitch/internal/config"
	"logiswitch/internal/hid"
	"logiswitch/internal/logging"
	"logiswitch/internal/switcher"
)

const (
	logMaxBytes = 1 << 20
	// Event IDs (EventCreate.exe message file accepts 1-1000).
	eidInfo  = 1
	eidError = 3
	// eventDedup suppresses repeats of an identical error in the event log.
	eventDedup = 10 * time.Minute
	// stopGiveUp bounds the wait for Release on Stop/Shutdown.
	stopGiveUp = 30 * time.Second

	// Power broadcast event types (EventType of an svc.PowerEvent).
	pbtAPMSuspend         = 0x4  // PBT_APMSUSPEND: sleep, hibernate, Fast Startup shutdown
	pbtAPMResumeSuspend   = 0x7  // PBT_APMRESUMESUSPEND: resumed, user present
	pbtAPMResumeAutomatic = 0x12 // PBT_APMRESUMEAUTOMATIC: resumed
)

func runService() {
	elog, err := eventlog.Open(svcName)
	if err != nil {
		elog = nil // source missing: run without an event log
	}
	ev := &eventLog{l: elog, seen: map[string]time.Time{}}
	if err := svc.Run(svcName, &handler{ev: ev}); err != nil {
		ev.error("service failed: " + err.Error())
	}
	if elog != nil {
		elog.Close()
	}
}

type handler struct{ ev *eventLog }

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	log, cfg, closeLog := h.setup()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sw := switcher.New(hid.System, cfg, sysClock{}, log)
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic; releasing keys and exiting", "panic", r)
				func() {
					defer func() { _ = recover() }()
					sw.Release()
				}()
				panic(r) // crash: the SCM recovery actions restart the service
			}
		}()
		sw.Run(ctx)
	}()
	// Power events too: sleep, and the default Windows shutdown (Fast
	// Startup hibernates session 0), send no Stop or Shutdown, yet must
	// give the keys back.
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPowerEvent}
	log.Info("service started", "version", version)
	h.ev.info("LogiSwitch started")

loop:
	for c := range req {
		switch c.Cmd {
		case svc.Interrogate:
			st <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			log.Info("service stop requested", "cmd", c.Cmd)
			break loop
		case svc.PowerEvent:
			switch c.EventType {
			case pbtAPMSuspend:
				log.Info("power: suspending")
				if !sw.Suspend() {
					log.Warn("power: release before suspend did not finish in time")
				}
			case pbtAPMResumeAutomatic, pbtAPMResumeSuspend:
				log.Info("power: resumed", "event", c.EventType)
				sw.Resume()
			}
		}
	}

	// Run returns after Release has undiverted the keys and closed the
	// devices. Keep the SCM informed while it does.
	checkpoint := uint32(1)
	st <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 10000}
	cancel()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	giveUp := time.After(stopGiveUp)
wait:
	for {
		select {
		case <-done:
			break wait
		case <-t.C:
			checkpoint++
			st <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 10000}
		case <-giveUp:
			log.Error("release did not finish; stopping anyway", "after", stopGiveUp)
			closeLog()
			h.ev.info("LogiSwitch stopped (release timed out)")
			return false, uint32(windows.ERROR_TIMEOUT)
		}
	}
	log.Info("service stopped")
	closeLog()
	h.ev.info("LogiSwitch stopped")
	return false, 0 // svc reports Stopped once Execute returns
}

// setup secures the data dir, then loads the config and opens the rotating
// log file. If the dir cannot be secured it fails closed: no file log, no
// config.json (defaults), errors to the event log only.
func (h *handler) setup() (*slog.Logger, config.Config, func()) {
	evHandler := slog.NewTextHandler(h.ev, &slog.HandlerOptions{
		Level: slog.LevelError,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{} // the event log has its own timestamp
			}
			return a
		},
	})
	dir := logging.DataDir()
	warnings, err := logging.SecureDataDir(dir)
	if err != nil {
		h.ev.error("cannot secure data directory; running with defaults, no file log, config.json ignored: " + err.Error())
		return slog.New(evHandler), config.Default(), func() {}
	}

	var problems []string
	cfg := config.Default()
	// Read only through the handle-validated path: the bytes come from the
	// handle whose type, link count and owner were checked.
	if data, err := logging.ReadTrustedFile(config.DefaultPath()); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, "config.json ignored; using defaults: "+err.Error())
		}
	} else if c, err := config.Parse(data); err != nil {
		problems = append(problems, "config load failed; using defaults: "+err.Error())
	} else {
		cfg = c
	}

	handlers := fanout{evHandler}
	closeLog := func() {}
	if w, err := logging.OpenRotating(logging.DefaultPath(), logMaxBytes); err != nil {
		problems = append(problems, "file log unavailable: "+err.Error())
	} else {
		handlers = append(handlers, slog.NewTextHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel}))
		closeLog = func() { w.Close() }
	}
	log := slog.New(handlers)
	for _, w := range warnings {
		log.Warn(w)
	}
	for _, p := range problems {
		log.Error(p)
	}
	return log, cfg, closeLog
}

// fanout sends each record to every handler that accepts its level.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// eventLog writes to the Windows event log (nil l: dropped). As an
// io.Writer it receives one formatted error record per Write, and drops a
// record identical to one written within eventDedup so a persistent fault
// polled every tick cannot flood the log.
type eventLog struct {
	l    *eventlog.Log
	mu   sync.Mutex
	seen map[string]time.Time
}

var _ io.Writer = (*eventLog)(nil)

func (e *eventLog) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	now := time.Now()
	e.mu.Lock()
	for k, t := range e.seen {
		if now.Sub(t) >= eventDedup {
			delete(e.seen, k)
		}
	}
	_, dup := e.seen[msg]
	if !dup {
		e.seen[msg] = now
	}
	e.mu.Unlock()
	if !dup {
		e.error(msg)
	}
	return len(p), nil
}

func (e *eventLog) info(msg string) {
	if e.l != nil {
		_ = e.l.Info(eidInfo, msg)
	}
}

func (e *eventLog) error(msg string) {
	if e.l != nil {
		_ = e.l.Error(eidError, msg)
	}
}
