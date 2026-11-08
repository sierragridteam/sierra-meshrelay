// Package e2e runs the bridge against the live SIERRA Mesh Relay.
//
// These tests are skipped unless MESHBRIDGE_LIVE=1. They read credentials from
// the environment (loaded by direnv from the parent workspace):
//
//	RELAY             relay API base, e.g. https://data.sierragridteam.org/mesh/relay/api
//	SIM_BRIDGE_NAME   gateway for the fake node (default bridge-dev-sim)
//	SIM_BRIDGE_TOKEN  that gateway's smb_ token
//	CLIENT_TOKEN      an operator smc_ token
//	ADMIN             optional admin smc_ token; admin-only checks skip without it
//
// They use a fake node only, on its own gateway, so they never pin a fake key
// onto the hardware gateway (bridge-dev). Never run two copies at once: two
// bridges with one token knock each other off (4009). Credentials are never
// logged, and these tests never try a bad credential (the relay locks an IP
// out after 10 auth failures).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/connect"
	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/relayclient"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// simNodeName seeds the fake node's pubkey. Keep it stable: the relay pinned
// this key for the sim gateway the first time it saw it.
const simNodeName = "meshbridge-sim"

type env struct {
	api, wsBase, gateway, bridgeToken, clientToken, admin string
}

func liveEnv(t *testing.T) env {
	t.Helper()
	if os.Getenv("MESHBRIDGE_LIVE") != "1" {
		t.Skip("set MESHBRIDGE_LIVE=1 (with the parent workspace environment loaded) to run live relay tests")
	}
	e := env{
		api:         strings.TrimRight(os.Getenv("RELAY"), "/"),
		gateway:     os.Getenv("SIM_BRIDGE_NAME"),
		bridgeToken: os.Getenv("SIM_BRIDGE_TOKEN"),
		clientToken: os.Getenv("CLIENT_TOKEN"),
		admin:       os.Getenv("ADMIN"),
	}
	if e.gateway == "" {
		e.gateway = "bridge-dev-sim"
	}
	if e.gateway == "bridge-dev" {
		t.Fatal("live tests use a fake node and must not run against bridge-dev (pubkey pinning)")
	}
	for name, v := range map[string]string{"RELAY": e.api, "SIM_BRIDGE_TOKEN": e.bridgeToken, "CLIENT_TOKEN": e.clientToken} {
		if v == "" {
			t.Fatalf("%s is not set", name)
		}
	}
	base := strings.TrimSuffix(e.api, "/api")
	e.wsBase = "wss://" + strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if strings.HasPrefix(base, "http://") {
		e.wsBase = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return e
}

type gatewayDetail struct {
	Online       bool            `json:"online"`
	Node         *meshproto.Node `json:"node"`
	Link         *meshproto.Link `json:"link"`
	Agent        *meshproto.Agent
	PubkeyStatus string `json:"pubkey_status"`
	PinnedPubkey string `json:"pinned_pubkey"`
	Lease        *struct {
		ID string `json:"id"`
	} `json:"lease"`
}

func (e env) call(t *testing.T, method, path, token string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, e.api+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func (e env) detail(t *testing.T) gatewayDetail {
	t.Helper()
	var d gatewayDetail
	if code := e.call(t, "GET", "/gateways/"+e.gateway, e.clientToken, &d); code != 200 {
		t.Fatalf("GET gateway: HTTP %d", code)
	}
	return d
}

func (e env) waitGateway(t *testing.T, what string, cond func(gatewayDetail) bool) gatewayDetail {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		d := e.detail(t)
		if cond(d) {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, d)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// runBridge starts a fake node and a real bridge pointed at the live relay.
func runBridge(t *testing.T, e env) (*fakenode.Node, *bridge.Bridge) {
	t.Helper()
	node, err := fakenode.Listen("127.0.0.1:0", fakenode.Options{Name: simNodeName, Model: "meshbridge fake node"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Close() })
	cfg := config.Default()
	cfg.Gateway, cfg.Relay, cfg.Token = e.gateway, e.wsBase, e.bridgeToken
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	b := bridge.New(bridge.Options{
		Config:  cfg,
		Opener:  nodelink.TCP{Host: "127.0.0.1", Port: node.Addr().Port},
		Log:     slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version: "e2e",
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return node, b
}

func TestLiveBridge(t *testing.T) {
	e := liveEnv(t)
	node, b := runBridge(t, e)

	d := e.waitGateway(t, "gateway online with node connected", func(d gatewayDetail) bool {
		return d.Online && d.Node != nil && d.Node.State == meshproto.NodeConnected
	})
	if d.Node.Pubkey != node.PubkeyHex() || d.Node.Name != simNodeName {
		t.Fatalf("relay sees node %+v", d.Node)
	}
	if d.PinnedPubkey != node.PubkeyHex() {
		t.Fatalf("pubkey_status %q, pinned %q: the sim gateway is pinned to another key (an admin must accept-pubkey)",
			d.PubkeyStatus, d.PinnedPubkey)
	}
	if d.Link == nil || d.Link.Kind != "tcp" {
		t.Errorf("link = %+v", d.Link)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := relayclient.Dial(ctx, e.wsBase, e.gateway, e.clientToken, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitStatus(t, b, "attach", func(st bridge.Status) bool { return st.Attached })

	t.Run("APP_START returns SELF_INFO", func(t *testing.T) {
		if err := c.SendFrame(ctx, companion.AppStartFrame()); err != nil {
			t.Fatal(err)
		}
		f := nextFrame(t, c)
		pk, name, err := companion.ParseSelfInfo(f)
		if err != nil || pk != node.PubkeyHex() || name != simNodeName {
			t.Fatalf("SELF_INFO: %s %q %v", pk, name, err)
		}
	})

	t.Run("frames round trip unchanged", func(t *testing.T) {
		for i := range 20 {
			f := make([]byte, 1+i*9%176)
			f[0] = 0x40 + byte(i)
			if err := c.SendFrame(ctx, f); err != nil {
				t.Fatal(err)
			}
			if got := nextFrame(t, c); !bytes.Equal(got, append([]byte{fakenode.EchoPrefix}, f...)) {
				t.Fatalf("frame %d came back altered (%d bytes)", i, len(got))
			}
		}
	})

	t.Run("burst from node arrives in order", func(t *testing.T) {
		const n = 300 // well over the relay's burst of 100: exercises its backpressure
		go func() {
			for i := range n {
				if node.Push([]byte{0x83, byte(i >> 8), byte(i)}) != nil {
					return
				}
			}
		}()
		for i := range n {
			f := nextFrame(t, c)
			if got := int(f[1])<<8 | int(f[2]); got != i {
				t.Fatalf("frame %d arrived as %d", i, got)
			}
		}
	})

	t.Run("renew", func(t *testing.T) {
		if err := c.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		var l meshproto.Lease
		nextControl(t, c, meshproto.TypeLease, &l)
		if l.Lease != c.Lease.Lease {
			t.Fatalf("renewed %q, want %q", l.Lease, c.Lease.Lease)
		}
	})

	t.Run("relink (admin)", func(t *testing.T) {
		if e.admin == "" {
			t.Skip("ADMIN not set")
		}
		before := node.Accepted()
		if code := e.call(t, "POST", "/gateways/"+e.gateway+"/relink", e.admin, nil); code != 200 {
			t.Fatalf("relink: HTTP %d", code)
		}
		deadline := time.Now().Add(10 * time.Second)
		for node.Accepted() == before {
			if time.Now().After(deadline) {
				t.Fatal("bridge didn't reopen the node link")
			}
			time.Sleep(50 * time.Millisecond)
		}
		waitStatus(t, b, "node reconnected", func(st bridge.Status) bool { return st.Node.State == meshproto.NodeConnected })
	})

	c.Close()
	waitStatus(t, b, "detach", func(st bridge.Status) bool { return !st.Attached })
}

func TestLiveBusy(t *testing.T) {
	e := liveEnv(t)
	_, b := runBridge(t, e)
	e.waitGateway(t, "gateway ready", func(d gatewayDetail) bool {
		return d.Online && d.Node != nil && d.Node.State == meshproto.NodeConnected && d.Lease == nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := relayclient.Dial(ctx, e.wsBase, e.gateway, e.clientToken, false)
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, b, "attach", func(st bridge.Status) bool { return st.Attached })
	_, err = relayclient.Dial(ctx, e.wsBase, e.gateway, e.clientToken, false)
	var re *relayclient.RefusedError
	if !asRefused(err, &re) || re.Code != meshproto.ErrBusy || re.CloseCode != meshproto.CloseBusy {
		t.Fatalf("second client: %v, want busy/4029", err)
	}
	c.Close()
}

func asRefused(err error, out **relayclient.RefusedError) bool {
	re, ok := err.(*relayclient.RefusedError)
	*out = re
	return ok
}

func waitStatus(t *testing.T, b *bridge.Bridge, what string, cond func(bridge.Status) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond(b.Status()) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func nextFrame(t *testing.T, c *relayclient.Conn) []byte {
	t.Helper()
	select {
	case f, ok := <-c.Frames:
		if !ok {
			t.Fatalf("session ended: %v", c.Err())
		}
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no frame within 10s")
	}
	return nil
}

func nextControl(t *testing.T, c *relayclient.Conn, typ string, out any) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-c.Control:
			if !ok {
				t.Fatalf("session ended: %v", c.Err())
			}
			if meshproto.TypeOf(m) == typ {
				if err := json.Unmarshal(m, out); err != nil {
					t.Fatal(err)
				}
				return
			}
		case <-deadline:
			t.Fatalf("no %s within 10s", typ)
		}
	}
}

// testWriter sends bridge logs to t.Log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// TestLiveConnect runs the whole chain: fake node → bridge → relay → connect
// shim → a TCP companion client, the path meshcore-cli takes.
func TestLiveConnect(t *testing.T) {
	e := liveEnv(t)
	node, _ := runBridge(t, e)
	e.waitGateway(t, "gateway ready", func(d gatewayDetail) bool {
		return d.Online && d.Node != nil && d.Node.State == meshproto.NodeConnected && d.Lease == nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	shim := connect.New(connect.Options{
		Gateway: e.gateway,
		Say:     func(f string, a ...any) { t.Logf("connect: "+f, a...) },
		Dial: func(ctx context.Context) (*relayclient.Conn, error) {
			return relayclient.Dial(ctx, e.wsBase, e.gateway, e.clientToken, false)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); shim.Serve(ctx, ln) }()
	defer func() { cancel(); <-served }()

	for session := range 2 { // a second session proves the lease was released
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		r := companion.NewReader(c, companion.HeaderFromNode, meshproto.MaxFrameBytes)
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := c.Write(companion.Encode(companion.HeaderToNode, companion.AppStartFrame())); err != nil {
			t.Fatal(err)
		}
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("session %d: %v", session, err)
		}
		if pk, _, err := companion.ParseSelfInfo(f); err != nil || pk != node.PubkeyHex() {
			t.Fatalf("session %d: SELF_INFO %s %v", session, pk, err)
		}
		c.Close()
	}
}
