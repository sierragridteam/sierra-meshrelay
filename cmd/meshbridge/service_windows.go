//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/logfile"
	msvc "github.com/sierragridteam/sierra-meshrelay/internal/svc"
	"github.com/sierragridteam/sierra-meshrelay/internal/update"
)

// LogFile is the service's log, beside its config.
const LogFile = "meshbridge.log"

func underServiceManager() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runAsService runs run under the service manager until it is told to stop.
// An error from run (a bad config, say) is logged and ends the service with
// a failure, which its recovery settings answer by restarting it.
func runAsService(dir string, run func(context.Context, io.Writer) error) error {
	out, err := logfile.Open(filepath.Join(dir, LogFile))
	if err != nil {
		return err
	}
	defer out.Close()
	h := &serviceHandler{run: run, out: out}
	if err := svc.Run(msvc.ServiceName, h); err != nil {
		fmt.Fprintf(out, "meshbridge: service manager: %v\n", err)
		return err
	}
	return h.err
}

type serviceHandler struct {
	run func(context.Context, io.Writer) error
	out io.Writer
	err error
}

func (h *serviceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx, h.out) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			// The bridge only returns on its own when something is wrong.
			if err == nil {
				err = fmt.Errorf("the bridge stopped unexpectedly")
			}
			h.err = err
			fmt.Fprintf(h.out, "time=%s level=ERROR msg=\"meshbridge exiting\" error=%q\n",
				time.Now().Format(time.RFC3339), err.Error())
			return true, 1
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
				}
				return false, 0
			}
		}
	}
}

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var setThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// keepAwake stops Windows sleeping on idle while ctx lives (host.keep_awake).
// The request belongs to the thread that made it, so it is made and later
// cleared from one locked thread. It doesn't keep the display on, and it
// doesn't stop someone choosing Sleep.
func keepAwake(ctx context.Context, log *slog.Logger) {
	ok := make(chan bool)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		r, _, err := setThreadExecutionState.Call(esContinuous | esSystemRequired)
		if r == 0 {
			log.Warn("can't keep this computer awake; it may sleep and take the bridge offline", "error", err)
			ok <- false
			return
		}
		ok <- true
		<-ctx.Done()
		setThreadExecutionState.Call(esContinuous)
	}()
	if <-ok {
		log.Info("keeping this computer awake while the bridge runs (host.keep_awake)")
	}
}

const (
	installHint = "meshbridge install, as administrator"
	setupHint   = "meshbridge setup, as administrator"
	runHint     = "meshbridge run, as administrator"
	elevateHint = "run it from an elevated PowerShell (Run as administrator)"
	// serviceSupported: install sets up a Windows service.
	serviceSupported = true
)

// applyStaged marks a staged update ready. The service's account can't
// write Program Files, so the "SIERRA meshbridge update" scheduled task
// (SYSTEM, every few minutes) installs it: see cmdApplyUpdate.
func applyStaged(u *autoUpdater) {
	if err := update.MarkReady(u.dir); err != nil {
		u.log.Error("can't mark the update ready", "error", err)
		return
	}
	u.log.Info("update ready; Windows installs it within a few minutes (scheduled task)")
	go u.watchStalled(15*time.Minute, 4*time.Hour,
		"the scheduled task ("+msvc.UpdateTask+") hasn't run it")
}

// cmdApplyUpdate, run by the scheduled task as SYSTEM, installs an update
// the bridge has staged and marked ready: stop the service, swap the binary,
// start it again. It writes what it did to the service's log, and leaves an
// update.Result that the bridge reports to the relay when it starts.
//
// With nothing to install, it brings the scheduled task itself up to date:
// SYSTEM can, and the bridge's own account can't.
func cmdApplyUpdate(args []string) error {
	fs := flag.NewFlagSet("apply-update", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "bridge config file")
	_ = fs.Parse(args)
	dir := filepath.Dir(*path)
	// Run by hand, it says what it did (the task's output goes nowhere).
	// Without access to the folder it would otherwise find nothing staged
	// and quietly do nothing.
	if _, err := os.ReadDir(dir); err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return &errNoBridge{*path}
		}
		return fmt.Errorf("can't read the bridge's folder %s: %s", dir, elevateHint)
	}
	staged, ready := update.Staged(dir)
	if !ready && msvc.UpdateTaskCurrent(*path) {
		if staged == "" {
			fmt.Println("No update is waiting to install. This bridge runs", version+".")
		} else {
			fmt.Printf("%s is downloaded but not ready: the bridge marks it ready once nobody is using the node.\n", staged)
		}
		return nil
	}
	out, err := logfile.Open(filepath.Join(dir, LogFile))
	if err != nil {
		return err
	}
	defer out.Close()
	say := func(level, msg string, kv ...any) {
		fmt.Fprintf(out, "time=%s level=%s msg=%q", time.Now().Format(time.RFC3339), level, msg)
		fmt.Print(msg)
		for i := 0; i+1 < len(kv); i += 2 {
			fmt.Fprintf(out, " %v=%q", kv[i], fmt.Sprint(kv[i+1]))
			fmt.Printf(" %v=%q", kv[i], fmt.Sprint(kv[i+1]))
		}
		fmt.Fprintln(out)
		fmt.Println()
	}
	if !ready {
		// Last, so that if replacing the task ends this run, nothing is lost.
		if err := msvc.RegisterUpdateTask(*path); err != nil {
			say("ERROR", "update: can't refresh the update task", "error", err)
		} else {
			say("INFO", "update: refreshed the update task", "revision", msvc.UpdateTaskRevision)
		}
		return nil
	}
	result := func(installed bool, err error) {
		r := update.Result{Version: staged, Was: version, Installed: installed}
		if err != nil {
			r.Error = err.Error()
		}
		if werr := update.WriteResult(dir, r); werr != nil {
			say("ERROR", "update: can't record the result", "error", werr)
		}
	}
	if err := msvc.StopService(); err != nil {
		say("ERROR", "update: can't stop the service; will retry", "error", err)
		result(false, fmt.Errorf("can't stop the service (will retry): %w", err))
		return nil
	}
	v, err := update.Apply(dir, msvc.BinaryPath(), version, nil)
	switch {
	case err != nil:
		say("ERROR", "update not installed", "version", staged, "error", err)
		result(false, err)
	case v != "":
		say("INFO", "update installed", "version", v, "was", version)
		result(true, nil)
	}
	if err := msvc.StartService(); err != nil {
		say("ERROR", "update: can't start the service again", "error", err)
	}
	return nil
}
