//go:build windows

package e2e

// Runs scripts/install.ps1 end to end on Windows: a local "GitHub releases"
// server serves a freshly built meshbridge.exe zip and SHA256SUMS, and
// pairing goes to a real relay in process. The script needs an administrator.
//
// TestWindowsService also installs the real Windows service, which runs from
// %ProgramFiles%; it runs only with MESHBRIDGE_WINDOWS_SERVICE_TEST=1 (CI sets
// it on a throwaway runner), so `go test` on a workstation never touches the
// machine's services.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	msvc "github.com/sierragridteam/sierra-meshrelay/internal/svc"
	"github.com/sierragridteam/sierra-meshrelay/internal/update"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

func requireAdmin(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("install.ps1 needs an administrator")
	}
}

// fakeWindowsRelease builds meshbridge.exe and lays out a release directory
// serving <prefix>/latest/download/<asset>, plus install.ps1 at the root.
func fakeWindowsRelease(t *testing.T) (dir, asset string) {
	t.Helper()
	asset = "meshbridge_windows_" + runtime.GOARCH + ".zip"
	stage := t.TempDir()
	exe := filepath.Join(stage, "meshbridge.exe")
	winBuild(t, exe, "v9.9.9")
	dir = t.TempDir()
	d := filepath.Join(dir, "latest", "download")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	writeZip(t, d, asset, exe, "v9.9.9")
	// An update for the service test: download/v9.9.10/, signed.
	exe2 := filepath.Join(stage, "v9.9.10", "meshbridge.exe")
	winBuild(t, exe2, "v9.9.10")
	writeZip(t, filepath.Join(dir, "download", "v9.9.10"), asset, exe2, "v9.9.10")

	script, _ := os.ReadFile("../scripts/install.ps1")
	if err := os.WriteFile(filepath.Join(dir, "install.ps1"), script, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, asset
}

// winTestKey signs test releases; the test binaries trust it.
var winTestKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func winBuild(t *testing.T, out, version string) {
	t.Helper()
	pub := base64.StdEncoding.EncodeToString(winTestKey.Public().(ed25519.PublicKey))
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+version+
		" -X github.com/sierragridteam/sierra-meshrelay/internal/update.extraKey="+pub, "-o", out, "./cmd/meshbridge")
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
}

// writeZip writes a release directory: the zip, SHA256SUMS and its signature.
func writeZip(t *testing.T, d, asset, exe, version string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	zf, err := os.Create(filepath.Join(d, asset))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, _ := zw.Create("meshbridge.exe")
	data, _ := os.ReadFile(exe)
	_, _ = w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zf.Close()
	archive, _ := os.ReadFile(filepath.Join(d, asset))
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	if err := os.WriteFile(filepath.Join(d, "SHA256SUMS"), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SHA256SUMS.sig"), []byte(update.Sign(winTestKey, version, sums)), 0o644); err != nil {
		t.Fatal(err)
	}
}

type winEnv struct {
	t       *testing.T
	baseURL string
	work    string
}

func newWinEnv(t *testing.T) (*winEnv, string) {
	release, _ := fakeWindowsRelease(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(release)))
	t.Cleanup(srv.Close)
	return &winEnv{t: t, baseURL: srv.URL, work: t.TempDir()}, filepath.Join(release, "latest", "download", "SHA256SUMS")
}

func (e *winEnv) config() string     { return filepath.Join(e.work, "config", "meshbridge.toml") }
func (e *winEnv) installDir() string { return filepath.Join(e.work, "bin") }

// powershell runs a script with Windows PowerShell 5.1, the one every
// Windows machine has. Stdin is empty, like a non-interactive session.
func powershell(t *testing.T, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Stdin = strings.NewReader("")
	// Started from PowerShell 7 (as CI's steps are), PSModulePath points at
	// 7's modules, which Windows PowerShell can't load. A user's session
	// doesn't have that.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// run invokes the script the way the documented one-liner with options does:
// & ([scriptblock]::Create((irm URL))) -Option value …
func (e *winEnv) run(args ...string) (string, error) {
	e.t.Helper()
	all := append([]string{"-BaseUrl", psQuote(e.baseURL), "-Config", psQuote(e.config()), "-NoPath"}, args...)
	return powershell(e.t, "& ([scriptblock]::Create((irm "+psQuote(e.baseURL+"/install.ps1")+"))) "+strings.Join(all, " "))
}

func TestWindowsInstallScript(t *testing.T) {
	requireAdmin(t)
	env, sums := newWinEnv(t)
	lr := startRelay(t)
	node := startNode(t, "Win Node")
	phrase, code := lr.invite(map[string]any{"gateway": "win-test"})
	if code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}

	out, err := env.run("-NoService", "-InstallDir", psQuote(env.installDir()), "-Relay", psQuote(lr.wsBase()),
		"-Passphrase", psQuote(strings.ReplaceAll(phrase, "-", " ")), "-NodeHost", "127.0.0.1", "-Port", strconv.Itoa(node.Addr().Port))
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{"Installed meshbridge v9.9.9", "Found: Win Node", `Connected to SIERRA as gateway "win-test"`,
		"Installing the service", "Skipped the service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	cfg, err := os.ReadFile(env.config())
	if err != nil || !strings.Contains(string(cfg), `token = "smb_`) || !strings.Contains(string(cfg), node.PubkeyHex()) {
		t.Fatalf("config: %v\n%s", err, cfg)
	}
	assertLockedDown(t, filepath.Dir(env.config()), false)

	// Running it again is an upgrade: no second pairing.
	if out, err := env.run("-NoService", "-InstallDir", psQuote(env.installDir())); err != nil || !strings.Contains(out, "already paired") {
		t.Fatalf("re-run: %v\n%s", err, out)
	}

	// A bad checksum stops before anything is replaced.
	bin := filepath.Join(env.installDir(), "meshbridge.exe")
	_ = os.WriteFile(bin, []byte("previous binary"), 0o755)
	asset := "meshbridge_windows_" + runtime.GOARCH + ".zip"
	_ = os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  "+asset+"\n"), 0o644)
	out, err = env.run("-NoService", "-InstallDir", psQuote(env.installDir()))
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("tampered download: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(bin); string(got) != "previous binary" {
		t.Fatal("binary replaced despite a checksum mismatch")
	}
}

// TestWindowsInstallScriptPiped runs the plain one-liner, `irm URL | iex`,
// which can't take options. It stops at the CPU check (forced to fail), which
// shows the script's param block and function run under iex, without
// downloading anything.
func TestWindowsInstallScriptPiped(t *testing.T) {
	requireAdmin(t)
	env, _ := newWinEnv(t)
	out, err := powershell(t, "$env:PROCESSOR_ARCHITEW6432 = 'bogus'; irm "+psQuote(env.baseURL+"/install.ps1")+" | iex")
	if err == nil || !strings.Contains(out, "unsupported CPU: bogus") {
		t.Fatalf("piped: %v\n%s", err, out)
	}
	if strings.Contains(out, "Downloading") {
		t.Fatalf("got past the CPU check:\n%s", out)
	}
}

// assertLockedDown checks the config folder's ACL: no access for ordinary
// users (it holds the bridge token).
func assertLockedDown(t *testing.T, dir string, withService bool) {
	t.Helper()
	out, err := exec.Command("icacls", filepath.Join(dir, "meshbridge.toml")).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	acl := string(out)
	for _, bad := range []string{`BUILTIN\Users`, "Everyone", `NT AUTHORITY\Authenticated Users`, "CREATOR OWNER"} {
		if strings.Contains(acl, bad) {
			t.Errorf("config is readable by %s:\n%s", bad, acl)
		}
	}
	for _, want := range []string{`NT AUTHORITY\SYSTEM`, `BUILTIN\Administrators`} {
		if !strings.Contains(acl, want) {
			t.Errorf("config ACL lacks %s:\n%s", want, acl)
		}
	}
	if withService != strings.Contains(acl, `NT SERVICE\meshbridge`) {
		t.Errorf("service account in ACL = %v, want %v:\n%s", !withService, withService, acl)
	}
}

func TestWindowsService(t *testing.T) {
	requireAdmin(t)
	if os.Getenv("MESHBRIDGE_WINDOWS_SERVICE_TEST") != "1" {
		t.Skip("installs a real Windows service; set MESHBRIDGE_WINDOWS_SERVICE_TEST=1 on a throwaway machine")
	}
	env, _ := newWinEnv(t)
	lr := startRelay(t, func(c *relay.Config) { c.ReleaseBaseURL = env.baseURL + "/download" })
	node := startNode(t, "Service Node")
	phrase, _ := lr.invite(map[string]any{"gateway": "win-svc"})
	installed := filepath.Join(os.Getenv("ProgramFiles"), "SIERRA", "meshbridge", "meshbridge.exe")
	t.Cleanup(func() { _ = exec.Command(installed, "uninstall").Run() })

	t.Cleanup(func() {
		if t.Failed() {
			logData, err := os.ReadFile(filepath.Join(filepath.Dir(env.config()), "meshbridge.log"))
			t.Logf("service log (%v):\n%s", err, logData)
			ev, _ := powershell(t, "Get-WinEvent -LogName System -MaxEvents 15 | Where-Object ProviderName -eq 'Service Control Manager' | Format-List TimeCreated,Id,Message")
			t.Logf("service manager events:\n%s", ev)
			acl, _ := exec.Command("icacls", filepath.Dir(env.config())).CombinedOutput()
			files, _ := exec.Command("icacls", filepath.Join(filepath.Dir(env.config()), "*")).CombinedOutput()
			t.Logf("config folder ACL:\n%s\n%s", acl, files)
		}
	})

	out, err := env.run("-Relay", psQuote(lr.wsBase()), "-Passphrase", psQuote(strings.ReplaceAll(phrase, "-", " ")),
		"-NodeHost", "127.0.0.1", "-Port", strconv.Itoa(node.Addr().Port))
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Installed and started the meshbridge service") {
		t.Fatalf("output:\n%s", out)
	}
	waitConnected(t, lr, "win-svc", true, 60*time.Second)
	assertLockedDown(t, filepath.Dir(env.config()), true)

	qc, _ := exec.Command("sc.exe", "qc", "meshbridge").CombinedOutput()
	for _, want := range []string{`NT SERVICE\meshbridge`, "AUTO_START", installed} {
		if !strings.Contains(string(qc), want) {
			t.Errorf("sc qc lacks %q:\n%s", want, qc)
		}
	}
	status, err := exec.Command(installed, "status", "-config", env.config()).CombinedOutput()
	if err != nil || !strings.Contains(string(status), "win-svc") || !strings.Contains(string(status), "Service Node") {
		t.Fatalf("status: %v\n%s", err, status)
	}
	logData, err := os.ReadFile(filepath.Join(filepath.Dir(env.config()), "meshbridge.log"))
	if err != nil || !strings.Contains(string(logData), "meshbridge starting") {
		t.Fatalf("service log: %v\n%s", err, logData)
	}
	if !strings.Contains(string(logData), "keeping this computer awake") {
		t.Errorf("service didn't take the keep-awake request:\n%s", logData)
	}

	// A crash is restarted by the service's recovery settings.
	if out, err := exec.Command("taskkill", "/F", "/IM", "meshbridge.exe").CombinedOutput(); err != nil {
		t.Fatalf("taskkill: %v\n%s", err, out)
	}
	waitConnected(t, lr, "win-svc", false, 30*time.Second)
	waitConnected(t, lr, "win-svc", true, 60*time.Second)

	// Re-running the installer upgrades in place: stop, replace, start.
	if out, err := env.run(); err != nil || !strings.Contains(out, "already paired") || !strings.Contains(out, "Installed and started") {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	waitConnected(t, lr, "win-svc", true, 60*time.Second)

	// The update task runs on battery power too (schtasks' defaults don't).
	assertUpdateTaskRunsOnBattery(t)

	// A bridge installed with the old task (schtasks' defaults) has it
	// replaced by apply-update, run by that task itself.
	marker := filepath.Join(filepath.Dir(env.config()), msvc.UpdateTaskMarker)
	if out, err := exec.Command("schtasks", "/Create", "/TN", msvc.UpdateTask,
		"/TR", `"`+installed+`" apply-update -config "`+env.config()+`"`,
		"/SC", "MINUTE", "/MO", "5", "/RU", "SYSTEM", "/RL", "HIGHEST", "/F").CombinedOutput(); err != nil {
		t.Fatalf("old-style task: %v\n%s", err, out)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if ps, _ := powershell(t, updateTaskBatteryPS); !strings.Contains(ps, "True") {
		t.Fatalf("the old-style task should skip on battery: %s", ps)
	}
	if out, err := exec.Command("schtasks", "/Run", "/TN", msvc.UpdateTask).CombinedOutput(); err != nil {
		t.Fatalf("schtasks /Run: %v\n%s", err, out)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		ps, _ := powershell(t, updateTaskBatteryPS)
		if strings.TrimSpace(ps) == "False" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("apply-update didn't refresh the task: %s", ps)
		}
		time.Sleep(time.Second)
	}
	assertUpdateTaskRunsOnBattery(t)
	if b, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(b)) != msvc.UpdateTaskRevision {
		t.Fatalf("marker: %q %v", b, err)
	}

	// Auto-update: the relay offers v9.9.10; the service downloads and
	// verifies it and marks it ready; the update task (run now, rather than
	// waiting up to 5 minutes) stops the service, swaps the binary, starts it.
	if code := lr.api("PUT", "/bridge-update", map[string]any{"version": "v9.9.10"}, nil); code != 200 {
		t.Fatalf("offer update: HTTP %d", code)
	}
	ready := filepath.Join(filepath.Dir(env.config()), "update", "ready")
	deadline = time.Now().Add(90 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the service never staged the update")
		}
		time.Sleep(250 * time.Millisecond)
	}
	if out, err := exec.Command("schtasks", "/Run", "/TN", msvc.UpdateTask).CombinedOutput(); err != nil {
		t.Fatalf("schtasks /Run: %v\n%s", err, out)
	}
	deadline = time.Now().Add(120 * time.Second)
	for {
		var d gatewayDetail
		code := lr.api("GET", "/gateways/win-svc", nil, &d)
		if code == 200 && connected(d) && d.Agent != nil && d.Agent.Version == "v9.9.10" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the update wasn't installed: %+v", d)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if out, _ := exec.Command(installed, "version").Output(); !strings.Contains(string(out), "v9.9.10") {
		t.Fatalf("installed binary: %s", out)
	}
	// The new version tells the relay how the install went: the task's own
	// output only reaches the log file on this computer.
	ac := adminClient(lr)
	deadline = time.Now().Add(30 * time.Second)
	for {
		logs, _, err := ac.Logs(context.Background(), "win-svc", 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, l := range logs {
			if l.Msg == "update installed" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay never heard about the install: %+v", logs)
		}
		time.Sleep(500 * time.Millisecond)
	}

	if out, err := exec.Command(installed, "uninstall").CombinedOutput(); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	waitConnected(t, lr, "win-svc", false, 30*time.Second)
	if out, err := exec.Command("schtasks", "/Query", "/TN", msvc.UpdateTask).CombinedOutput(); err == nil {
		t.Fatalf("uninstall left the update task:\n%s", out)
	}
}

const updateTaskBatteryPS = `$s = (Get-ScheduledTask -TaskPath '\SIERRA\' -TaskName 'meshbridge update').Settings; $s.DisallowStartIfOnBatteries`

func assertUpdateTaskRunsOnBattery(t *testing.T) {
	t.Helper()
	ps, err := powershell(t, `$s = (Get-ScheduledTask -TaskPath '\SIERRA\' -TaskName 'meshbridge update').Settings; "$($s.DisallowStartIfOnBatteries) $($s.StopIfGoingOnBatteries) $($s.ExecutionTimeLimit)"`)
	if err != nil || strings.TrimSpace(ps) != "False False PT10M" {
		t.Fatalf("update task settings (want \"False False PT10M\"): %q %v", ps, err)
	}
}

func waitConnected(t *testing.T, lr *localRelay, gateway string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		// Polls through transient errors: the relay's SQLite can report
		// "locked" for a few seconds when Windows is busy with the disk
		// (Defender, servicing) on a CI runner.
		var d gatewayDetail
		code := lr.api("GET", "/gateways/"+gateway, nil, &d)
		if code == 200 && connected(d) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway %s connected=%v after %s, want %v: %+v", gateway, !want, timeout, want, d)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
