//go:build !windows

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/update"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// buildBridge builds meshbridge at version, trusting testKey for updates.
func buildBridge(t *testing.T, out, version string, testKey ed25519.PublicKey) {
	t.Helper()
	ld := "-X main.version=" + version + " -X github.com/sierragridteam/sierra-meshrelay/internal/update.extraKey=" +
		base64.StdEncoding.EncodeToString(testKey)
	build := exec.Command("go", "build", "-ldflags", ld, "-o", out, "./cmd/meshbridge")
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
}

// signedRelease serves <srv>/<version>/{asset, SHA256SUMS, SHA256SUMS.sig}.
func signedRelease(t *testing.T, key ed25519.PrivateKey, version string, binary []byte) *httptest.Server {
	t.Helper()
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "./meshbridge", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(binary)
	tw.Close()
	gz.Close()
	asset := update.AssetName()
	sum := sha256.Sum256(tgz.Bytes())
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	files := map[string][]byte{asset: tgz.Bytes(), update.SumsFile: sums, update.SigFile: []byte(update.Sign(key, version, sums))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/"+version+"/") {
			http.NotFound(w, r)
			return
		}
		b, ok := files[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// syncBuffer is a bytes.Buffer safe to read while a process writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestAutoUpdate runs the real binary the way the systemd unit does: the
// relay offers a signed release, the bridge downloads and verifies it and
// exits, and apply-update (ExecStartPre) swaps the binary in.
func TestAutoUpdate(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	work := t.TempDir()
	bin := filepath.Join(work, "meshbridge")
	buildBridge(t, bin, "v9.9.9", pub)
	newBin := filepath.Join(t.TempDir(), "meshbridge")
	buildBridge(t, newBin, "v9.9.10", pub)
	newBytes, _ := os.ReadFile(newBin)
	rel := signedRelease(t, priv, "v9.9.10", newBytes)

	lr := startRelay(t, func(c *relay.Config) { c.ReleaseBaseURL = rel.URL })
	tok := lr.createGateway("cabin")
	node := startNode(t, "Cabin Node")
	cfg := config.Default()
	cfg.Gateway, cfg.Relay, cfg.Token = "cabin", lr.wsBase(), tok
	cfg.Node.TCPHost, cfg.Node.TCPPort = "127.0.0.1", node.Addr().Port
	cfgPath := filepath.Join(work, "conf", "meshbridge.toml")
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	runService := func() (*exec.Cmd, *syncBuffer) {
		out := &syncBuffer{}
		cmd := exec.Command(bin, "run", "-service", "-control", "off", "-config", cfgPath)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd, out
	}
	cmd, out := runService()
	lr.waitGateway("cabin", "v9.9.9 online", func(d gatewayDetail) bool { return connected(d) })

	// Offer it to another gateway only: nothing happens.
	ac := adminClient(lr)
	ctx := context.Background()
	if offered, err := ac.SetBridgeUpdate(ctx, admin.BridgeUpdate{Version: "v9.9.10", Gateways: []string{"elsewhere"}}); err != nil || len(offered) != 0 {
		t.Fatalf("canary elsewhere: %v %v", offered, err)
	}
	// Now to this one: the bridge stages it and exits for the restart.
	if offered, err := ac.SetBridgeUpdate(ctx, admin.BridgeUpdate{Version: "v9.9.10", Gateways: []string{"cabin"}}); err != nil || len(offered) != 1 {
		t.Fatalf("offer: %v %v", offered, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("bridge exited with %v:\n%s", err, out)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("bridge didn't restart for the update:\n%s", out)
	}
	if v, _ := update.Staged(filepath.Dir(cfgPath)); v != "v9.9.10" {
		t.Fatalf("staged %q:\n%s", v, out)
	}

	// systemd's ExecStartPre.
	if b, err := exec.Command(bin, "apply-update", "-config", cfgPath).CombinedOutput(); err != nil || !strings.Contains(string(b), "installed v9.9.10") {
		t.Fatalf("apply-update: %v\n%s", err, b)
	}
	if b, _ := exec.Command(bin, "version").Output(); !strings.Contains(string(b), "v9.9.10") {
		t.Fatalf("binary is now: %s", b)
	}

	// The new version reconnects, and isn't offered the update again.
	_, out = runService()
	lr.waitGateway("cabin", "v9.9.10 online", func(d gatewayDetail) bool {
		return connected(d) && d.Agent != nil && d.Agent.Version == "v9.9.10"
	})
	time.Sleep(500 * time.Millisecond)
	if strings.Contains(out.String(), "downloading update") {
		t.Fatalf("offered the update again:\n%s", out)
	}
}

// TestAutoUpdateRefusesUnsignedRelease: a release signed by a key the bridge
// doesn't trust is never staged.
func TestAutoUpdateRefusesUnsignedRelease(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	work := t.TempDir()
	bin := filepath.Join(work, "meshbridge")
	buildBridge(t, bin, "v9.9.9", pub)
	rel := signedRelease(t, otherPriv, "v9.9.10", []byte("evil"))

	lr := startRelay(t, func(c *relay.Config) { c.ReleaseBaseURL = rel.URL })
	tok := lr.createGateway("cabin")
	node := startNode(t, "Cabin Node")
	cfg := config.Default()
	cfg.Gateway, cfg.Relay, cfg.Token = "cabin", lr.wsBase(), tok
	cfg.Node.TCPHost, cfg.Node.TCPPort = "127.0.0.1", node.Addr().Port
	cfgPath := filepath.Join(work, "conf", "meshbridge.toml")
	_ = config.Save(cfgPath, cfg)
	out := &syncBuffer{}
	cmd := exec.Command(bin, "run", "-service", "-control", "off", "-config", cfgPath)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	lr.waitGateway("cabin", "online", connected)
	if _, err := adminClient(lr).SetBridgeUpdate(context.Background(), admin.BridgeUpdate{Version: "v9.9.10"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), "update failed") {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal logged:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "signature") {
		t.Fatalf("refused for the wrong reason:\n%s", out.String())
	}
	if v, _ := update.Staged(filepath.Dir(cfgPath)); v != "" {
		t.Fatal("staged an untrusted release")
	}
}
