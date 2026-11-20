//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"logiswitch/internal/logging"
)

const (
	svcName     = "LogiSwitch"
	displayName = "LogiSwitch (mouse follows keyboard)"
	description = "Moves the Logitech mouse to the Easy-Switch channel chosen on the Logitech keyboard."
	stopTimeout = 10 * time.Second
)

func install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(svcName); err == nil {
		s.Close()
		return errors.New("already installed (run: logiswitch uninstall first)")
	}
	dir := logging.DataDir()
	if err := logging.SecureDataDir(dir); err != nil {
		return fmt.Errorf("securing %s: %w", dir, err)
	}
	s, err := m.CreateService(svcName, exe, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: displayName,
		Description: description,
		// ServiceStartName empty = LocalSystem.
	})
	if err != nil {
		return err
	}
	defer s.Close()
	fail := func(what string, err error) error {
		_ = s.Delete()
		return fmt.Errorf("%s: %w (service removed again)", what, err)
	}
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, uint32((24 * time.Hour).Seconds())); err != nil {
		return fail("set recovery actions", err)
	}
	// A stale source from an earlier, partly removed install would make
	// InstallAsEventCreate fail with "already exists".
	_ = eventlog.Remove(svcName)
	if err := eventlog.InstallAsEventCreate(svcName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		return fail("create event log source", err)
	}
	fmt.Printf("installed service %s (%s), automatic start, LocalSystem\n  exe:  %s\n  data: %s\n", svcName, displayName, exe, dir)
	fmt.Println("start it now with: logiswitch start   (it also starts at every boot)")
	return nil
}

func uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := openService(m)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := stopAndWait(s); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err, "- deleting anyway; it is removed once it exits")
	}
	if err := s.Delete(); err != nil {
		return err
	}
	if err := eventlog.Remove(svcName); err != nil {
		fmt.Fprintln(os.Stderr, "warning: remove event log source:", err)
	}
	fmt.Println("uninstalled service", svcName, "(data in", logging.DataDir(), "kept)")
	return nil
}

func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := openService(m)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			fmt.Println(svcName, "is already running")
			return nil
		}
		return err
	}
	fmt.Println("started", svcName)
	return nil
}

func stopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := openService(m)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := stopAndWait(s); err != nil {
		return err
	}
	fmt.Println("stopped", svcName)
	return nil
}

func openService(m *mgr.Mgr) (*mgr.Service, error) {
	s, err := m.OpenService(svcName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, errors.New("service " + svcName + " is not installed")
	}
	return s, err
}

// stopAndWait asks the service to stop and waits up to stopTimeout for it
// to report Stopped. A service that is not running is not an error.
func stopAndWait(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("stop: %w", err)
		}
	}
	deadline := time.Now().Add(stopTimeout)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service did not stop within %s", stopTimeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// serviceRunning reports whether the LogiSwitch service is in any state
// other than Stopped. It needs only connect/query rights, so it works
// without elevation; if the query fails it returns false (proceed).
func serviceRunning() bool {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(scm)
	name, err := windows.UTF16PtrFromString(svcName)
	if err != nil {
		return false
	}
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return false
	}
	return st.CurrentState != windows.SERVICE_STOPPED
}
