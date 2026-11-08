//go:build !windows

package e2e

import (
	"context"
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/admin"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
)

// TestAdminRemoveGateway runs `meshbridge admin remove` against a gateway with
// a live bridge: it asks for -yes without a terminal, then removes the
// gateway, disconnects the bridge and leaves its token useless.
func TestAdminRemoveGateway(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	bin := filepath.Join(t.TempDir(), "meshbridge")
	buildBridge(t, bin, "v9.9.9", pub)

	lr := startRelay(t)
	tok := lr.createGateway("cabin")
	node := startNode(t, "Cabin Node")
	_, stop := startBridge(t, lr, "cabin", tok, node)
	defer stop()
	lr.waitGateway("cabin", "online", connected)

	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, append([]string{"admin"}, append(args, "-relay", lr.wsBase())...)...)
		cmd.Env = append(os.Environ(), "MESHBRIDGE_ADMIN_TOKEN="+lr.admin)
		cmd.Stdin = strings.NewReader("cabin\n") // not a terminal
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	if out, err := run("remove", "nowhere", "-yes"); err == nil || !strings.Contains(out, "no gateway named") {
		t.Fatalf("unknown gateway: %v\n%s", err, out)
	}
	if out, err := run("remove", "cabin"); err == nil || !strings.Contains(out, "pass -yes") {
		t.Fatalf("no terminal, no -yes: %v\n%s", err, out)
	}
	if code := lr.api("GET", "/gateways/cabin", nil, nil); code != 200 {
		t.Fatalf("removed without confirmation: HTTP %d", code)
	}
	if out, err := run("remove", "cabin", "-yes"); err != nil || !strings.Contains(out, "Removed cabin.") {
		t.Fatalf("remove: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code := lr.api("GET", "/gateways/cabin", nil, nil); code != 404 {
		t.Fatalf("still there: HTTP %d", code)
	}
	// The old token no longer authenticates.
	old := &admin.Client{APIBase: config.HTTPBase(lr.wsBase()), Token: tok}
	if _, err := old.MyRepeaters(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("old token: %v", err)
	}
}
