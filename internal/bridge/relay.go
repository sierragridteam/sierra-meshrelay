package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// session is one established (welcomed) relay connection.
type session struct {
	conn *websocket.Conn
	// ioCtx bounds socket reads and writes. coder/websocket closes the socket
	// at once when an I/O context ends, so this outlives the session context
	// and is cancelled only after the close handshake, letting a 1001 close
	// frame go out on shutdown.
	ioCtx context.Context
	out   chan []byte   // node → relay frames, drained by the writer
	done  chan struct{} // closed when the session ends

	mu     sync.Mutex
	leases map[string]string // lease id → client name
}

func (s *session) isAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.leases) > 0
}

// runRelay keeps a relay session up for as long as ctx lives.
func (b *Bridge) runRelay(ctx context.Context) {
	for ctx.Err() == nil {
		res := b.relaySession(ctx)
		if ctx.Err() != nil {
			return
		}
		b.mu.Lock()
		b.relayClass = Classify(res)
		b.mu.Unlock()
		wait := b.opts.Backoff.Next(res)
		attrs := []any{"wait", wait.Round(time.Millisecond), "uptime", res.Uptime.Round(time.Second)}
		if res.HTTPStatus != 0 {
			attrs = append(attrs, "http_status", res.HTTPStatus)
		}
		if res.CloseCode >= 0 {
			attrs = append(attrs, "close_code", res.CloseCode)
		}
		if res.Err != nil {
			attrs = append(attrs, "error", res.Err)
		}
		switch Classify(res) {
		case ClassAuth:
			b.log.Error("relay rejected this bridge's credentials; check the token and gateway name", attrs...)
		case ClassUpgrade:
			b.log.Error("the relay doesn't support this bridge's protocol version; update required", attrs...)
		default:
			if res.CloseCode == meshproto.CloseMessageTooBig {
				b.log.Error("relay closed with 1009 (message too big): this is a bridge bug", attrs...)
			} else if res.CloseCode == meshproto.CloseReplaced {
				b.log.Warn("another bridge with this token connected (4009); two bridges sharing a token will flap", attrs...)
			} else {
				b.log.Info("relay connection ended; reconnecting", attrs...)
			}
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

func (b *Bridge) dialURL() string {
	return b.opts.Config.Relay + "/" + b.opts.Config.Gateway + "/dial"
}

// relaySession dials the relay and serves one session.
func (b *Bridge) relaySession(ctx context.Context) (res SessionResult) {
	res.CloseCode = -1
	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, b.dialURL(), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + b.opts.Config.Token}},
		Subprotocols: []string{meshproto.BridgeSubprotocol},
	})
	cancelDial()
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			res.HTTPStatus = resp.StatusCode
		}
		res.Err = err
		return res
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != meshproto.BridgeSubprotocol {
		conn.Close(websocket.StatusPolicyViolation, "subprotocol required")
		res.Err = fmt.Errorf("relay didn't accept subprotocol %s", meshproto.BridgeSubprotocol)
		return res
	}
	// Binary frames are ≤512 bytes and text ≤8 KiB; leave room for framing.
	conn.SetReadLimit(meshproto.MaxControlBytes + 1024)

	defer func() {
		if code := websocket.CloseStatus(res.Err); code != -1 {
			res.CloseCode = int(code)
		}
	}()

	node, link, ver, _ := b.nodeSnapshot()
	hello := meshproto.Hello{
		Type: meshproto.TypeHello, Protocol: meshproto.Version, Gateway: b.opts.Config.Gateway,
		Agent: b.agent(), Link: link, Node: helloNode(node),
	}
	if res.Err = writeJSON(ctx, conn, hello); res.Err != nil {
		return res
	}
	welcome, err := b.awaitWelcome(ctx, conn)
	if err != nil {
		res.Err = err
		return res
	}
	res.Welcomed = true
	start := time.Now()
	defer func() { res.Uptime = time.Since(start) }()
	if b.relayWelcomes.Add(1) > 1 {
		b.log.Debug("relay reconnected")
	}
	pingEvery := time.Duration(welcome.PingIntervalS) * time.Second
	if pingEvery <= 0 {
		pingEvery = 25 * time.Second
	}
	b.log.Info("connected to relay", "gateway", b.opts.Config.Gateway, "session", welcome.Session,
		"ping_interval", pingEvery, "node_state", node.State)

	ioCtx, cancelIO := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelIO()
	s := &session{conn: conn, ioCtx: ioCtx, out: make(chan []byte, b.opts.OutQueue), done: make(chan struct{}), leases: map[string]string{}}
	sctx, cancel := context.WithCancelCause(ctx)
	b.mu.Lock()
	b.session, b.relayClass = s, ClassRetry
	b.mu.Unlock()
	defer func() {
		b.clearSession(s)
		close(s.done)
		cancel(nil)
	}()

	var wg sync.WaitGroup
	run := func(f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(sctx); err != nil {
				cancel(err)
			}
		}()
	}
	// The read loop's error carries the relay's close code, which picks the
	// backoff. Other goroutines can fail first once the socket is gone, so
	// it's kept separately rather than taken from the context cause.
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		readErr = b.readLoop(s)
		cancel(readErr)
	}()
	run(func(ctx context.Context) error { return b.writeLoop(ctx, s) })
	run(func(ctx context.Context) error { return b.statusLoop(ctx, s, ver) })
	run(func(ctx context.Context) error { return b.statsLoop(ctx, s) })
	run(func(ctx context.Context) error { return pingLoop(ctx, s, pingEvery) })
	if b.opts.Diag != nil {
		run(func(ctx context.Context) error { return b.logLoop(ctx, s) })
	}

	<-sctx.Done()
	err = context.Cause(sctx)
	if ctx.Err() != nil {
		conn.Close(websocket.StatusGoingAway, "bridge shutting down")
	} else {
		conn.CloseNow()
	}
	cancelIO()
	wg.Wait()
	res.Err = err
	if websocket.CloseStatus(readErr) != -1 {
		res.Err = readErr
	}
	return res
}

// helloNode trims node fields the protocol says to omit when not connected.
func helloNode(n meshproto.Node) meshproto.Node {
	if n.State == meshproto.NodeConnected || n.State == meshproto.NodeMismatch {
		return n
	}
	return meshproto.Node{State: n.State, Until: n.Until, Error: n.Error}
}

func (b *Bridge) awaitWelcome(ctx context.Context, conn *websocket.Conn) (meshproto.Welcome, error) {
	ctx, cancel := context.WithTimeout(ctx, b.opts.HelloTimeout)
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return meshproto.Welcome{}, err
		}
		if typ != websocket.MessageText {
			continue
		}
		switch meshproto.TypeOf(data) {
		case meshproto.TypeWelcome:
			var w meshproto.Welcome
			if err := json.Unmarshal(data, &w); err != nil {
				return w, fmt.Errorf("bad welcome: %w", err)
			}
			return w, nil
		case meshproto.TypeError:
			b.logRelayError(data)
		}
	}
}

func (b *Bridge) readLoop(s *session) error {
	for {
		typ, data, err := s.conn.Read(s.ioCtx)
		if err != nil {
			return err
		}
		switch typ {
		case websocket.MessageBinary:
			if len(data) > meshproto.MaxFrameBytes {
				b.log.Warn("dropping oversized frame from relay", "bytes", len(data))
				b.droppedToNode.Add(1)
				continue
			}
			b.toNode(s, data)
		case websocket.MessageText:
			b.handleControl(s, data)
		}
	}
}

func (b *Bridge) handleControl(s *session, data []byte) {
	switch meshproto.TypeOf(data) {
	case meshproto.TypeAttach:
		var m meshproto.Attach
		if json.Unmarshal(data, &m) == nil {
			s.mu.Lock()
			s.leases[m.Lease] = m.Client
			s.mu.Unlock()
			b.log.Info("client attached", "lease", m.Lease, "client", m.Client)
		}
	case meshproto.TypeDetach:
		var m meshproto.Detach
		if json.Unmarshal(data, &m) == nil {
			s.mu.Lock()
			client := s.leases[m.Lease]
			delete(s.leases, m.Lease)
			s.mu.Unlock()
			b.log.Info("client detached", "lease", m.Lease, "client", client)
		}
	case meshproto.TypeRelink:
		b.Relink()
	case meshproto.TypeUpdateAvailable:
		var m meshproto.UpdateAvailable
		if json.Unmarshal(data, &m) != nil {
			return
		}
		if b.opts.OnUpdate != nil {
			go b.opts.OnUpdate(m)
		} else {
			b.log.Info("relay reports an update is available", "version", m.Version)
		}
	case meshproto.TypeError:
		b.logRelayError(data)
	}
	// Unknown types are ignored (forward compatibility).
}

// logRelayError logs an error's code. The message is free text and is never
// shown or relied on.
func (b *Bridge) logRelayError(data []byte) {
	var m meshproto.Error
	_ = json.Unmarshal(data, &m)
	switch m.Code {
	case meshproto.ErrPubkeyMismatch:
		b.log.Error("SIERRA has a different node on record for this gateway; "+
			"if you replaced the node, run meshbridge change-node on this computer", "code", m.Code)
	case meshproto.ErrPubkeyUnverified:
		b.log.Error("relay reported an error", "code", m.Code)
	default:
		b.log.Warn("relay reported an error", "code", m.Code)
	}
}

func (b *Bridge) writeLoop(ctx context.Context, s *session) error {
	for {
		select {
		case f := <-s.out:
			if err := s.write(websocket.MessageBinary, f); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// statusLoop sends node_status whenever the node changes after hello.
func (b *Bridge) statusLoop(ctx context.Context, s *session, sentVer uint64) error {
	for {
		node, _, ver, changed := b.nodeSnapshot()
		if ver != sentVer {
			msg := meshproto.NodeStatus{Type: meshproto.TypeNodeStatus, Node: helloNode(node)}
			if err := s.writeJSON(msg); err != nil {
				return err
			}
			sentVer = ver
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *Bridge) statsLoop(ctx context.Context, s *session) error {
	t := time.NewTicker(b.opts.StatsInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			msg := meshproto.StatsMsg{Type: meshproto.TypeStats, Stats: meshproto.Stats{
				UptimeS:            int64(time.Since(b.started).Seconds()),
				NodeLinkReconnects: max(b.nodeOpens.Load()-1, 0),
				FramesToNode:       b.framesToNode.Load(),
				FramesFromNode:     b.framesFromNode.Load(),
				RelayReconnects:    max(b.relayWelcomes.Load()-1, 0),
			}}
			if err := s.writeJSON(msg); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// maxLogMsg keeps a log message well under the relay's 8 KiB limit.
const maxLogMsg = meshproto.MaxControlBytes - 1024

// logLoop sends log lines the relay hasn't had, including those logged while
// the bridge was offline (the ring holds the last few hundred).
func (b *Bridge) logLoop(ctx context.Context, s *session) error {
	for {
		changed := b.opts.Diag.Changed()
		entries, lost := b.opts.Diag.Since(b.diagSent.Load())
		for len(entries) > 0 {
			msg := meshproto.LogMsg{Type: meshproto.TypeLog, Lost: lost}
			size := 64
			for _, e := range entries {
				eb, _ := json.Marshal(e)
				if len(msg.Entries) > 0 && size+len(eb)+1 > maxLogMsg {
					break
				}
				msg.Entries = append(msg.Entries, e)
				size += len(eb) + 1
			}
			if err := s.writeJSON(msg); err != nil {
				return err
			}
			b.diagSent.Store(msg.Entries[len(msg.Entries)-1].Seq)
			entries, lost = entries[len(msg.Entries):], 0
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil
		}
	}
}

// errPingTimeout ends a session after three missed pongs.
var errPingTimeout = errors.New("relay missed 3 pongs")

func pingLoop(ctx context.Context, s *session, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	missed := 0
	for {
		select {
		case <-t.C:
			pctx, cancel := context.WithTimeout(s.ioCtx, every)
			err := s.conn.Ping(pctx)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				if missed++; missed >= 3 {
					return errPingTimeout
				}
				continue
			}
			missed = 0
		case <-ctx.Done():
			return nil
		}
	}
}

// writeTimeout bounds one write. Under the relay's backpressure a write
// waits while the relay reads slowly; a relay that reads nothing for this long
// is gone.
const writeTimeout = 30 * time.Second

func (s *session) write(typ websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(s.ioCtx, writeTimeout)
	defer cancel()
	return s.conn.Write(ctx, typ, data)
}

func (s *session) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write(websocket.MessageText, b)
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}
