//go:build windows

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
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	wsvc "golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// InstallDir is where the service's binary lives.
func InstallDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "SIERRA", "meshbridge")
}

// BinaryPath is the installed binary.
func BinaryPath() string { return filepath.Join(InstallDir(), "meshbridge.exe") }

// Supported reports whether this host can install the service, and why not.
func Supported() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("installing the service needs an administrator: run it from an elevated PowerShell (Run as administrator)")
	}
	return nil
}

func serviceConfig() mgr.Config {
	return mgr.Config{
		DisplayName:      "SIERRA mesh bridge",
		Description:      "Connects a MeshCore companion node to the SIERRA Mesh Relay. https://github.com/sierragridteam/sierra-meshrelay",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: ServiceAccount,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
}

// commandLine is how CreateService quotes the service's command line.
func commandLine(exe string, args ...string) string {
	s := syscall.EscapeArg(exe)
	for _, a := range args {
		s += " " + syscall.EscapeArg(a)
	}
	return s
}

// Install copies exe into InstallDir, creates (or updates) the service,
// grants it the config folder, and (re)starts it. Running it again upgrades
// in place.
func Install(exe, configPath string) (Installed, error) {
	m, err := mgr.Connect()
	if err != nil {
		return Installed{}, fmt.Errorf("connecting to the service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	exists := err == nil
	if exists {
		defer s.Close()
		// Windows won't replace a running binary.
		if err := stop(s); err != nil {
			return Installed{}, err
		}
	}
	bin := BinaryPath()
	if err := copyExecutable(exe, bin); err != nil {
		return Installed{}, fmt.Errorf("installing the binary to %s: %w", bin, err)
	}
	args := []string{"run", "-config", configPath}
	if exists {
		c := serviceConfig()
		c.ServiceType = windows.SERVICE_WIN32_OWN_PROCESS
		c.BinaryPathName = commandLine(bin, args...)
		if err := s.UpdateConfig(c); err != nil {
			return Installed{}, fmt.Errorf("updating the service: %w", err)
		}
	} else {
		if s, err = m.CreateService(ServiceName, bin, serviceConfig(), args...); err != nil {
			return Installed{}, fmt.Errorf("creating the service: %w", err)
		}
		defer s.Close()
	}
	// Restart after any failure, including the bridge exiting with an error
	// (a "non-crash" failure to Windows). The count resets after a day up.
	restart := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(restart, 24*60*60); err != nil {
		return Installed{}, fmt.Errorf("setting the service to restart on failure: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return Installed{}, fmt.Errorf("setting the service to restart on failure: %w", err)
	}
	if err := SecureConfigDir(filepath.Dir(configPath)); err != nil {
		return Installed{}, err
	}
	if err := RegisterUpdateTask(configPath); err != nil {
		return Installed{}, fmt.Errorf("creating the update task: %w", err)
	}
	if err := start(s); err != nil {
		return Installed{}, fmt.Errorf("%w; see %s", err, filepath.Join(filepath.Dir(configPath), "meshbridge.log"))
	}
	return Installed{Binary: bin, CheckOn: []string{
		"meshbridge status",
		"Get-Content -Wait " + filepath.Join(filepath.Dir(configPath), "meshbridge.log"),
	}}, nil
}

// Uninstall stops and removes the service. The binary and the config (and
// the bridge's token) are left in place.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return errors.New("the meshbridge service isn't installed")
	}
	defer s.Close()
	if err := stop(s); err != nil {
		return err
	}
	_ = schtasks("/Delete", "/TN", UpdateTask, "/F")
	return s.Delete()
}

// RegisterUpdateTask creates (or replaces) UpdateTask and records its
// revision beside the config.
func RegisterUpdateTask(configPath string) error {
	bin := BinaryPath()
	args := "apply-update -config " + syscall.EscapeArg(configPath)
	f, err := os.CreateTemp("", "meshbridge-update-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(updateTaskXML(bin, args))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// The marker goes first: apply-update refreshes the task from inside a
	// run of that task, and if replacing the task ends the run, the new
	// task must still read as current.
	marker := filepath.Join(filepath.Dir(configPath), UpdateTaskMarker)
	if err := os.WriteFile(marker, []byte(UpdateTaskRevision+"\n"), 0o644); err != nil {
		return err
	}
	if err := schtasks("/Create", "/TN", UpdateTask, "/XML", f.Name(), "/RU", "SYSTEM", "/F"); err != nil {
		_ = os.Remove(marker)
		return err
	}
	return nil
}

// UpdateTaskCurrent reports whether the registered task is the current
// revision (as recorded by RegisterUpdateTask).
func UpdateTaskCurrent(configPath string) bool {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), UpdateTaskMarker))
	return err == nil && strings.TrimSpace(string(b)) == UpdateTaskRevision
}

func schtasks(args ...string) error {
	cmd := exec.Command("schtasks", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// StopService stops the installed service and waits for it.
func StopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	return stop(s)
}

// StartService starts the installed service.
func StartService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return err
	}
	return waitFor(s, wsvc.Running, 30*time.Second)
}

// SecureConfigDir restricts dir to SYSTEM, Administrators and, once it
// exists, the service account.
func SecureConfigDir(dir string) error {
	withService := false
	if m, err := mgr.Connect(); err == nil {
		if s, err := m.OpenService(ServiceName); err == nil {
			withService = true
			s.Close()
		}
		m.Disconnect()
	}
	if err := icacls(icaclsArgs(dir, withService)); err != nil {
		return fmt.Errorf("restricting access to %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := icacls(resetArgs(filepath.Join(dir, e.Name()))); err != nil {
			return fmt.Errorf("restricting access to %s: %w", filepath.Join(dir, e.Name()), err)
		}
	}
	return nil
}

func icacls(args []string) error {
	cmd := exec.Command("icacls", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func stop(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("querying the service: %w", err)
	}
	if st.State == wsvc.Stopped {
		return nil
	}
	if st.State != wsvc.StopPending {
		if _, err := s.Control(wsvc.Stop); err != nil {
			return fmt.Errorf("stopping the service: %w", err)
		}
	}
	return waitFor(s, wsvc.Stopped, 30*time.Second)
}

func start(s *mgr.Service) error {
	if err := s.Start(); err != nil {
		return fmt.Errorf("starting the service: %w", err)
	}
	if err := waitFor(s, wsvc.Running, 30*time.Second); err != nil {
		return err
	}
	// A bridge that can't read its config exits within a moment.
	time.Sleep(2 * time.Second)
	if st, err := s.Query(); err != nil || st.State != wsvc.Running {
		return fmt.Errorf("the service started but stopped again (%s)", exitText(st))
	}
	return nil
}

// exitText describes why a stopped service stopped.
func exitText(st wsvc.Status) string {
	if st.Win32ExitCode == uint32(windows.ERROR_SERVICE_SPECIFIC_ERROR) {
		return fmt.Sprintf("meshbridge exit code %d", st.ServiceSpecificExitCode)
	}
	return fmt.Sprintf("Windows error %d: %v", st.Win32ExitCode, syscall.Errno(st.Win32ExitCode))
}

func waitFor(s *mgr.Service, want wsvc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("querying the service: %w", err)
		}
		if st.State == want {
			return nil
		}
		if want == wsvc.Running && st.State == wsvc.Stopped {
			return fmt.Errorf("the service stopped while starting (%s)", exitText(st))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service didn't reach state %d within %s (it is in state %d, %s)", want, timeout, st.State, exitText(st))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// copyExecutable copies src to dst. Copying onto itself is a no-op. dst must
// not be running (Install stops the service first).
func copyExecutable(src, dst string) error {
	if a, err := filepath.Abs(src); err == nil {
		if b, err := filepath.Abs(dst); err == nil && strings.EqualFold(a, b) {
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
	out, err := os.Create(tmp)
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
	return os.Rename(tmp, dst)
}
