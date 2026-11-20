//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"logiswitch/internal/logging"
	"logiswitch/internal/paths"
)

const (
	svcName     = "LogiSwitch"
	displayName = "LogiSwitch (mouse follows keyboard)"
	description = "Moves the Logitech mouse to the Easy-Switch channel chosen on the Logitech keyboard."
	stopTimeout = 10 * time.Second
	exeName     = "logiswitch.exe"
)

func install() error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if src, err = filepath.EvalSymlinks(src); err != nil {
		return err
	}
	if src, err = filepath.Abs(src); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	existing, err := m.OpenService(svcName)
	if err == nil {
		defer existing.Close()
		st, err := existing.Query()
		if err != nil {
			return err
		}
		if st.State != svc.Stopped {
			return errors.New("the service is running: stop the service first (logiswitch stop), then run install again")
		}
	} else {
		existing = nil
	}
	dataDir := logging.DataDir()
	warnings, err := logging.SecureDataDir(dataDir)
	if err != nil {
		return fmt.Errorf("securing %s: %w", dataDir, err)
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	exe, err := installExe(src)
	if err != nil {
		return err
	}
	if existing != nil {
		return updateService(existing, exe, dataDir)
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
	if err := finishInstall(s); err != nil {
		_ = s.Delete()
		return fmt.Errorf("%w (service removed again)", err)
	}
	fmt.Printf("installed service %s (%s), automatic start, LocalSystem\n  exe:  %s\n  data: %s\n", svcName, displayName, exe, dataDir)
	fmt.Println("start it now with: logiswitch start   (it also starts at every boot)")
	return nil
}

// updateService points an existing, stopped service at the freshly copied
// exe and reapplies its settings (an upgrade or repair).
func updateService(s *mgr.Service, exe, dataDir string) error {
	c, err := s.Config()
	if err != nil {
		return err
	}
	c.BinaryPathName = syscall.EscapeArg(exe)
	c.StartType = mgr.StartAutomatic
	c.DisplayName = displayName
	c.Description = description
	if err := s.UpdateConfig(c); err != nil {
		return fmt.Errorf("update service config: %w", err)
	}
	if err := finishInstall(s); err != nil {
		return err
	}
	fmt.Printf("updated existing service %s\n  exe:  %s\n  data: %s\n", svcName, exe, dataDir)
	fmt.Println("start it with: logiswitch start")
	return nil
}

// finishInstall sets the recovery actions and (re)creates the event source.
func finishInstall(s *mgr.Service) error {
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	// A stale source from an earlier, partly removed install would make
	// InstallAsEventCreate fail with "already exists".
	_ = eventlog.Remove(svcName)
	if err := eventlog.InstallAsEventCreate(svcName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		return fmt.Errorf("create event log source: %w", err)
	}
	return nil
}

// installDir returns <ProgramFiles>\LogiSwitch.
func installDir() (string, error) {
	pf, err := paths.ProgramFiles()
	if err != nil {
		return "", fmt.Errorf("locate Program Files: %w", err)
	}
	return filepath.Join(pf, "LogiSwitch"), nil
}

// installExe copies src to <ProgramFiles>\LogiSwitch\logiswitch.exe (a
// directory only admins can write, so the binary SYSTEM runs cannot be
// swapped by a user) and returns that path. The directory gets the same
// protected DACL as the data dir; the copy is written to a temp file there,
// renamed into place and reset to owner Administrators with inherited ACEs.
// If src already is that path, nothing is copied.
func installExe(src string) (string, error) {
	dir, err := installDir()
	if err != nil {
		return "", err
	}
	if err := logging.SecureDir(dir); err != nil {
		return "", fmt.Errorf("securing %s: %w", dir, err)
	}
	dst := filepath.Join(dir, exeName)
	if samePath(src, dst) {
		return dst, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, "logiswitch-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return "", fmt.Errorf("copy exe: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := logging.CheckFile(dst); err != nil {
		return "", err
	}
	from, err := windows.UTF16PtrFromString(tmpName)
	if err != nil {
		return "", err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return "", err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			// We already created a file in this admin-only directory, so
			// this is not missing elevation: the old exe is running. Not
			// wrapped with %w, so exitFor does not say "run as administrator".
			return "", fmt.Errorf("cannot replace %s: file in use; stop the service (logiswitch stop) and close any `logiswitch run`, then retry (%v)", dst, err)
		}
		return "", fmt.Errorf("install exe to %s: %w", dst, err)
	}
	ok = true
	if err := logging.ResetToInherited(dst); err != nil {
		return "", fmt.Errorf("reset security of %s: %w", dst, err)
	}
	return dst, nil
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
	fmt.Println("uninstalled service", svcName)
	if dir, err := installDir(); err == nil {
		fmt.Println("  program copy left in place:", filepath.Join(dir, exeName))
	}
	fmt.Println("  data left in place:", logging.DataDir())
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
// to report Stopped, retrying the request while the service cannot accept
// controls yet (e.g. still starting). A stopped service is not an error.
func stopAndWait(s *mgr.Service) error {
	deadline := time.Now().Add(stopTimeout)
	sent := false
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		if !sent && st.State != svc.StopPending {
			_, err := s.Control(svc.Stop)
			switch {
			case err == nil, errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
				sent = true
			case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL):
				// retry below until the deadline
			default:
				return fmt.Errorf("stop: %w", err)
			}
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
