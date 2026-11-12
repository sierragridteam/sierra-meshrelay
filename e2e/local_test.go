package e2e

// Local end-to-end tests: the real relay, the real bridge, the connect shim and
// a TCP companion client, all in process. No network, no credentials, no
// fakes between the relay and the bridge; only the radio is simulated.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/connect"
	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/relayclient"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// localRelay is a real relay on a loopback port.
type localRelay struct {
	t      *testing.T
	base   string // http://127.0.0.1:port
	admin  string
	server *prefab.Server
	relay  *relay.Relay
}

func startRelay(t *testing.T, tune ...func(*relay.Config)) *localRelay {
	t.Helper()
	ctx := logging.With(context.Background(), logging.NewDevLogger())
	st := openStore(t, ctx)
	admin, _ := relay.MintToken(relay.ClientTokenPrefix)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := relay.Config{
		BootstrapAdmins: map[string]string{"root": relay.TokenHash(admin)},
		TickInterval:    20 * time.Millisecond,
		PublicBaseURL:   fmt.Sprintf("http://127.0.0.1:%d", port),
		Pairing:         relay.PairingConfig{BcryptCost: 4, PollInterval: time.Second},
	}
	for _, f := range tune {
		f(&cfg)
	}
	r := relay.New(ctx, cfg, st)
	server := prefab.New(prefab.WithContext(ctx), prefab.WithHost("127.0.0.1"), prefab.WithPort(port), prefab.WithPlugin(r))
	go func() { _ = server.Start() }()
	lr := &localRelay{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), admin: admin, server: server, relay: r}
	t.Cleanup(func() { _ = server.Shutdown(); st.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(lr.base + relay.PathPrefix + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return lr
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("relay never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (lr *localRelay) wsBase() string {
	return "ws" + strings.TrimPrefix(lr.base, "http") + relay.PathPrefix
}

func (lr *localRelay) api(method, path string, body, out any) int {
	lr.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, lr.base+relay.PathPrefix+"/api"+path, rd)
	req.Header.Set("Authorization", "Bearer "+lr.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		lr.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (lr *localRelay) createGateway(name string) string {
	lr.t.Helper()
	var out struct {
		BridgeToken string `json:"bridge_token"`
	}
	if code := lr.api("POST", "/gateways", map[string]any{"name": name}, &out); code != 200 {
		lr.t.Fatalf("create gateway: HTTP %d", code)
	}
	return out.BridgeToken
}

func (lr *localRelay) createClient(name, role string) string {
	lr.t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	if code := lr.api("POST", "/clients", map[string]any{"name": name, "role": role}, &out); code != 200 {
		lr.t.Fatalf("create client: HTTP %d", code)
	}
	return out.Token
}

func (lr *localRelay) gateway(name string) gatewayDetail {
	lr.t.Helper()
	var d gatewayDetail
	if code := lr.api("GET", "/gateways/"+name, nil, &d); code != 200 {
		lr.t.Fatalf("get gateway: HTTP %d", code)
	}
	return d
}

func (lr *localRelay) waitGateway(name, what string, cond func(gatewayDetail) bool) gatewayDetail {
	lr.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d := lr.gateway(name)
		if cond(d) {
			return d
		}
		if time.Now().After(deadline) {
			lr.t.Fatalf("timed out waiting for %s: %+v", what, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startBridge runs a real bridge for gateway against lr, linked to node.
func startBridge(t *testing.T, lr *localRelay, gateway, token string, node *fakenode.Node) (*bridge.Bridge, func()) {
	t.Helper()
	cfg := config.Default()
	cfg.Gateway, cfg.Relay, cfg.Token = gateway, lr.wsBase(), token
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	b := bridge.New(bridge.Options{
		Config: cfg, Opener: nodelink.TCP{Host: "127.0.0.1", Port: node.Addr().Port},
		Log:          slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version:      "e2e",
		NodeRetryMin: 20 * time.Millisecond, NodeRetryMax: 100 * time.Millisecond,
		Backoff: &bridge.Backoff{Min: 20 * time.Millisecond, Max: 200 * time.Millisecond,
			ResetAfter: time.Minute, AuthWait: time.Hour, UpgradeWait: time.Hour},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return b, stop
}

func startNode(t *testing.T, name string) *fakenode.Node {
	t.Helper()
	n, err := fakenode.Listen("127.0.0.1:0", fakenode.Options{Name: name, Model: "e2e board"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func connected(d gatewayDetail) bool {
	return d.Online && d.Node != nil && d.Node.State == meshproto.NodeConnected && d.PubkeyStatus == "pinned"
}

// TestLocalFullChain: TCP companion client → connect shim → relay → bridge → node.
func TestLocalFullChain(t *testing.T) {
	lr := startRelay(t)
	tok := lr.createGateway("cabin")
	op := lr.createClient("tester", "operator")
	node := startNode(t, "Cabin Node")
	startBridge(t, lr, "cabin", tok, node)

	d := lr.waitGateway("cabin", "bridge online with node pinned", connected)
	if d.PinnedPubkey != node.PubkeyHex() || d.Node.Name != "Cabin Node" || d.Node.Model != "e2e board" {
		t.Fatalf("relay sees %+v", d)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	shim := connect.New(connect.Options{
		Gateway: "cabin",
		Say:     func(f string, a ...any) { t.Logf("connect: "+f, a...) },
		Dial: func(ctx context.Context) (*relayclient.Conn, error) {
			return relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); shim.Serve(ctx, ln) }()
	defer func() { cancel(); <-served }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	r := companion.NewReader(c, companion.HeaderFromNode, meshproto.MaxFrameBytes)
	send := func(p []byte) {
		t.Helper()
		if _, err := c.Write(companion.Encode(companion.HeaderToNode, p)); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() []byte {
		t.Helper()
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	send(companion.AppStartFrame())
	if pk, name, err := companion.ParseSelfInfo(recv()); err != nil || pk != node.PubkeyHex() || name != "Cabin Node" {
		t.Fatalf("SELF_INFO: %s %q %v", pk, name, err)
	}
	for i := range 50 {
		p := append([]byte{0x60}, bytes.Repeat([]byte{byte(i)}, i*3)...) // 0x60: not a command the fake node answers
		send(p)
		if got := recv(); !bytes.Equal(got, append([]byte{fakenode.EchoPrefix}, p...)) {
			t.Fatalf("frame %d came back altered", i)
		}
	}
	// A burst well past the relay's per-connection burst: throttled, not dropped.
	const burst = 300
	go func() {
		for i := range burst {
			if node.Push([]byte{0x83, byte(i >> 8), byte(i)}) != nil {
				return
			}
		}
	}()
	for i := range burst {
		f := recv()
		if got := int(f[1])<<8 | int(f[2]); got != i {
			t.Fatalf("burst frame %d arrived as %d", i, got)
		}
	}
}

// TestLocalPubkeyPinning: a different radio behind the same gateway is blocked
// until an admin accepts its key.
func TestLocalPubkeyPinning(t *testing.T) {
	lr := startRelay(t)
	tok := lr.createGateway("cabin")
	op := lr.createClient("tester", "operator")
	first := startNode(t, "Original")
	_, stop := startBridge(t, lr, "cabin", tok, first)
	lr.waitGateway("cabin", "first node pinned", connected)
	stop()

	swapped := startNode(t, "Swapped")
	startBridge(t, lr, "cabin", tok, swapped)
	d := lr.waitGateway("cabin", "mismatch", func(d gatewayDetail) bool { return d.PubkeyStatus == "mismatch" })
	if d.PinnedPubkey != first.PubkeyHex() {
		t.Fatalf("pin moved: %+v", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	var re *relayclient.RefusedError
	if !errors.As(err, &re) || re.Code != meshproto.ErrPubkeyMismatch {
		t.Fatalf("lease on a swapped node: %v, want pubkey_mismatch", err)
	}
	if got := connect.Explain(err); got != "Gateway's node needs checking by a SIERRA admin." {
		t.Errorf("Explain = %q", got)
	}

	if code := lr.api("POST", "/gateways/cabin/accept-pubkey", nil, nil); code != 200 {
		t.Fatalf("accept-pubkey: HTTP %d", code)
	}
	c, err := relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	if err != nil {
		t.Fatalf("lease after accept: %v", err)
	}
	c.Close()
}

// TestLocalRelinkAndBusy: an admin relink reaches the node through the bridge;
// a second client is refused while the first holds the lease.
func TestLocalRelinkAndBusy(t *testing.T) {
	lr := startRelay(t)
	tok := lr.createGateway("cabin")
	op := lr.createClient("tester", "operator")
	node := startNode(t, "Cabin Node")
	b, _ := startBridge(t, lr, "cabin", tok, node)
	lr.waitGateway("cabin", "ready", connected)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitStatus(t, b, "attach", func(st bridge.Status) bool { return st.Attached })

	_, err = relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	var re *relayclient.RefusedError
	if !errors.As(err, &re) || re.Code != meshproto.ErrBusy || re.CloseCode != meshproto.CloseBusy {
		t.Fatalf("second client: %v, want busy/4029", err)
	}

	before := node.Accepted()
	if code := lr.api("POST", "/gateways/cabin/relink", nil, nil); code != 200 {
		t.Fatalf("relink: HTTP %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for node.Accepted() == before {
		if time.Now().After(deadline) {
			t.Fatal("bridge didn't reopen the node link")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLocalBridgeDropEndsLease: when the bridge goes away the lease holder is
// told, and the bridge's return makes the gateway usable again.
func TestLocalBridgeDropEndsLease(t *testing.T) {
	lr := startRelay(t)
	tok := lr.createGateway("cabin")
	op := lr.createClient("tester", "operator")
	node := startNode(t, "Cabin Node")
	_, stop := startBridge(t, lr, "cabin", tok, node)
	lr.waitGateway("cabin", "ready", connected)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	var le meshproto.LeaseEnding
	nextControl(t, c, meshproto.TypeLeaseEnding, &le)
	if le.Reason != meshproto.ReasonGatewayOffline || le.InS != 0 {
		t.Fatalf("lease_ending %+v", le)
	}
	if got := connect.LeaseEndedText(le.Reason); got != "The gateway went offline." {
		t.Errorf("LeaseEndedText = %q", got)
	}

	startBridge(t, lr, "cabin", tok, node)
	lr.waitGateway("cabin", "back online", connected)
	c2, err := relayclient.Dial(ctx, lr.wsBase(), "cabin", op, false)
	if err != nil {
		t.Fatalf("lease after the bridge returned: %v", err)
	}
	c2.Close()
}

func jsonDecode(resp *http.Response, v any) error { return json.NewDecoder(resp.Body).Decode(v) }
