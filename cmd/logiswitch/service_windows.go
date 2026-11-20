//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

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
	go func() {
		defer close(done)
		switcher.New(hid.System, cfg, sysClock{}, log).Run(ctx)
	}()
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	log.Info("service started")
	h.ev.info("LogiSwitch started")

loop:
	for c := range req {
		switch c.Cmd {
		case svc.Interrogate:
			st <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			log.Info("service stop requested", "cmd", c.Cmd)
			break loop
		}
	}

	// Run returns after Release has undiverted the keys and closed the
	// devices. Keep the SCM informed while it does.
	checkpoint := uint32(1)
	st <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 10000}
	cancel()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
wait:
	for {
		select {
		case <-done:
			break wait
		case <-t.C:
			checkpoint++
			st <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 10000}
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
	if err := logging.SecureDataDir(dir); err != nil {
		h.ev.error("cannot secure data directory; running with defaults, no file log, config.json ignored: " + err.Error())
		return slog.New(evHandler), config.Default(), func() {}
	}

	var problems []string
	cfgPath := config.DefaultPath()
	cfg := config.Default()
	if err := logging.CheckFile(cfgPath); err != nil {
		problems = append(problems, "config.json ignored: "+err.Error())
	} else if c, err := config.Load(cfgPath); err != nil {
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
