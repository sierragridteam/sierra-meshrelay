package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sierragridteam/sierra-meshrelay/nodepolicy"
)

// bufLogger is a prefab logging.Logger over zap, writing JSON to a buffer so
// tests can assert on what the relay logs.
type bufLogger struct{ z *zap.SugaredLogger }

func newBufLogger(w io.Writer) logging.Logger {
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(w), zapcore.InfoLevel)
	return &bufLogger{z: zap.New(core).Sugar()}
}

func (l *bufLogger) Debug(a ...interface{})             { l.z.Debug(a...) }
func (l *bufLogger) Debugw(m string, kv ...interface{}) { l.z.Debugw(m, kv...) }
func (l *bufLogger) Debugf(m string, a ...interface{})  { l.z.Debugf(m, a...) }
func (l *bufLogger) Info(a ...interface{})              { l.z.Info(a...) }
func (l *bufLogger) Infow(m string, kv ...interface{})  { l.z.Infow(m, kv...) }
func (l *bufLogger) Infof(m string, a ...interface{})   { l.z.Infof(m, a...) }
func (l *bufLogger) Warn(a ...interface{})              { l.z.Warn(a...) }
func (l *bufLogger) Warnw(m string, kv ...interface{})  { l.z.Warnw(m, kv...) }
func (l *bufLogger) Warnf(m string, a ...interface{})   { l.z.Warnf(m, a...) }
func (l *bufLogger) Error(a ...interface{})             { l.z.Error(a...) }
func (l *bufLogger) Errorw(m string, kv ...interface{}) { l.z.Errorw(m, kv...) }
func (l *bufLogger) Errorf(m string, a ...interface{})  { l.z.Errorf(m, a...) }
func (l *bufLogger) Panic(a ...interface{})             { l.z.Panic(a...) }
func (l *bufLogger) Panicw(m string, kv ...interface{}) { l.z.Panicw(m, kv...) }
func (l *bufLogger) Panicf(m string, a ...interface{})  { l.z.Panicf(m, a...) }
func (l *bufLogger) Fatal(a ...interface{})             { l.z.Fatal(a...) }
func (l *bufLogger) Fatalw(m string, kv ...interface{}) { l.z.Fatalw(m, kv...) }
func (l *bufLogger) Fatalf(m string, a ...interface{})  { l.z.Fatalf(m, a...) }
func (l *bufLogger) Named(n string) logging.Logger      { return &bufLogger{z: l.z.Named(n)} }
func (l *bufLogger) With(f string, v interface{}) logging.Logger {
	return &bufLogger{z: l.z.With(f, v)}
}

const (
	pubkeyA = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	pubkeyB = "b1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
)

// syncBuffer is a goroutine-safe log sink.
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

type harness struct {
	t      *testing.T
	relay  *Relay
	server *prefab.Server
	base   string // http://127.0.0.1:<port>
	store  *Store
	admin  string // bootstrap admin token
	logs   *syncBuffer
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	return newHarnessAt(t, cfg, filepath.Join(t.TempDir(), "relay.db"))
}

// newHarnessAt runs the relay on a real prefab server, as production does, so
// every test also covers prefab's middleware (gzip, security headers) in front
// of the websocket upgrade.
func newHarnessAt(t *testing.T, cfg Config, dbPath string) *harness {
	t.Helper()
	logs := &syncBuffer{}
	ctx := logging.With(context.Background(), newBufLogger(logs))
	st, err := OpenStore(ctx, dbPath, time.Second)
	require.NoError(t, err)
	admin, _ := MintToken(ClientTokenPrefix)
	cfg.BootstrapAdmins = map[string]string{"root": TokenHash(admin)}
	if cfg.TickInterval == 0 {
		cfg.TickInterval = 20 * time.Millisecond
	}
	r := New(ctx, cfg, st)
	port := freePort(t)
	server := prefab.New(prefab.WithContext(ctx), prefab.WithHost("127.0.0.1"), prefab.WithPort(port),
		prefab.WithPlugin(r))
	go func() { _ = server.Start() }()
	h := &harness{t: t, relay: r, server: server, store: st, admin: admin, logs: logs,
		base: fmt.Sprintf("http://127.0.0.1:%d", port)}
	eventually(t, 5*time.Second, func() bool {
		resp, err := http.Get(h.base + PathPrefix + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, "relay never came up")
	t.Cleanup(h.stop)
	return h
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func (h *harness) stop() {
	if h.server == nil {
		return
	}
	_ = h.server.Shutdown()
	h.store.Close()
	h.server = nil
}

func (h *harness) api(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+PathPrefix+"/api"+path, rd)
	require.NoError(h.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (h *harness) createGateway(name string) string {
	h.t.Helper()
	code, out := h.api("POST", "/gateways", h.admin, map[string]any{"name": name})
	require.Equal(h.t, http.StatusOK, code, out)
	return out["bridge_token"].(string)
}

func (h *harness) createClient(name string, role Role) string {
	h.t.Helper()
	code, out := h.api("POST", "/clients", h.admin, map[string]any{"name": name, "role": role})
	require.Equal(h.t, http.StatusOK, code, out)
	return out["token"].(string)
}

func (h *harness) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(h.base, "http") + PathPrefix + path
}

func (h *harness) dial(path, token, subprotocol string) (*websocket.Conn, *http.Response, error) {
	d := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	if subprotocol != "" {
		d.Subprotocols = []string{subprotocol}
	}
	// Browsers send Accept-Encoding on a websocket handshake, which engages
	// prefab's gzip wrapper; the upgrade must still hijack cleanly through it.
	hdr := http.Header{"Accept-Encoding": {"gzip"}}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	return d.Dial(h.wsURL(path), hdr)
}

// fakeBridge is a bridge with a fake node behind it: every binary frame it
// receives is answered with 0x80 + the frame.
type fakeBridge struct {
	t        *testing.T
	ws       *websocket.Conn
	controls chan map[string]any
	closed   chan error
	wmu      sync.Mutex // one writer at a time: the echo loop and send
}

// send pushes a frame as if the node sent it unprompted.
func (b *fakeBridge) send(frame []byte) {
	b.t.Helper()
	b.wmu.Lock()
	defer b.wmu.Unlock()
	require.NoError(b.t, b.ws.WriteMessage(websocket.BinaryMessage, frame))
}

func hello(gw, pubkey string) map[string]any {
	return map[string]any{
		"type": "hello", "protocol": 1, "gateway": gw,
		"agent": map[string]any{"name": "meshbridge", "version": "0.3.1", "os": "linux", "arch": "arm64"},
		"link":  map[string]any{"kind": "serial", "detail": "/dev/ttyACM0"},
		"node":  map[string]any{"state": "connected", "pubkey": pubkey, "name": "SIERRA-GW-" + gw, "fw": "1.18.0"},
	}
}

func (h *harness) bridge(gw, token, pubkey string) *fakeBridge {
	h.t.Helper()
	ws, _, err := h.dial("/"+gw+"/dial", token, BridgeSubprotocol)
	require.NoError(h.t, err)
	require.NoError(h.t, ws.WriteJSON(hello(gw, pubkey)))
	var welcome map[string]any
	require.NoError(h.t, ws.SetReadDeadline(time.Now().Add(2*time.Second)))
	require.NoError(h.t, ws.ReadJSON(&welcome))
	require.Equal(h.t, "welcome", welcome["type"])
	require.EqualValues(h.t, 25, welcome["ping_interval_s"])
	b := &fakeBridge{t: h.t, ws: ws, controls: make(chan map[string]any, 32), closed: make(chan error, 1)}
	go b.loop()
	h.t.Cleanup(func() { ws.Close() })
	return b
}

func (b *fakeBridge) loop() {
	_ = b.ws.SetReadDeadline(time.Time{})
	for {
		kind, data, err := b.ws.ReadMessage()
		if err != nil {
			b.closed <- err
			return
		}
		if kind == websocket.BinaryMessage {
			b.wmu.Lock()
			_ = b.ws.WriteMessage(websocket.BinaryMessage, append([]byte{0x80}, data...))
			b.wmu.Unlock()
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		b.controls <- m
	}
}

func (b *fakeBridge) expect(typ string) map[string]any {
	b.t.Helper()
	for {
		select {
		case m := <-b.controls:
			if m["type"] == typ {
				return m
			}
		case <-time.After(2 * time.Second):
			b.t.Fatalf("bridge: no %q message", typ)
			return nil
		}
	}
}

func (b *fakeBridge) expectClose(code int) {
	b.t.Helper()
	select {
	case err := <-b.closed:
		assertCloseCode(b.t, err, code)
	case <-time.After(2 * time.Second):
		b.t.Fatalf("bridge: not closed (want %d)", code)
	}
}

func assertCloseCode(t *testing.T, err error, code int) {
	t.Helper()
	var ce *websocket.CloseError
	require.True(t, errors.As(err, &ce), "want close %d, got %v", code, err)
	assert.Equal(t, code, ce.Code)
}

// client dials the client endpoint and returns the socket.
func (h *harness) client(gw, token, query string) *websocket.Conn {
	h.t.Helper()
	ws, resp, err := h.dial("/"+gw+query, token, ClientSubprotocol)
	if err != nil && resp != nil {
		h.t.Fatalf("client dial: HTTP %d", resp.StatusCode)
	}
	require.NoError(h.t, err)
	h.t.Cleanup(func() { ws.Close() })
	return ws
}

// read reads the next message: a control object, a binary frame, or the close.
func read(t *testing.T, ws *websocket.Conn) (map[string]any, []byte, error) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, data, err := ws.ReadMessage()
	if err != nil {
		return nil, nil, err
	}
	if kind == websocket.BinaryMessage {
		return nil, data, nil
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	return m, nil, nil
}

func expectControl(t *testing.T, ws *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for {
		m, _, err := read(t, ws)
		require.NoError(t, err, "waiting for %q", typ)
		if m != nil && m["type"] == typ {
			return m
		}
	}
}

func expectClosed(t *testing.T, ws *websocket.Conn, code int) {
	t.Helper()
	for {
		_, _, err := read(t, ws)
		if err != nil {
			assertCloseCode(t, err, code)
			return
		}
	}
}

func gatewayStatus(h *harness, name string) map[string]any {
	code, out := h.api("GET", "/gateways/"+name, h.admin, nil)
	require.Equal(h.t, http.StatusOK, code)
	return out
}

func eventually(t *testing.T, within time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// Acceptance 1: hello -> welcome, online in GET /gateways within 2s.
func TestBridgeConnectsAndShowsOnline(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("avery-ridge")
	h.bridge("avery-ridge", tok, pubkeyA)

	eventually(t, 2*time.Second, func() bool {
		_, out := h.api("GET", "/gateways", h.admin, nil)
		gws := out["gateways"].([]any)
		return len(gws) == 1 && gws[0].(map[string]any)["online"] == true
	}, "gateway never showed online")

	st := gatewayStatus(h, "avery-ridge")
	assert.Equal(t, "pinned", st["pubkey_status"], "first pubkey is pinned on sight")
	assert.Equal(t, pubkeyA, st["pinned_pubkey"])
	assert.Equal(t, "0.3.1", st["agent"].(map[string]any)["version"])
	assert.Equal(t, "serial", st["link"].(map[string]any)["kind"])
	assert.Equal(t, "connected", st["node"].(map[string]any)["state"])
}

// Acceptance 2: bad token -> 401; another gateway's token -> 401.
func TestBridgeAuth(t *testing.T) {
	h := newHarness(t, Config{HelloTimeout: 200 * time.Millisecond})
	tokA := h.createGateway("gw-a")
	h.createGateway("gw-b")

	_, resp, err := h.dial("/gw-a/dial", "smb_nope", BridgeSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	_, resp, err = h.dial("/gw-a/dial", "", BridgeSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	_, resp, err = h.dial("/gw-b/dial", tokA, BridgeSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a token for a different gateway")

	_, resp, err = h.dial("/gw-a/dial", tokA, "")
	require.Error(t, err)
	assert.Equal(t, http.StatusUpgradeRequired, resp.StatusCode, "missing subprotocol")

	// A client token is not a bridge token.
	_, resp, err = h.dial("/gw-a/dial", h.admin, BridgeSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestBridgeHelloRules(t *testing.T) {
	h := newHarness(t, Config{HelloTimeout: 200 * time.Millisecond})
	tok := h.createGateway("gw-a")

	t.Run("no hello", func(t *testing.T) {
		ws, _, err := h.dial("/gw-a/dial", tok, BridgeSubprotocol)
		require.NoError(t, err)
		defer ws.Close()
		expectClosed(t, ws, CloseProtocolError)
	})
	t.Run("hello for another gateway", func(t *testing.T) {
		ws, _, err := h.dial("/gw-a/dial", tok, BridgeSubprotocol)
		require.NoError(t, err)
		defer ws.Close()
		require.NoError(t, ws.WriteJSON(hello("gw-other", pubkeyA)))
		expectClosed(t, ws, CloseGatewayMismatch)
	})
	t.Run("unsupported protocol", func(t *testing.T) {
		ws, _, err := h.dial("/gw-a/dial", tok, BridgeSubprotocol)
		require.NoError(t, err)
		defer ws.Close()
		m := hello("gw-a", pubkeyA)
		m["protocol"] = 2
		require.NoError(t, ws.WriteJSON(m))
		expectClosed(t, ws, CloseUnsupported)
	})
	t.Run("binary first", func(t *testing.T) {
		ws, _, err := h.dial("/gw-a/dial", tok, BridgeSubprotocol)
		require.NoError(t, err)
		defer ws.Close()
		require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}))
		expectClosed(t, ws, CloseProtocolError)
	})
}

// Acceptance 3: a second bridge on the same token takes over; the first gets 4009.
func TestBridgeReplaced(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	first := h.bridge("gw-a", tok, pubkeyA)
	second := h.bridge("gw-a", tok, pubkeyA)
	first.expectClose(CloseReplaced)

	time.Sleep(50 * time.Millisecond) // the replaced session's teardown must not take the gateway offline
	st := gatewayStatus(h, "gw-a")
	assert.Equal(t, true, st["online"])

	// The new session carries traffic.
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")
	second.expect("attach")
}

// Acceptance 4: an operator lease exchanges frames end to end; a second client gets 4029.
func TestClientLeaseEndToEnd(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)

	c := h.client("gw-a", op, "")
	lease := expectControl(t, c, "lease")
	assert.Equal(t, "gw-a", lease["gateway"])
	assert.Equal(t, pubkeyA, lease["node"].(map[string]any)["pubkey"])
	attach := b.expect("attach")
	assert.Equal(t, lease["lease"], attach["lease"])
	assert.Equal(t, "op", attach["client"])

	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, []byte{0x01, 0x03, 'h', 'i'}))
	_, frame, err := read(t, c)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x80, 0x01, 0x03, 'h', 'i'}, frame, "frames pass through unchanged both ways")

	// Second client: busy.
	agent := h.createClient("elmer", RoleAgent)
	c2 := h.client("gw-a", agent, "")
	m := expectControl(t, c2, "error")
	assert.Equal(t, "busy", m["code"])
	expectClosed(t, c2, CloseBusy)

	// Hanging up releases the lease (detach to the bridge, audit row closed).
	c.Close()
	b.expect("detach")
	eventually(t, 2*time.Second, func() bool {
		_, out := h.api("GET", "/leases", h.admin, nil)
		recent := out["recent"].([]any)
		if len(recent) != 1 {
			return false
		}
		r := recent[0].(map[string]any)
		return r["end_reason"] == "client_closed" && r["frames_to_node"] == 1.0 && r["frames_from_node"] == 1.0
	}, "lease audit row not recorded with frame counts")
}

func TestClientGatewayOfflineAndUnknown(t *testing.T) {
	h := newHarness(t, Config{})
	h.createGateway("gw-a")
	op := h.createClient("op", RoleOperator)

	c := h.client("gw-a", op, "")
	st := expectControl(t, c, "gateway_status")
	assert.Equal(t, "offline", st["bridge"])
	expectClosed(t, c, CloseNormal)

	_, resp, err := h.dial("/no-such-gw", op, ClientSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	_, resp, err = h.dial("/gw-a", "smc_bogus", ClientSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	_, resp, err = h.dial("/gw-a", op, BridgeSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUpgradeRequired, resp.StatusCode)
}

// Acceptance 5a: idle timeout sends lease_ending and closes cleanly.
func TestLeaseIdleTimeout(t *testing.T) {
	h := newHarness(t, Config{LeaseIdleTimeout: 300 * time.Millisecond})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")

	m := expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonIdle, m["reason"])
	assert.EqualValues(t, 0, m["in_s"])
	expectClosed(t, c, CloseNormal)
	b.expect("detach")
}

func TestLeaseExpiryWarningAndRenew(t *testing.T) {
	h := newHarness(t, Config{LeaseDuration: 600 * time.Millisecond, LeaseWarning: 400 * time.Millisecond,
		MaxLeaseTotal: 1500 * time.Millisecond})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	lease := expectControl(t, c, "lease")

	warn := expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonExpiry, warn["reason"])
	assert.Greater(t, warn["in_s"].(float64), -1.0)

	require.NoError(t, c.WriteJSON(map[string]any{"type": "renew", "lease": lease["lease"]}))
	renewed := expectControl(t, c, "lease")
	assert.Equal(t, lease["lease"], renewed["lease"])

	// Renewals stop at MaxLeaseTotal, then the lease expires.
	for {
		m, _, err := read(t, c)
		require.NoError(t, err)
		if m["type"] == "lease_ending" && m["in_s"] == 0.0 {
			assert.Equal(t, ReasonExpiry, m["reason"])
			break
		}
		if m["type"] == "lease_ending" {
			require.NoError(t, c.WriteJSON(map[string]any{"type": "renew", "lease": lease["lease"]}))
		}
	}
	expectClosed(t, c, CloseNormal)
}

// Acceptance 5b: admin preemption.
func TestAdminPreempt(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")
	b.expect("attach")

	// Only admins may preempt.
	_, resp, err := h.dial("/gw-a?preempt=1", op, ClientSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	admin := h.client("gw-a", h.admin, "?preempt=1")
	m := expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonPreempted, m["reason"])
	expectClosed(t, c, CloseNormal)
	lease := expectControl(t, admin, "lease")
	b.expect("detach")
	attach := b.expect("attach")
	assert.Equal(t, lease["lease"], attach["lease"])
	assert.Equal(t, "root", attach["client"])
}

// Acceptance 6: a pubkey change marks mismatch and blocks leases until accept-pubkey.
func TestPubkeyMismatch(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	first := h.bridge("gw-a", tok, pubkeyA)
	first.ws.Close()
	eventually(t, 2*time.Second, func() bool { return gatewayStatus(h, "gw-a")["online"] == false }, "never offline")

	h.bridge("gw-a", tok, pubkeyB)
	st := gatewayStatus(h, "gw-a")
	assert.Equal(t, "mismatch", st["pubkey_status"])
	assert.Equal(t, pubkeyA, st["pinned_pubkey"])
	assert.Equal(t, pubkeyB, st["mismatch_pubkey"])

	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	gs := expectControl(t, c, "gateway_status")
	assert.Equal(t, NodeMismatch, gs["node"].(map[string]any)["state"])
	e := expectControl(t, c, "error")
	assert.Equal(t, "pubkey_mismatch", e["code"])
	expectClosed(t, c, CloseNormal)

	code, _ := h.api("POST", "/gateways/gw-a/accept-pubkey", op, nil)
	assert.Equal(t, http.StatusForbidden, code, "operators cannot accept a pubkey")
	code, out := h.api("POST", "/gateways/gw-a/accept-pubkey", h.admin, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, pubkeyB, out["gateway"].(map[string]any)["pinned_pubkey"])

	c = h.client("gw-a", op, "")
	expectControl(t, c, "lease")
}

func TestPubkeyChangeMidLeaseEndsIt(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")

	require.NoError(t, b.ws.WriteJSON(map[string]any{"type": "node_status", "state": "connected", "pubkey": pubkeyB}))
	m := expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonPubkeyMismatch, m["reason"])
	expectClosed(t, c, CloseNormal)
}

func TestGatewayOfflineEndsLease(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")

	b.ws.Close()
	st := expectControl(t, c, "gateway_status")
	assert.Equal(t, "offline", st["bridge"])
	m := expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonGatewayOffline, m["reason"])
	expectClosed(t, c, CloseNormal)
}

func TestRotateTokenClosesSession(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)

	code, out := h.api("POST", "/gateways/gw-a/rotate-token", h.admin, nil)
	require.Equal(t, http.StatusOK, code)
	b.expectClose(CloseBadToken)

	_, resp, err := h.dial("/gw-a/dial", tok, BridgeSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the old token stops working")
	h.bridge("gw-a", out["bridge_token"].(string), pubkeyA)
}

func TestRelinkAndDelete(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)

	code, _ := h.api("POST", "/gateways/gw-a/relink", h.admin, nil)
	assert.Equal(t, http.StatusOK, code)
	b.expect("relink")

	code, _ = h.api("DELETE", "/gateways/gw-a", h.admin, nil)
	assert.Equal(t, http.StatusOK, code)
	b.expectClose(CloseBadToken)
	code, _ = h.api("GET", "/gateways/gw-a", h.admin, nil)
	assert.Equal(t, http.StatusNotFound, code)
}

func TestRevokeClientEndsLease(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")

	code, _ := h.api("DELETE", "/clients/op", h.admin, nil)
	require.Equal(t, http.StatusOK, code)
	m := expectControl(t, c, "lease_ending")
	assert.Equal(t, ReasonRevoked, m["reason"])

	_, resp, err := h.dial("/gw-a", op, ClientSubprotocol)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIRoles(t *testing.T) {
	h := newHarness(t, Config{})
	op := h.createClient("op", RoleOperator)
	agent := h.createClient("elmer", RoleAgent)
	tok := h.createGateway("gw-a")

	code, _ := h.api("GET", "/gateways", "", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	code, _ = h.api("GET", "/gateways", tok, nil)
	assert.Equal(t, http.StatusUnauthorized, code, "a bridge token is not an API credential")
	for _, who := range []string{op, agent} {
		code, _ = h.api("GET", "/gateways", who, nil)
		assert.Equal(t, http.StatusOK, code, "view status")
		code, _ = h.api("POST", "/gateways", who, map[string]any{"name": "gw-x"})
		assert.Equal(t, http.StatusForbidden, code)
		code, _ = h.api("POST", "/clients", who, map[string]any{"name": "x", "role": "admin"})
		assert.Equal(t, http.StatusForbidden, code)
	}

	code, _ = h.api("POST", "/gateways", h.admin, map[string]any{"name": "api"})
	assert.Equal(t, http.StatusBadRequest, code, "reserved name")
	code, _ = h.api("POST", "/gateways", h.admin, map[string]any{"name": "gw-a"})
	assert.Equal(t, http.StatusConflict, code)
	code, _ = h.api("POST", "/gateways", h.admin, map[string]any{"name": "gw-b", "expected_pubkey": "nope"})
	assert.Equal(t, http.StatusBadRequest, code)
	code, _ = h.api("DELETE", "/clients/root", h.admin, nil)
	assert.Equal(t, http.StatusConflict, code, "bootstrap admins cannot be revoked via the API")

	_, out := h.api("GET", "/clients", h.admin, nil)
	assert.NotContains(t, mustJSON(out), "sha256", "client listing never exposes hashes")
}

func TestExpectedPubkeyIsPinnedAtCreate(t *testing.T) {
	h := newHarness(t, Config{})
	code, out := h.api("POST", "/gateways", h.admin, map[string]any{"name": "gw-a", "expected_pubkey": strings.ToUpper(pubkeyA)})
	require.Equal(t, http.StatusOK, code)
	h.bridge("gw-a", out["bridge_token"].(string), pubkeyB)
	assert.Equal(t, "mismatch", gatewayStatus(h, "gw-a")["pubkey_status"])
}

func TestFrameLimits(t *testing.T) {
	h := newHarness(t, Config{FrameRate: 50, FrameBurst: 5})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)

	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")
	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, make([]byte, MaxFrameBytes+1)))
	m := expectControl(t, c, "error")
	assert.Equal(t, "frame_too_large", m["code"])
	expectClosed(t, c, CloseProtocolError)

	// Excess frames are throttled, not dropped and not disconnected: 25 frames
	// at 50/s with a burst of 5 take at least ~400ms, and all 25 arrive.
	c = h.client("gw-a", op, "")
	expectControl(t, c, "lease")
	start := time.Now()
	for i := 0; i < 25; i++ {
		// Sequence in the second byte: some first bytes are refused commands.
		require.NoError(t, c.WriteMessage(websocket.BinaryMessage, []byte{0x20, byte(i)}))
	}
	for i := 0; i < 25; i++ {
		_, frame, err := read(t, c)
		require.NoError(t, err)
		require.Equal(t, []byte{0x80, 0x20, byte(i)}, frame)
	}
	assert.GreaterOrEqual(t, time.Since(start), 350*time.Millisecond)
}

func TestAuthLockout(t *testing.T) {
	h := newHarness(t, Config{AuthFailLimit: 3})
	h.createGateway("gw-a")
	for i := 0; i < 3; i++ {
		_, resp, _ := h.dial("/gw-a/dial", "smb_bad", BridgeSubprotocol)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
	_, resp, _ := h.dial("/gw-a/dial", "smb_bad", BridgeSubprotocol)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	code, _ := h.api("GET", "/gateways", h.admin, nil)
	assert.Equal(t, http.StatusTooManyRequests, code, "lockout is per IP, across endpoints")
}

// Acceptance 7 (the relay half): state survives a restart; leases do not.
func TestRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "relay.db")
	h := newHarnessAt(t, Config{}, db)
	tok := h.createGateway("gw-a")
	op := h.createClient("op", RoleOperator)
	h.bridge("gw-a", tok, pubkeyA)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")
	h.stop() // closes sessions; the lease ends as relay_shutdown

	h2 := newHarnessAt(t, Config{}, db)
	st := gatewayStatus(h2, "gw-a")
	assert.Equal(t, false, st["online"])
	assert.NotEmpty(t, st["last_seen"])
	assert.Equal(t, pubkeyA, st["pinned_pubkey"], "the pin survives")
	_, out := h2.api("GET", "/leases", h2.admin, nil)
	assert.Empty(t, out["current"])
	require.Len(t, out["recent"], 1)
	assert.Equal(t, ReasonShutdown, out["recent"].([]any)[0].(map[string]any)["end_reason"])

	// The bridge (and its token) and the client token still work.
	h2.bridge("gw-a", tok, pubkeyA)
	c = h2.client("gw-a", op, "")
	expectControl(t, c, "lease")
}

func TestOrphanLeasesClosedAtStartup(t *testing.T) {
	db := filepath.Join(t.TempDir(), "relay.db")
	st, err := OpenStore(context.Background(), db, time.Second)
	require.NoError(t, err)
	require.NoError(t, st.InsertLease(LeaseRecord{ID: "l_x", Gateway: "gw", Client: "c", Role: RoleAgent, StartedAt: time.Now()}))
	require.NoError(t, st.Close())

	h := newHarnessAt(t, Config{}, db)
	eventually(t, 2*time.Second, func() bool {
		_, out := h.api("GET", "/leases", h.admin, nil)
		recent, _ := out["recent"].([]any)
		return len(recent) == 1 && recent[0].(map[string]any)["end_reason"] == "relay_restart"
	}, "orphan lease not closed")
}

// Acceptance 8: no payload bytes in logs at the default level.
func TestNoPayloadInLogs(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")

	secret := []byte("CHANNEL-SECRET-0123456789")
	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, secret))
	_, frame, err := read(t, c)
	require.NoError(t, err)
	require.Equal(t, append([]byte{0x80}, secret...), frame)
	c.Close()
	time.Sleep(100 * time.Millisecond)

	logs := h.logs.String()
	assert.Contains(t, logs, "lease ended", "sanity: the session was logged")
	assert.NotContains(t, logs, string(secret))
	assert.NotContains(t, logs, hexString(secret))
	assert.NotContains(t, logs, tok, "tokens are never logged")
	assert.NotContains(t, logs, op)
}

// The public status board: no token, and nothing beyond "does it work".
func TestPublicStatus(t *testing.T) {
	h := newHarness(t, Config{})
	code, out := h.api("POST", "/gateways", h.admin, map[string]any{"name": "gw-a", "notes": "behind the barn"})
	require.Equal(t, http.StatusOK, code)
	tok := out["bridge_token"].(string)
	h.createGateway("gw-b")
	b := h.bridge("gw-a", tok, pubkeyA)
	op := h.createClient("casey-holder", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")

	code, st := h.api("GET", "/status", "", nil)
	require.Equal(t, http.StatusOK, code, "no token needed")
	gws := st["gateways"].([]any)
	require.Len(t, gws, 2)
	a, bb := gws[0].(map[string]any), gws[1].(map[string]any)
	assert.Equal(t, "gw-a", a["name"])
	assert.Equal(t, true, a["online"])
	assert.Equal(t, true, a["ready"])
	assert.Equal(t, true, a["in_use"])
	assert.Equal(t, string(RoleOperator), a["in_use_role"], "the holder's role, never its name")
	assert.Nil(t, bb["in_use_role"])
	assert.Equal(t, NodeConnected, a["node_state"])
	assert.Equal(t, "0.3.1", a["agent_version"])
	assert.Equal(t, false, bb["online"])
	assert.Nil(t, bb["node_state"], "an offline gateway's node state is unknown, not disconnected")

	raw := mustJSON(st)
	for _, secret := range []string{pubkeyA, "behind the barn", "casey-holder", "/dev/ttyACM0", "SIERRA-GW-", "linux", "arm64"} {
		assert.NotContains(t, raw, `"`+secret, "public status must not expose %q", secret)
		assert.NotContains(t, raw, secret+`"`, "public status must not expose %q", secret)
	}

	// A node reporting an unpinned key is not ready, and says so.
	require.NoError(t, b.ws.WriteJSON(map[string]any{"type": "node_status", "state": "connected", "pubkey": pubkeyB}))
	eventually(t, 2*time.Second, func() bool {
		_, st := h.api("GET", "/status", "", nil)
		g := st["gateways"].([]any)[0].(map[string]any)
		return g["node_state"] == NodeMismatch && g["ready"] == false
	}, "mismatch not reflected in public status")
}

func TestHealthAndRootRedirect(t *testing.T) {
	h := newHarness(t, Config{})
	resp, err := http.Get(h.base + PathPrefix + "/healthz")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// The relay has no page of its own: its root points at the site's /relay.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{PathPrefix, PathPrefix + "/"} {
		resp, err := noFollow.Get(h.base + p)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusFound, resp.StatusCode, p)
		assert.Equal(t, SiteStatusPath, resp.Header.Get("Location"), p)
	}
}

// The admin API speaks prefab's JSON error envelope, like /api/v1.
func TestAPIErrorEnvelope(t *testing.T) {
	h := newHarness(t, Config{})
	code, out := h.api("GET", "/gateways/nope", h.admin, nil)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "NOT_FOUND", out["codeName"])
	assert.Equal(t, "unknown gateway", out["message"])
	code, out = h.api("GET", "/gateways", "", nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Equal(t, "UNAUTHENTICATED", out["codeName"])
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// chanHolder is an in-process lease holder, as an internal poller would be.
type chanHolder struct {
	frames chan []byte
	ended  chan string
}

func newChanHolder() *chanHolder {
	return &chanHolder{frames: make(chan []byte, 8), ended: make(chan string, 1)}
}

func (h *chanHolder) Deliver(f []byte) {
	select {
	case h.frames <- f:
	default:
	}
}
func (h *chanHolder) Notify(any) {}
func (h *chanHolder) End(reason string) {
	select {
	case h.ended <- reason:
	default:
	}
}

// An in-process holder takes a background lease, exchanges frames through
// Send/Deliver, and gives way to a human session.
func TestInProcessHolderSendAndPreempt(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)

	poller := newChanHolder()
	l, err := h.relay.Acquire(LeaseRequest{Gateway: "gw-a", Client: "poller", Role: RoleAgent,
		Priority: PriorityBackground, Holder: poller})
	require.NoError(t, err)
	b.expect("attach")

	require.NoError(t, h.relay.Send(l, []byte{0x38, 0x00}))
	select {
	case f := <-poller.frames:
		assert.Equal(t, []byte{0x80, 0x38, 0x00}, f)
	case <-time.After(2 * time.Second):
		t.Fatal("no frame delivered to the in-process holder")
	}
	assert.ErrorIs(t, h.relay.Send(l, make([]byte, MaxFrameBytes+1)), ErrFrameTooLarge)

	// A normal-priority client preempts the background lease.
	op := h.createClient("op", RoleOperator)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")
	select {
	case reason := <-poller.ended:
		assert.Equal(t, ReasonPreempted, reason)
	case <-time.After(2 * time.Second):
		t.Fatal("background lease was not preempted")
	}
	assert.ErrorIs(t, h.relay.Send(l, []byte{0x01}), ErrLeaseEnded)

	// And while the human holds it, the poller is refused.
	_, err = h.relay.Acquire(LeaseRequest{Gateway: "gw-a", Client: "poller", Role: RoleAgent,
		Priority: PriorityBackground, Holder: newChanHolder()})
	assert.ErrorIs(t, err, ErrBusy)
}

func TestNodePolicyRefusesSecretsBothWays(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	// The policy applies to every remote session, admins included.
	c := h.client("gw-a", h.admin, "")
	expectControl(t, c, "lease")
	b.expect("attach")

	next := func() []byte {
		t.Helper()
		_, frame, err := read(t, c)
		require.NoError(t, err)
		return frame
	}
	for _, f := range [][]byte{
		{nodepolicy.CmdExportPrivateKey},
		append([]byte{nodepolicy.CmdRunCLICommand}, " ab|get wifi.pwd"...),
	} {
		require.NoError(t, c.WriteMessage(websocket.BinaryMessage, f))
		assert.Equal(t, nodepolicy.Refusal(), next(), "refused with the firmware's disabled reply")
	}
	// The refused frames never reached the bridge: the next echo is this one.
	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, []byte{0x16, 0x03}))
	assert.Equal(t, []byte{0x80, 0x16, 0x03}, next())

	// A private key reply from the node is dropped; the frame after it arrives.
	b.send(append([]byte{nodepolicy.RespPrivateKey}, make([]byte, 64)...))
	b.send([]byte{0x05, 0x01})
	assert.Equal(t, []byte{0x05, 0x01}, next())

	assert.EqualValues(t, 3, h.relay.Metrics().FramesRefused)
}

func TestNodePolicyAppliesToInProcessHolders(t *testing.T) {
	h := newHarness(t, Config{})
	tok := h.createGateway("gw-a")
	b := h.bridge("gw-a", tok, pubkeyA)
	holder := newChanHolder()
	l, err := h.relay.Acquire(LeaseRequest{Gateway: "gw-a", Client: "poller", Role: RoleAgent,
		Priority: PriorityBackground, Holder: holder})
	require.NoError(t, err)
	b.expect("attach")

	require.NoError(t, h.relay.Send(l, []byte{nodepolicy.CmdFactoryReset}), "a refusal doesn't end the lease")
	require.NoError(t, h.relay.Send(l, []byte{0x01}))
	for _, want := range [][]byte{nodepolicy.Refusal(), {0x80, 0x01}} {
		select {
		case f := <-holder.frames:
			assert.Equal(t, want, f)
		case <-time.After(2 * time.Second):
			t.Fatalf("no frame %x delivered", want)
		}
	}
}
