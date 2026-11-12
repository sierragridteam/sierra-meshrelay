//go:build !windows

package svc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Systemd install locations.
const (
	UnitName   = "meshbridge.service"
	UnitPath   = "/etc/systemd/system/" + UnitName
	BinaryPath = "/usr/local/bin/meshbridge"
)

// Unit renders the systemd unit for a bridge whose config is configPath.
//
// It runs as root, but sandboxed: the filesystem is read-only except the
// config directory (which holds the token, the control secret and
// state.json), with no access to home directories and no privilege
// escalation. Restart=always with a short delay keeps a long-lived bridge up
// through crashes; the bridge itself handles network and node outages.
//
// Updates: the sandboxed bridge can't replace its own binary, so it stages
// a verified release in the config directory and exits. ExecStartPre then
// runs apply-update with full privileges ("+") before the next start; it
// re-verifies the release's signature and never fails the start ("-").
func Unit(binary, configPath string) string {
	dir := filepath.Dir(configPath)
	return fmt.Sprintf(`# Installed by "meshbridge install". Reinstall to update; "meshbridge uninstall" to remove.
[Unit]
Description=SIERRA mesh bridge (MeshCore node to the SIERRA Mesh Relay)
Documentation=https://github.com/sierragridteam/sierra-meshrelay
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStartPre=-+%s apply-update -config %s
ExecStart=%s run -service -config %s
Restart=always
RestartSec=5
TimeoutStopSec=15

NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=%s
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
`, quote(binary), quote(configPath), quote(binary), quote(configPath), quote(dir))
}

// quote escapes a path for a systemd unit line.
func quote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Systemd installs and removes the service.
type Systemd struct {
	// Run executes a command (systemctl). Default: exec, output to Out.
	Run func(name string, args ...string) error
	// Root is prepended to the install paths (tests). Default "/".
	Root string
	Out  io.Writer
}

func (s *Systemd) path(p string) string { return filepath.Join(s.root(), p) }

func (s *Systemd) root() string {
	if s.Root == "" {
		return "/"
	}
	return s.Root
}

func (s *Systemd) out() io.Writer {
	if s.Out == nil {
		return os.Stdout
	}
	return s.Out
}

func (s *Systemd) run(name string, args ...string) error {
	if s.Run != nil {
		return s.Run(name, args...)
	}
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = s.out(), &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Install copies exe to BinaryPath, writes the unit, and (re)starts the
// service. Running it again upgrades in place.
func (s *Systemd) Install(exe, configPath string) error {
	bin := s.path(BinaryPath)
	if err := copyExecutable(exe, bin); err != nil {
		return fmt.Errorf("installing the binary to %s: %w", BinaryPath, err)
	}
	unit := Unit(BinaryPath, configPath)
	if err := os.MkdirAll(filepath.Dir(s.path(UnitPath)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path(UnitPath), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", UnitPath, err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", UnitName}, {"restart", UnitName}} {
		if err := s.run("systemctl", args...); err != nil {
			return err
		}
	}
	return nil
}

// Restart restarts the service; ErrNotInstalled if there's no unit.
func (s *Systemd) Restart() error {
	if _, err := os.Stat(s.path(UnitPath)); errors.Is(err, os.ErrNotExist) {
		return ErrNotInstalled
	}
	return s.run("systemctl", "restart", UnitName)
}

// Uninstall stops and removes the service. The config (and the bridge's
// token) is left in place.
func (s *Systemd) Uninstall() error {
	if _, err := os.Stat(s.path(UnitPath)); errors.Is(err, os.ErrNotExist) {
		return errors.New("the meshbridge service isn't installed")
	}
	if err := s.run("systemctl", "disable", "--now", UnitName); err != nil {
		return err
	}
	if err := os.Remove(s.path(UnitPath)); err != nil {
		return err
	}
	return s.run("systemctl", "daemon-reload")
}

// copyExecutable copies src to dst atomically (0755). Copying onto itself is
// a no-op.
func copyExecutable(src, dst string) error {
	if a, err := filepath.EvalSymlinks(src); err == nil {
		if b, err := filepath.EvalSymlinks(dst); err == nil && a == b {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename over the old binary even while it runs: the running process
	// keeps its inode, and the restart picks up the new one.
	return os.Rename(tmp, dst)
}
