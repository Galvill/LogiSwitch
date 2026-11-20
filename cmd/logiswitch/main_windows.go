//go:build windows

// Command logiswitch makes a Logitech mouse follow the Easy-Switch channel
// of a Logitech keyboard. Run by the Service Control Manager it is the
// LogiSwitch service; from a console it installs and controls the service or
// runs one-shot commands (see usage).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"logiswitch/internal/config"
	"logiswitch/internal/hid"
	"logiswitch/internal/switcher"
)

const usageText = `usage: logiswitch <command>

  install     register the LogiSwitch service (automatic start, LocalSystem); admin
  uninstall   stop and remove the service; admin
  start       start the service; admin
  stop        stop the service; admin
  run         run in this console, logging to stdout (Ctrl+C to stop)
  list        show detected devices, feature indexes, current host, and
              whether the keys were already diverted (by another program)
  set N       send the mouse and keyboard to slot N (1-3)
  version     print the version`

func main() {
	if isSvc, err := svc.IsWindowsService(); err == nil && isSvc {
		runService()
		return
	}
	os.Exit(dispatch(os.Args[1:]))
}

func usage() int {
	fmt.Fprintln(os.Stderr, usageText)
	return 2
}

func dispatch(args []string) int {
	if len(args) == 0 {
		return usage()
	}
	cmd, rest := args[0], args[1:]
	if cmd == "set" {
		if len(rest) != 1 {
			return usage()
		}
		n, err := strconv.Atoi(rest[0])
		if err != nil || n < 1 || n > 3 {
			return usage()
		}
		return exitFor(cmdSet(n))
	}
	if len(rest) != 0 {
		return usage()
	}
	switch cmd {
	case "install":
		return exitFor(install())
	case "uninstall":
		return exitFor(uninstall())
	case "start":
		return exitFor(startService())
	case "stop":
		return exitFor(stopService())
	case "run":
		return exitFor(cmdRun())
	case "list":
		return exitFor(cmdList())
	case "version":
		fmt.Println("logiswitch", version)
		return 0
	}
	return usage()
}

// exitFor prints err (if any) and returns the process exit code.
func exitFor(err error) int {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		fmt.Fprintln(os.Stderr, "error: access denied: run as administrator")
	case errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE):
		fmt.Fprintln(os.Stderr, "error: the LogiSwitch service is marked for deletion (close Services/Event Viewer windows); wait or reboot, then retry")
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return 1
}

type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }

// consoleSetup refuses to run next to the service (both would divert the
// keys and switch the mouse), then loads the config (non-fatal) and returns
// a text logger to w at the configured level.
func consoleSetup(w *os.File) (config.Config, *slog.Logger, error) {
	if serviceRunning() {
		return config.Config{}, nil, errors.New("the LogiSwitch service is running and owns the devices; stop it first: logiswitch stop")
	}
	cfg, err := config.Load(config.DefaultPath())
	log := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err != nil {
		log.Error("config load failed; using defaults", "err", err)
	}
	return cfg, log, nil
}

func cmdRun() error {
	cfg, log, err := consoleSetup(os.Stdout)
	if err != nil {
		return err
	}
	// Ctrl+C, and closing the console window (delivered as SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("running; press Ctrl+C to stop and release the keys",
		"mapping", fmt.Sprint(cfg.MouseSlotFor), "poll", cfg.PollInterval)
	switcher.New(hid.System, cfg, sysClock{}, log).Run(ctx)
	log.Info("stopped")
	return nil
}

func cmdList() error {
	cfg, log, err := consoleSetup(os.Stderr)
	if err != nil {
		return err
	}
	sw := switcher.New(hid.System, cfg, sysClock{}, log)
	sw.Tick()
	sw.Settle()
	st := sw.Status()
	sw.Release()
	if len(st) == 0 {
		fmt.Println("no devices found")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// WAS_DIVERTED is the state found before list diverted the keys itself:
	// true means something else (Logi Options+, a crashed LogiSwitch) holds
	// them.
	fmt.Fprintln(tw, "KIND\tHOST\tWAS_DIVERTED\tCHANGE_HOST\tREPROG\tPRODUCT\tPATH")
	for _, d := range st {
		host := "?"
		if d.CurrentHost > 0 {
			host = strconv.Itoa(d.CurrentHost)
		}
		div := "-"
		if d.Kind == "keyboard" {
			div = strconv.FormatBool(d.DivertedBefore)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t0x%02X\t0x%02X\t%s\t%s\n",
			d.Kind, host, div, d.ChangeHostIdx, d.ReprogIdx, d.Product, d.Path)
	}
	return tw.Flush()
}

func cmdSet(slot int) error {
	cfg, log, err := consoleSetup(os.Stderr)
	if err != nil {
		return err
	}
	sw := switcher.New(hid.System, cfg, sysClock{}, log)
	err = sw.SetAll(slot)
	mouse := false
	for _, d := range sw.Status() {
		mouse = mouse || d.Kind == "mouse"
	}
	sw.Release()
	if err != nil {
		return fmt.Errorf("set %d: %w", slot, err)
	}
	if mouse {
		fmt.Printf("switched to slot %d (mouse to slot %d)\n", slot, cfg.MouseSlotFor[slot-1]+1)
	} else {
		fmt.Printf("switched to slot %d (no mouse found)\n", slot)
	}
	return nil
}
