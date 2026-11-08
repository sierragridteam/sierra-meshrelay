package bridge_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/relaytest"
	statefile "github.com/sierragridteam/sierra-meshrelay/internal/state"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

const token = "smb_test-token"

type harness struct {
	t      *testing.T
	relay  *relaytest.Server
	node   *fakenode.Node
	bridge *bridge.Bridge
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan struct{}
}

type setup struct {
	nodeOpts fakenode.Options
	expected string
	tune     func(*bridge.Options)
	noNode   bool
}

func start(t *testing.T, s setup) *harness {
	t.Helper()
	h := &harness{t: t, relay: relaytest.New(token), logs: &syncBuffer{}, done: make(chan struct{})}
	t.Cleanup(h.relay.Close)
	opener := nodelink.TCP{Host: "127.0.0.1", Port: 1} // nothing listens on port 1
	if !s.noNode {
		n, err := fakenode.Listen("127.0.0.1:0", s.nodeOpts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.Close() })
		h.node = n
		opener.Port = n.Addr().Port
	}
	cfg := config.Default()
	cfg.Gateway, cfg.Relay, cfg.Token = "test-gw", h.relay.URL(), token
	cfg.Node.ExpectedPubkey = s.expected
	opts := bridge.Options{
		Config: cfg, Opener: opener, Version: "test",
		Log:             slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		IdentifyTimeout: 300 * time.Millisecond,
		NodeRetryMin:    10 * time.Millisecond,
		NodeRetryMax:    50 * time.Millisecond,
		MismatchRetry:   50 * time.Millisecond,
		Backoff: &bridge.Backoff{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond,
			ResetAfter: time.Minute, AuthWait: time.Hour, UpgradeWait: time.Hour},
	}
	if s.tune != nil {
		s.tune(&opts)
	}
	h.bridge = bridge.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { defer close(h.done); h.bridge.Run(ctx) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("bridge didn't stop")
	}
	// The core rule: no payload bytes in logs. Every test payload contains
	// this marker.
	if strings.Contains(h.logs.String(), "SECRET") {
		h.t.Error("frame payload appeared in logs")
	}
}

func (h *harness) session() *relaytest.Session {
	h.t.Helper()
	s, err := h.relay.Next()
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) waitFor(what string, cond func(bridge.Status) bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(h.bridge.Status()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s (status %+v)", what, h.bridge.Status())
}

func (h *harness) attach(s *relaytest.Session) {
	h.t.Helper()
	if err := s.Attach("l_1"); err != nil {
		h.t.Fatal(err)
	}
	h.waitFor("attach", func(st bridge.Status) bool { return st.Attached })
}

func expectNode(t *testing.T, s *relaytest.Session, state string) meshproto.Node {
	t.Helper()
	n, err := s.ExpectNode(state)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func expectFrame(t *testing.T, s *relaytest.Session) []byte {
	t.Helper()
	f, err := s.ExpectFrame()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func expectClosed(t *testing.T, s *relaytest.Session) int {
	t.Helper()
	c, err := s.ExpectClosed()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHelloAndIdentity(t *testing.T) {
	h := start(t, setup{nodeOpts: fakenode.Options{Name: "JayNode", Model: "Heltec V4"}})
	s := h.session()
	if s.Hello.Protocol != 1 || s.Hello.Gateway != "test-gw" || s.Hello.Agent.Name != "meshbridge" ||
		s.Hello.Agent.Version != "test" || s.Hello.Agent.OS == "" || s.Hello.Link.Kind != "tcp" {
		t.Fatalf("hello = %+v", s.Hello)
	}
	n := expectNode(t, s, meshproto.NodeConnected)
	if n.Pubkey != h.node.PubkeyHex() || n.Name != "JayNode" || n.Model != "Heltec V4" || n.FW != "v1.18.0" {
		t.Fatalf("node = %+v", n)
	}
}

func TestFramesRoundTrip(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.attach(s)
	for i := range 20 {
		f := append([]byte{0x20, byte(i)}, "SECRET"...)
		if err := s.SendFrame(f); err != nil {
			t.Fatal(err)
		}
		got := expectFrame(t, s)
		if !bytes.Equal(got, append([]byte{fakenode.EchoPrefix}, f...)) {
			t.Fatalf("frame %d came back as % x", i, got)
		}
	}
	// The echo can arrive before the bridge counts the frame it sent.
	h.waitFor("20 frames counted", func(st bridge.Status) bool { return st.FramesToNode == 20 })
	if st := h.bridge.Status(); st.DroppedFromNode != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestNodeFramesDroppedWithoutLease(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	if err := h.node.Push([]byte("\x80SECRET")); err != nil {
		t.Fatal(err)
	}
	if err := s.NoFrame(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	h.waitFor("drop counted", func(st bridge.Status) bool { return st.DroppedFromNode == 1 })

	// After detach, frames are dropped again.
	h.attach(s)
	if err := s.Send(meshproto.Detach{Type: meshproto.TypeDetach, Lease: "l_1"}); err != nil {
		t.Fatal(err)
	}
	h.waitFor("detach", func(st bridge.Status) bool { return !st.Attached })
	_ = h.node.Push([]byte("\x80SECRET"))
	if err := s.NoFrame(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestRelink(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	if err := s.Send(meshproto.Relink{Type: meshproto.TypeRelink}); err != nil {
		t.Fatal(err)
	}
	// The relay may see only the reconnected state: a quick reopen can
	// supersede the disconnected one before it's sent.
	deadline := time.Now().Add(5 * time.Second)
	for h.node.Accepted() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.node.Accepted(); got != 2 {
		t.Fatalf("node accepted %d connections, want 2", got)
	}
	h.waitFor("node reconnected", func(st bridge.Status) bool { return st.Node.State == meshproto.NodeConnected })
}

func TestNodeDropRelayStays(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.node.DropConn() // node reboot
	n := expectNode(t, s, meshproto.NodeDisconnected)
	if n.Error == "" || n.Pubkey != "" {
		t.Fatalf("disconnected node = %+v", n)
	}
	expectNode(t, s, meshproto.NodeConnected)
	if err := h.relay.NoSession(100 * time.Millisecond); err != nil {
		t.Fatal("relay session was replaced:", err)
	}
}

func TestRelayUpWithoutNode(t *testing.T) {
	h := start(t, setup{noNode: true})
	s := h.session()
	if s.Hello.Node.State != meshproto.NodeDisconnected {
		t.Fatalf("hello node = %+v", s.Hello.Node)
	}
	h.waitFor("relay connected", func(st bridge.Status) bool { return st.RelayConnected })
}

func TestPubkeyMismatch(t *testing.T) {
	h := start(t, setup{expected: strings.Repeat("ab", 32)})
	s := h.session()
	n := expectNode(t, s, meshproto.NodeMismatch)
	if n.Pubkey != h.node.PubkeyHex() {
		t.Fatalf("mismatch node = %+v", n)
	}
	h.attach(s)
	_ = s.SendFrame([]byte("SECRET"))
	if err := s.NoFrame(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "isn't the node SIERRA expects") {
		t.Error("mismatch not logged")
	}
}

func TestExpectedPubkeyMatches(t *testing.T) {
	key := fakenode.KeyFor("expected")
	h := start(t, setup{nodeOpts: fakenode.Options{Pubkey: key}, expected: strings.ToLower(hex.EncodeToString(key))})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
}

func TestSilentNodeNeverConnects(t *testing.T) {
	h := start(t, setup{nodeOpts: fakenode.Options{Silent: true}})
	s := h.session()
	_ = s
	time.Sleep(500 * time.Millisecond)
	if st := h.bridge.Status(); st.Node.State != meshproto.NodeDisconnected {
		t.Fatalf("node state %q", st.Node.State)
	}
	if !strings.Contains(h.logs.String(), "identity check failed") {
		t.Error("identity failure not logged")
	}
}

func TestOversizeNodeFrameRelinks(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.attach(s)
	// A header announcing 600 bytes: the link is broken, not forwarded.
	_ = h.node.Push(make([]byte, 600))
	n := expectNode(t, s, meshproto.NodeDisconnected)
	if n.Error != "node link out of sync" {
		t.Fatalf("error = %q", n.Error)
	}
	expectNode(t, s, meshproto.NodeConnected)
	if err := s.NoFrame(100 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestReconnectAfterRelayClose(t *testing.T) {
	for _, code := range []int{meshproto.CloseNormal, meshproto.CloseGoingAway, meshproto.CloseMessageTooBig, meshproto.CloseReplaced} {
		h := start(t, setup{})
		s := h.session()
		expectNode(t, s, meshproto.NodeConnected)
		_ = s.Close(code, "test")
		s2 := h.session()
		// The new session's hello carries the live node state.
		if s2.Hello.Node.State != meshproto.NodeConnected {
			t.Fatalf("close %d: second hello node = %+v", code, s2.Hello.Node)
		}
		h.stop()
	}
}

func TestAuthCloseBacksOff(t *testing.T) {
	for _, code := range []int{meshproto.CloseBadToken, meshproto.CloseGatewayMismatch, meshproto.CloseUnsupported} {
		h := start(t, setup{})
		s := h.session()
		h.waitFor("relay session", func(st bridge.Status) bool { return st.RelayConnected })
		_ = s.Close(code, "test")
		if err := h.relay.NoSession(300 * time.Millisecond); err != nil {
			t.Fatalf("close %d: %v", code, err)
		}
		h.stop()
	}
}

func TestHTTPRejectNoRetryLoop(t *testing.T) {
	for _, status := range []int32{401, 404, 429} {
		h := start(t, setup{})
		h.relay.RejectStatus.Store(status)
		// The first dial may already be in flight; allow it, then no more.
		time.Sleep(300 * time.Millisecond)
		if d := h.relay.Dials(); d > 1 {
			t.Fatalf("HTTP %d: %d dials, want 1 (would trip the relay's auth lockout)", status, d)
		}
		h.stop()
	}
}

func TestBackpressureNoDrops(t *testing.T) {
	const n = 1000
	h := start(t, setup{tune: func(o *bridge.Options) { o.OutQueue = 8 }})
	h.relay.ReadDelay = 200 * time.Microsecond
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.attach(s)
	go func() {
		for i := range n {
			f := []byte{0x83, byte(i >> 8), byte(i), 'S', 'E', 'C', 'R', 'E', 'T'}
			if err := h.node.Push(f); err != nil {
				return
			}
		}
	}()
	for i := range n {
		f := expectFrame(t, s)
		if got := int(f[1])<<8 | int(f[2]); got != i {
			t.Fatalf("frame %d arrived as %d (reordered or dropped)", i, got)
		}
	}
	if st := h.bridge.Status(); st.DroppedFromNode != 0 {
		t.Fatalf("dropped %d frames", st.DroppedFromNode)
	}
}

func TestStats(t *testing.T) {
	h := start(t, setup{tune: func(o *bridge.Options) { o.StatsInterval = 50 * time.Millisecond }})
	s := h.session()
	var st meshproto.StatsMsg
	if err := s.Expect(meshproto.TypeStats, &st); err != nil {
		t.Fatal(err)
	}
	if st.RelayReconnects != 0 || st.UptimeS < 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestShutdownClosesGoingAway(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.waitFor("relay session", func(st bridge.Status) bool { return st.RelayConnected })
	h.cancel()
	code := expectClosed(t, s)
	if code != meshproto.CloseGoingAway {
		t.Fatalf("close code %d, want 1001", code)
	}
}

func TestErrorMessageLogsCodeOnly(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	_ = s.Send(meshproto.Error{Type: meshproto.TypeError, Code: meshproto.ErrPubkeyMismatch, Message: "SECRET detail"})
	_ = s.Send(map[string]string{"type": "some_future_message"})
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(h.logs.String(), "code=pubkey_mismatch") {
		if time.Now().After(deadline) {
			t.Fatal("error code not logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPauseReleasesNodeAndResumes(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.attach(s)

	until := time.Now().Add(time.Hour).Truncate(time.Second)
	h.bridge.Pause(until)
	n := expectNode(t, s, meshproto.NodePaused)
	if n.Until != until.UTC().Format(time.RFC3339) || n.Pubkey != "" {
		t.Fatalf("paused node = %+v", n)
	}
	// The node's one connection is free for the host's own app.
	deadline := time.Now().Add(2 * time.Second)
	for h.node.Push([]byte{0x83}) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the bridge kept its node connection while paused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.NoFrame(100 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if st := h.bridge.Status(); !st.Paused || !st.PauseUntil.Equal(until) {
		t.Fatalf("status %+v", st)
	}

	accepted := h.node.Accepted()
	h.bridge.Resume()
	expectNode(t, s, meshproto.NodeConnected)
	if h.node.Accepted() != accepted+1 {
		t.Fatalf("node accepted %d, want %d", h.node.Accepted(), accepted+1)
	}
}

func TestTimedPauseRunsOut(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.bridge.Pause(time.Now().Add(300 * time.Millisecond))
	expectNode(t, s, meshproto.NodePaused)
	expectNode(t, s, meshproto.NodeConnected)
	if p, _ := h.bridge.Paused(); p {
		t.Fatal("still paused after the pause ran out")
	}
}

func TestPauseUntilResumedSurvivesRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	h := start(t, setup{tune: func(o *bridge.Options) { o.State = statefile.New(state) }})
	s := h.session()
	expectNode(t, s, meshproto.NodeConnected)
	h.bridge.Pause(time.Time{})
	n := expectNode(t, s, meshproto.NodePaused)
	if n.Until != "" {
		t.Fatalf("until-resumed pause reported until %q", n.Until)
	}
	h.stop()

	// A new bridge with the same state file starts paused and says so in hello.
	h2 := start(t, setup{tune: func(o *bridge.Options) { o.State = statefile.New(state) }})
	s2 := h2.session()
	if s2.Hello.Node.State != meshproto.NodePaused {
		t.Fatalf("hello after restart: %+v", s2.Hello.Node)
	}
	if h2.node.Accepted() != 0 {
		t.Fatal("a bridge paused until resumed opened the node link")
	}
	h2.bridge.Resume()
	expectNode(t, s2, meshproto.NodeConnected)
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("state file still there after resume: %v", err)
	}
}

func TestTimedPauseDoesNotSurviveRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	h := start(t, setup{tune: func(o *bridge.Options) { o.State = statefile.New(state) }})
	h.bridge.Pause(time.Now().Add(time.Hour))
	h.stop()
	h2 := start(t, setup{tune: func(o *bridge.Options) { o.State = statefile.New(state) }})
	s2 := h2.session()
	expectNode(t, s2, meshproto.NodeConnected)
}

// A node outage is logged once, not on every retry: retries alternate
// reasons (a timeout, then "no node found"), and logging each filled the
// log within hours.
type flakyOpener struct{ n atomic.Int64 }

func (o *flakyOpener) Open(context.Context) (nodelink.Link, error) {
	if o.n.Add(1)%2 == 0 {
		return nil, nodelink.ErrNodeNotFound
	}
	return nil, &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}
}

func TestNodeOutageIsLoggedOnce(t *testing.T) {
	h := start(t, setup{noNode: true, tune: func(o *bridge.Options) { o.Opener = &flakyOpener{} }})
	time.Sleep(400 * time.Millisecond) // many retries at 10-50ms
	logs := h.logs.String()
	if n := strings.Count(logs, "node unavailable"); n != 1 {
		t.Fatalf("logged the outage %d times, want once:\n%s", n, logs)
	}
}
