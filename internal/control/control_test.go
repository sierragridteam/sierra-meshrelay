package control_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/control"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

func TestParseUntil(t *testing.T) {
	now := time.Date(2026, 11, 5, 21, 30, 0, 0, time.Local)
	cases := map[string]time.Time{
		"until-resumed":        {},
		"":                     {},
		"1h":                   now.Add(time.Hour),
		"90m":                  now.Add(90 * time.Minute),
		"tomorrow":             time.Date(2026, 11, 6, 8, 0, 0, 0, time.Local),
		"2026-11-06T01:30:00Z": time.Date(2026, 11, 6, 1, 30, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := control.ParseUntil(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseUntil(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1h", "0s", "soon", "2020-01-01T00:00:00Z"} {
		if _, err := control.ParseUntil(bad, now); err == nil {
			t.Errorf("ParseUntil(%q) accepted", bad)
		}
	}
}

func TestDescribe(t *testing.T) {
	connected := meshproto.Node{State: meshproto.NodeConnected, Name: "N"}
	cases := []struct {
		st    bridge.Status
		level string
	}{
		{bridge.Status{RelayConnected: true, Node: connected}, control.Green},
		{bridge.Status{RelayConnected: true, Node: meshproto.Node{State: meshproto.NodeDisconnected}}, control.Amber},
		{bridge.Status{RelayConnected: true, Paused: true, Node: meshproto.Node{State: meshproto.NodePaused}}, control.Amber},
		{bridge.Status{RelayConnected: false, Node: connected}, control.Red},
		{bridge.Status{RelayConnected: false, RelayClass: bridge.ClassAuth, Node: connected}, control.Red},
		{bridge.Status{RelayConnected: true, Node: meshproto.Node{State: meshproto.NodeMismatch}}, control.Red},
	}
	resuming := control.Describe(bridge.Status{RelayConnected: true,
		Node: meshproto.Node{State: meshproto.NodeDisconnected, Error: bridge.NodeErrResuming}}, "gw", "v")
	if resuming.Level != control.Amber || resuming.Reason != "Resuming sharing…" {
		t.Errorf("just resumed: %s %q", resuming.Level, resuming.Reason)
	}
	for _, c := range cases {
		d := control.Describe(c.st, "gw", "v")
		if d.Level != c.level || d.Reason == "" {
			t.Errorf("%+v: level %s (%q), want %s", c.st, d.Level, d.Reason, c.level)
		}
	}
}

func TestTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", control.TokenFile)
	tok, err := control.LoadOrCreateToken(path)
	if err != nil || len(tok) < 40 {
		t.Fatalf("token %q, %v", tok, err)
	}
	// Windows has no file modes; there the config folder's ACL protects it
	// (svc.SecureConfigDir).
	fi, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("token file mode %v, %v", fi.Mode().Perm(), err)
	}
	again, _ := control.LoadOrCreateToken(path)
	if again != tok {
		t.Fatal("token changed on reload")
	}
}

func TestAPI(t *testing.T) {
	cfg := config.Default()
	cfg.Gateway, cfg.Token = "gw-test", "smb_x"
	b := bridge.New(bridge.Options{Config: cfg, Opener: nodelink.TCP{}})
	srv := &control.Server{Bridge: b, Token: "secret", Gateway: "gw-test", Version: "t"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, ln)
	addr := ln.Addr().String()

	bad := control.Client{Addr: addr, Token: "wrong"}
	if _, err := bad.Do(ctx, "GET", "/status", nil); err == nil {
		t.Fatal("wrong token accepted")
	}
	c := control.Client{Addr: addr, Token: "secret"}
	d, err := c.Do(ctx, "GET", "/status", nil)
	if err != nil || d.Gateway != "gw-test" || d.Paused {
		t.Fatalf("status %+v %v", d, err)
	}
	d, err = c.Do(ctx, "POST", "/pause", control.PauseRequest{Until: "1h"})
	if err != nil || !d.Paused || d.PauseUntil == nil || d.Level != control.Amber {
		t.Fatalf("after pause: %+v %v", d, err)
	}
	if _, err := c.Do(ctx, "POST", "/pause", control.PauseRequest{Until: "whenever"}); err == nil {
		t.Fatal("bad until accepted")
	}
	d, err = c.Do(ctx, "POST", "/resume", nil)
	if err != nil || d.Paused {
		t.Fatalf("after resume: %+v %v", d, err)
	}
}

func TestRefusesNonLoopback(t *testing.T) {
	srv := &control.Server{Token: "x"}
	if err := srv.ListenAndServe(context.Background(), "0.0.0.0:0"); err == nil {
		t.Fatal("listened on a non-loopback address")
	}
}
