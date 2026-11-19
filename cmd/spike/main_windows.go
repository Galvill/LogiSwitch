//go:build windows

// Command spike is a THROWAWAY Windows service used to confirm on real
// hardware that a LocalSystem service can do HID++ I/O (including at the lock
// and login screens). It is deleted in a later task.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"logiswitch/internal/hid"
	"logiswitch/internal/hidpp"
)

const (
	svcName    = "LogiSwitchSpike"
	logiVendor = 0x046D
	usagePage  = 0xFF43
	usage      = 0x0202
	tick       = 2 * time.Second
	retryAfter = 10 * time.Second
)

type role string

const (
	roleKeyboard role = "keyboard"
	roleMouse    role = "mouse"
	roleOther    role = "other"
)

type dev struct {
	info    hid.Info
	conn    *hidpp.Conn
	role    role
	hostFI  byte
	reprog  byte
	hasHost bool
}

type spike struct {
	ctx  context.Context
	log  *slog.Logger
	mu   sync.Mutex
	devs map[string]*dev
	// failed maps path -> time of last failed open/classify.
	failed  map[string]time.Time
	lastSig string
	lastCon uint32
}

func main() {
	if len(os.Args) < 2 {
		isSvc, _ := svc.IsWindowsService()
		if isSvc {
			runService()
			return
		}
		fmt.Println("usage: spike.exe install | uninstall | run")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = install()
	case "uninstall":
		err = uninstall()
	case "run":
		runConsole()
	default:
		fmt.Println("usage: spike.exe install | uninstall | run")
		os.Exit(2)
	}
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}

func install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(svcName); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", svcName)
	}
	s, err := m.CreateService(svcName, exe, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: "LogiSwitch spike (throwaway)",
	})
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Println("installed", svcName, "(LocalSystem, automatic). Start with: Start-Service", svcName)
	return nil
}

func uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(svcName)
	if err != nil {
		return err
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			fmt.Println("stop:", err)
		}
		for i := 0; i < 50; i++ {
			if st, err := s.Query(); err != nil || st.State == svc.Stopped {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Println("uninstalled", svcName)
	return nil
}

func newLogger(console bool) (*slog.Logger, func(), error) {
	dir := os.Getenv("ProgramData")
	if dir == "" {
		dir = `C:\ProgramData`
	}
	dir = filepath.Join(dir, "LogiSwitch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "spike.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	var w io.Writer = f
	if console {
		w = io.MultiWriter(f, os.Stdout)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})), func() { f.Close() }, nil
}

func runConsole() {
	log, closeLog, err := newLogger(true)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	defer closeLog()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	newSpike(ctx, log).run("console")
}

func runService() {
	log, closeLog, err := newLogger(false)
	if err != nil {
		os.Exit(1)
	}
	defer closeLog()
	if err := svc.Run(svcName, &handler{log: log}); err != nil {
		log.Error("svc.Run failed", "err", err)
	}
}

type handler struct{ log *slog.Logger }

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	st <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		newSpike(ctx, h.log).run("service")
	}()
	st <- svc.Status{State: svc.Running, Accepts: accepted}
	for c := range req {
		switch c.Cmd {
		case svc.Interrogate:
			st <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			h.log.Info("service stop/shutdown requested", "cmd", c.Cmd)
			st <- svc.Status{State: svc.StopPending}
			cancel()
			<-done
			return false, 0
		}
	}
	cancel()
	<-done
	return false, 0
}

func newSpike(ctx context.Context, log *slog.Logger) *spike {
	return &spike{ctx: ctx, log: log, devs: map[string]*dev{}, failed: map[string]time.Time{}}
}

func (s *spike) sessionInfo() {
	var proc uint32
	perr := windows.ProcessIdToSessionId(uint32(os.Getpid()), &proc)
	con := windows.WTSGetActiveConsoleSessionId()
	s.log.Info("session context", "processSession", proc, "processSessionErr", perr, "activeConsoleSession", con)
	s.lastCon = con
}

func (s *spike) run(mode string) {
	s.log.Info("spike starting", "mode", mode, "asService", mode == "service", "pid", os.Getpid())
	s.sessionInfo()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		s.scan()
		select {
		case <-s.ctx.Done():
			s.shutdown()
			return
		case <-t.C:
		}
	}
}

func (s *spike) shutdown() {
	s.log.Info("stopping: undiverting and closing devices")
	s.mu.Lock()
	devs := s.devs
	s.devs = map[string]*dev{}
	s.mu.Unlock()
	for p, d := range devs {
		if d.role == roleKeyboard {
			for _, cid := range hidpp.HostSwitchCIDs {
				err := d.conn.SetDiverted(d.reprog, cid, false)
				s.log.Info("undivert", "path", p, "cid", fmt.Sprintf("0x%X", cid), "err", err)
			}
		}
		s.log.Info("close", "path", p, "err", d.conn.Close())
	}
}

func (s *spike) scan() {
	if con := windows.WTSGetActiveConsoleSessionId(); con != s.lastCon {
		s.log.Info("active console session changed", "from", s.lastCon, "to", con)
		s.sessionInfo()
	}
	infos, err := hid.System.Enumerate(logiVendor)
	if err != nil {
		s.log.Error("enumerate failed", "err", err)
		return
	}
	var sig string
	found := map[string]hid.Info{}
	for _, in := range infos {
		sig += fmt.Sprintf("%s|%04x|%04x|%d|%d;", in.Path, in.UsagePage, in.Usage, in.InputLen, in.OutputLen)
		if in.UsagePage == usagePage && in.Usage == usage {
			found[in.Path] = in
		}
	}
	if sig != s.lastSig {
		s.lastSig = sig
		s.log.Info("enumeration changed", "count", len(infos), "hidppCollections", len(found))
		for _, in := range infos {
			s.log.Info("enumerated", "path", in.Path, "usagePage", fmt.Sprintf("0x%04X", in.UsagePage),
				"usage", fmt.Sprintf("0x%04X", in.Usage), "in", in.InputLen, "out", in.OutputLen,
				"pid", fmt.Sprintf("0x%04X", in.ProductID))
		}
	}
	for p := range s.failed {
		if _, ok := found[p]; !ok {
			delete(s.failed, p)
		}
	}
	for p, in := range found {
		s.mu.Lock()
		d := s.devs[p]
		s.mu.Unlock()
		if d != nil {
			if d.role == roleKeyboard {
				s.ensureDiverted(p, d)
			}
			continue
		}
		if t, ok := s.failed[p]; ok && time.Since(t) < retryAfter {
			continue
		}
		if d := s.openClassify(p, in); d != nil {
			delete(s.failed, p)
		} else {
			s.failed[p] = time.Now()
		}
	}
}

func (s *spike) openClassify(p string, in hid.Info) *dev {
	log := s.log.With("path", p)
	hd, err := hid.System.Open(p)
	if err != nil {
		log.Error("open failed", "err", err)
		return nil
	}
	conn := hidpp.NewConn(hd, 0xFF)
	hostFI, herr := conn.FeatureIndex(hidpp.FeatChangeHost)
	reprog, rerr := conn.FeatureIndex(hidpp.FeatReprogV4)
	log.Info("opened", "pid", fmt.Sprintf("0x%04X", in.ProductID),
		"changeHostIdx", hostFI, "changeHostErr", herr, "reprogIdx", reprog, "reprogErr", rerr)
	d := &dev{info: in, conn: conn, role: roleOther, hostFI: hostFI, reprog: reprog, hasHost: herr == nil}
	if herr != nil && rerr != nil && !isNoFeature(herr) && !isNoFeature(rerr) {
		log.Error("feature lookup failed; closing", "herr", herr, "rerr", rerr)
		conn.Close()
		return nil
	}
	if d.hasHost {
		n, cur, err := conn.HostInfo(hostFI)
		log.Info("hostinfo", "hosts", n, "current", cur, "err", err)
		if rerr == nil {
			ctrls, err := conn.Controls(reprog)
			if err != nil {
				log.Error("controls failed; closing", "err", err)
				conn.Close()
				return nil
			}
			ok := 0
			for _, c := range ctrls {
				for _, h := range hidpp.HostSwitchCIDs {
					if c.CID == h && c.Flags&0x20 != 0 {
						ok++
					}
				}
			}
			log.Info("controls", "count", len(ctrls), "divertableHostKeys", ok)
			if ok == 3 {
				d.role = roleKeyboard
			}
		}
		if d.role != roleKeyboard {
			d.role = roleMouse
		}
	}
	log.Info("classified", "role", d.role)
	s.mu.Lock()
	s.devs[p] = d
	s.mu.Unlock()
	if d.role == roleKeyboard {
		s.ensureDiverted(p, d)
	}
	go s.pump(p, d)
	return d
}

func isNoFeature(err error) bool { return err == hidpp.ErrNoFeature }

func (s *spike) ensureDiverted(p string, d *dev) {
	for _, cid := range hidpp.HostSwitchCIDs {
		on, err := d.conn.Diverted(d.reprog, cid)
		if err != nil {
			s.log.Error("diverted query failed", "path", p, "cid", fmt.Sprintf("0x%X", cid), "err", err)
			return
		}
		if on {
			continue
		}
		err = d.conn.SetDiverted(d.reprog, cid, true)
		s.log.Info("divert", "path", p, "cid", fmt.Sprintf("0x%X", cid), "err", err)
	}
}

// pump logs notifications and handles presses for one device until its
// connection fails or the spike stops.
func (s *spike) pump(p string, d *dev) {
	log := s.log.With("path", p, "role", d.role)
	for m := range d.conn.Notifications() {
		cids := hidpp.DivertedCIDs(m)
		log.Info("notification", "feature", m.Feature, "fn", m.Function, "swid", m.SWID,
			"params", fmt.Sprintf("%x", m.Params[:]), "divertedCIDs", cids)
		if d.role != roleKeyboard || m.Feature != d.reprog {
			continue
		}
		for _, cid := range cids {
			if cid >= 0xD1 && cid <= 0xD3 {
				s.switchTo(int(cid-0xD1), p)
				break
			}
		}
	}
	log.Warn("notifications closed; dropping device (will re-enumerate)")
	s.mu.Lock()
	if s.devs[p] == d {
		delete(s.devs, p)
	}
	s.mu.Unlock()
	d.conn.Close()
}

func (s *spike) switchTo(k int, kbPath string) {
	s.log.Info("press", "slot", k)
	s.mu.Lock()
	var mice, kbs []*dev
	for _, d := range s.devs {
		switch d.role {
		case roleMouse:
			mice = append(mice, d)
		case roleKeyboard:
			kbs = append(kbs, d)
		}
	}
	s.mu.Unlock()
	for _, m := range mice {
		err := m.conn.SetHost(m.hostFI, k, 200*time.Millisecond)
		s.log.Info("mouse SetHost", "path", m.info.Path, "slot", k, "err", err)
	}
	for _, kb := range kbs {
		if kb.info.Path != kbPath {
			continue
		}
		err := kb.conn.SetHost(kb.hostFI, k, hidpp.DefaultTimeout)
		s.log.Info("keyboard SetHost", "path", kb.info.Path, "slot", k, "err", err)
	}
}
