package connect_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/connect"
	"github.com/sierragridteam/sierra-meshrelay/internal/relayclient"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// fakeRelay is a scripted client endpoint. script runs once per client
// session after the upgrade.
type fakeRelay struct {
	srv    *httptest.Server
	script func(ctx context.Context, c *websocket.Conn)
	mu     sync.Mutex
	dials  int
}

func newFakeRelay(t *testing.T, script func(ctx context.Context, c *websocket.Conn)) *fakeRelay {
	f := &fakeRelay{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.dials++
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer smc_test" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{meshproto.ClientSubprotocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		f.script(r.Context(), c)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) base() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func sendJSON(ctx context.Context, c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = c.Write(ctx, websocket.MessageText, b)
}

func lease(id string) meshproto.Lease {
	return meshproto.Lease{Type: meshproto.TypeLease, Lease: id, Gateway: "gw",
		ExpiresAt: time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
		Node:      &meshproto.Node{State: meshproto.NodeConnected, Name: "N"}}
}

// echoScript grants a lease and echoes frames with a 0x80 prefix.
func echoScript(ctx context.Context, c *websocket.Conn) {
	sendJSON(ctx, c, lease("l_1"))
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			_ = c.Write(ctx, websocket.MessageBinary, append([]byte{0x80}, data...))
		}
	}
}

type lines struct {
	mu sync.Mutex
	l  []string
}

func (l *lines) say(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.l = append(l.l, fmt.Sprintf(format, a...))
}

func (l *lines) waitFor(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		all := strings.Join(l.l, "\n")
		l.mu.Unlock()
		if strings.Contains(all, sub) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Fatalf("no line containing %q; got:\n%s", sub, strings.Join(l.l, "\n"))
}

func startShim(t *testing.T, f *fakeRelay, token string) (string, *lines) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	out := &lines{}
	shim := connect.New(connect.Options{
		Gateway: "gw", Say: out.say,
		Dial: func(ctx context.Context) (*relayclient.Conn, error) {
			return relayclient.Dial(ctx, f.base(), "gw", token, false)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); shim.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-served })
	return ln.Addr().String(), out
}

func dialLocal(t *testing.T, addr string) (net.Conn, *companion.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, companion.NewReader(c, companion.HeaderFromNode, meshproto.MaxFrameBytes)
}

func TestTranslatesFraming(t *testing.T) {
	f := newFakeRelay(t, echoScript)
	addr, out := startShim(t, f, "smc_test")
	c, r := dialLocal(t, addr)
	out.waitFor(t, "Connected to gw (N)")
	for i := range 10 {
		p := bytes.Repeat([]byte{byte(i)}, 1+i*17)
		if _, err := c.Write(companion.Encode(companion.HeaderToNode, p)); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		got, err := r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, append([]byte{0x80}, p...)) {
			t.Fatalf("frame %d: got %d bytes", i, len(got))
		}
	}
	c.Close()
	out.waitFor(t, "Released gw.")
}

func TestNoLeaseUntilAppConnects(t *testing.T) {
	f := newFakeRelay(t, echoScript)
	addr, _ := startShim(t, f, "smc_test")
	_ = addr
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dials != 0 {
		t.Fatalf("shim dialled the relay %d times with no app connected", f.dials)
	}
}

func TestSequentialSessionsQueue(t *testing.T) {
	f := newFakeRelay(t, echoScript)
	addr, out := startShim(t, f, "smc_test")
	c1, _ := dialLocal(t, addr)
	out.waitFor(t, "Connected to gw")
	// A second app arrives just as the first leaves: it waits its turn.
	c2, r2 := dialLocal(t, addr)
	c1.Close()
	if _, err := c2.Write(companion.Encode(companion.HeaderToNode, []byte{1})); err != nil {
		t.Fatal(err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got, err := r2.ReadFrame(); err != nil || !bytes.Equal(got, []byte{0x80, 1}) {
		t.Fatalf("second session: % x %v", got, err)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name   string
		script func(ctx context.Context, c *websocket.Conn)
		want   string
	}{
		{"busy", func(ctx context.Context, c *websocket.Conn) {
			sendJSON(ctx, c, meshproto.Error{Type: meshproto.TypeError, Code: meshproto.ErrBusy})
			c.Close(meshproto.CloseBusy, "busy")
		}, "Someone else is using this gateway."},
		{"offline", func(ctx context.Context, c *websocket.Conn) {
			sendJSON(ctx, c, meshproto.GatewayStatus{Type: meshproto.TypeGatewayStatus, Bridge: "offline"})
			sendJSON(ctx, c, meshproto.Error{Type: meshproto.TypeError, Code: meshproto.ErrGatewayOffline})
			c.Close(websocket.StatusNormalClosure, "")
		}, "Gateway is offline."},
		{"paused", func(ctx context.Context, c *websocket.Conn) {
			sendJSON(ctx, c, meshproto.GatewayStatus{Type: meshproto.TypeGatewayStatus, Bridge: "online",
				Node: &meshproto.Node{State: meshproto.NodePaused, Until: "2026-11-04T01:30:00Z"}})
			sendJSON(ctx, c, meshproto.Error{Type: meshproto.TypeError, Code: "node_paused"})
			c.Close(websocket.StatusNormalClosure, "")
		}, "The host has paused sharing until"},
		{"mismatch", func(ctx context.Context, c *websocket.Conn) {
			sendJSON(ctx, c, meshproto.Error{Type: meshproto.TypeError, Code: meshproto.ErrPubkeyMismatch, Message: "SECRET"})
			c.Close(websocket.StatusNormalClosure, "")
		}, "Gateway's node needs checking by a SIERRA admin."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRelay(t, tc.script)
			addr, out := startShim(t, f, "smc_test")
			c, _ := dialLocal(t, addr)
			out.waitFor(t, tc.want)
			// The local app is disconnected.
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Read(make([]byte, 1)); err == nil {
				t.Fatal("local connection still open")
			}
		})
	}
}

func TestBadToken(t *testing.T) {
	f := newFakeRelay(t, echoScript)
	addr, out := startShim(t, f, "smc_wrong")
	dialLocal(t, addr)
	out.waitFor(t, "The relay didn't accept your token.")
}

func TestAutoRenewAndEnding(t *testing.T) {
	renewed := make(chan struct{}, 1)
	f := newFakeRelay(t, func(ctx context.Context, c *websocket.Conn) {
		sendJSON(ctx, c, lease("l_1"))
		sendJSON(ctx, c, meshproto.LeaseEnding{Type: meshproto.TypeLeaseEnding, Lease: "l_1", InS: 60, Reason: "expiry"})
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText && meshproto.TypeOf(data) == meshproto.TypeRenew {
				renewed <- struct{}{}
				sendJSON(ctx, c, lease("l_1"))
				sendJSON(ctx, c, meshproto.LeaseEnding{Type: meshproto.TypeLeaseEnding, Lease: "l_1", InS: 0, Reason: "preempted"})
				c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	})
	addr, out := startShim(t, f, "smc_test")
	dialLocal(t, addr)
	select {
	case <-renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("shim didn't renew")
	}
	out.waitFor(t, "Lease renewed until")
	out.waitFor(t, "A SIERRA admin took over this gateway.")
	out.waitFor(t, "Released gw.")
}

func TestExplainUnknownCode(t *testing.T) {
	if got := connect.CodeText("node_mismatch", nil); !strings.Contains(got, "node_mismatch") {
		t.Fatalf("CodeText = %q", got)
	}
	if got := connect.LeaseEndedText("something_new"); got != "The lease ended." {
		t.Fatalf("LeaseEndedText = %q", got)
	}
}
