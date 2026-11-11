//go:build !windows

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/update"
)

// underServiceManager reports whether the Windows service manager started
// this process. systemd runs `meshbridge run` like any other command.
func underServiceManager() bool { return false }

func runAsService(string, func(context.Context, io.Writer) error) error { panic("unreachable") }

// keepAwake is Windows-only for now; on Linux a bridge host is usually a
// machine that doesn't sleep.
func keepAwake(context.Context, *slog.Logger) {}

const (
	installHint = "sudo meshbridge install"
	setupHint   = "sudo meshbridge setup"
	runHint     = "sudo meshbridge run"
	elevateHint = "run it with sudo"
	// serviceSupported: install sets up a systemd service on Linux; macOS
	// has no service yet.
	serviceSupported = runtime.GOOS == "linux"
)

// applyStaged gets a staged update installed. The systemd unit installs it
// before the service starts (ExecStartPre runs apply-update as root, outside
// the sandbox), so the bridge only has to exit.
func applyStaged(u *autoUpdater) {
	if u.restart != nil {
		u.restart()
	}
}

// cmdApplyUpdate installs a staged update over this binary. systemd runs it
// before every start; it never fails, so a bad update can't keep the bridge
// from starting.
func cmdApplyUpdate(args []string) error {
	fs := flag.NewFlagSet("apply-update", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "bridge config file")
	_ = fs.Parse(args)
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshbridge: update not installed:", err)
		return nil
	}
	v, err := update.Apply(filepath.Dir(*path), exe, version, nil)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "meshbridge: update not installed:", err)
	case v != "":
		fmt.Printf("meshbridge: installed %s (was %s)\n", v, version)
	}
	return nil
}
