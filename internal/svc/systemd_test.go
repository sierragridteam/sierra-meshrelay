//go:build !windows

package svc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnit(t *testing.T) {
	u := Unit("/usr/local/bin/meshbridge", "/etc/meshbridge/meshbridge.toml")
	for _, want := range []string{
		"ExecStart=/usr/local/bin/meshbridge run -service -config /etc/meshbridge/meshbridge.toml\n",
		"ExecStartPre=-+/usr/local/bin/meshbridge apply-update -config /etc/meshbridge/meshbridge.toml\n",
		"ReadWritePaths=/etc/meshbridge\n",
		"After=network-online.target\n",
		"Restart=always\n",
		"ProtectSystem=strict\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if q := Unit("/opt/my bin/meshbridge", "/srv/a b/c.toml"); !strings.Contains(q, `ExecStart="/opt/my bin/meshbridge" run -service -config "/srv/a b/c.toml"`) ||
		!strings.Contains(q, `ReadWritePaths="/srv/a b"`) {
		t.Errorf("paths with spaces not quoted:\n%s", q)
	}
}

func TestInstallAndUninstall(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(t.TempDir(), "meshbridge")
	if err := os.WriteFile(exe, []byte("binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	var ran []string
	s := &Systemd{Root: root, Run: func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}}
	if err := s.Install(exe, "/etc/meshbridge/meshbridge.toml"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, BinaryPath))
	fi, _ := os.Stat(filepath.Join(root, BinaryPath))
	if string(got) != "binary v1" || fi.Mode().Perm() != 0o755 {
		t.Fatalf("binary %q mode %v", got, fi.Mode().Perm())
	}
	unit, _ := os.ReadFile(filepath.Join(root, UnitPath))
	if !strings.Contains(string(unit), "ExecStart="+BinaryPath+" run") {
		t.Fatalf("unit:\n%s", unit)
	}
	want := []string{"systemctl daemon-reload", "systemctl enable meshbridge.service", "systemctl restart meshbridge.service"}
	if strings.Join(ran, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %v", ran)
	}

	// Installing a new binary upgrades in place.
	_ = os.WriteFile(exe, []byte("binary v2"), 0o755)
	if err := s.Install(exe, "/etc/meshbridge/meshbridge.toml"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, BinaryPath)); string(got) != "binary v2" {
		t.Fatalf("after upgrade: %q", got)
	}

	ran = nil
	if err := s.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, UnitPath)); !os.IsNotExist(err) {
		t.Fatal("unit still there")
	}
	if strings.Join(ran, "|") != "systemctl disable --now meshbridge.service|systemctl daemon-reload" {
		t.Fatalf("ran %v", ran)
	}
	if err := s.Uninstall(); err == nil {
		t.Fatal("uninstalling twice should say it isn't installed")
	}
}

func TestRestart(t *testing.T) {
	root := t.TempDir()
	var ran []string
	s := &Systemd{Root: root, Run: func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}}
	if err := s.Restart(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("no unit: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, UnitPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, UnitPath), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart(); err != nil || strings.Join(ran, "|") != "systemctl restart meshbridge.service" {
		t.Fatalf("restart: %v %v", err, ran)
	}
}
