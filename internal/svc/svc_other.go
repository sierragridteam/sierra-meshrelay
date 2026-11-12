//go:build !windows

package svc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Supported reports whether this host can install the service, and why not.
func Supported() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("installing as a service isn't supported on %s yet; run `meshbridge run` instead", runtime.GOOS)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemd (systemctl) not found; run `meshbridge run` under your own supervisor instead")
	}
	if os.Geteuid() != 0 {
		return errors.New("installing the service needs root: run it with sudo")
	}
	return nil
}

// Install installs (or upgrades) and starts the systemd service.
func Install(exe, configPath string) (Installed, error) {
	if err := (&Systemd{}).Install(exe, configPath); err != nil {
		return Installed{}, err
	}
	return Installed{Binary: BinaryPath, CheckOn: []string{"sudo meshbridge status", "journalctl -u meshbridge -f"}}, nil
}

// Restart restarts the installed service, so it reads its config again.
func Restart() error { return (&Systemd{}).Restart() }

// Uninstall stops and removes the systemd service.
func Uninstall() error { return (&Systemd{}).Uninstall() }

// SecureConfigDir is a no-op here: the config and state files are written
// 0600 by their owner.
func SecureConfigDir(string) error { return nil }
