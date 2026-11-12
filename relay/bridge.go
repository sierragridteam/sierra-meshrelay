package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/gorilla/websocket"
)

// bridgeSession is one bridge's websocket.
type bridgeSession struct {
	id      string
	gateway string
	conn    *conn
	limiter *rateLimiter
	logs    logBudget
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Bridges and clients are programs presenting a bearer token, not
	// browsers, and the token is what authorizes them. A page on another
	// origin cannot set an Authorization header on a WebSocket handshake, so
	// there is no ambient credential for an Origin check to protect.
	CheckOrigin: func(*http.Request) bool { return true },
}

func hasSubprotocol(req *http.Request, want string) bool {
	for _, p := range websocket.Subprotocols(req) {
		if p == want {
			return true
		}
	}
	return false
}

// handleBridge serves /mesh/relay/{gateway}/dial.
func (r *Relay) handleBridge(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("gateway")
	ip := r.clientIP(req)
	if r.lockout.isLocked(ip, r.now()) {
		http.Error(w, "too many authentication failures", http.StatusTooManyRequests)
		return
	}
	tok := bearerToken(req.Header.Get("Authorization"))
	rec, err := r.store.GatewayByToken(TokenHash(tok))
	if tok == "" || errors.Is(err, ErrNotFound) {
		r.authFailed(w, ip, "bridge", name)
		return
	}
	if err != nil {
		logging.Errorw(r.ctx, "bridge auth lookup", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if rec.Name != name {
		// A real token for a different gateway: still a 401, and still counted —
		// it is either a misconfigured bridge or someone probing names.
		r.authFailed(w, ip, "bridge", name)
		return
	}
	if !hasSubprotocol(req, BridgeSubprotocol) {
		http.Error(w, "unsupported subprotocol; want "+BridgeSubprotocol, http.StatusUpgradeRequired)
		return
	}
	ws, err := upgrader.Upgrade(w, req, http.Header{"Sec-WebSocket-Protocol": {BridgeSubprotocol}})
	if err != nil {
		return // Upgrade already wrote the error
	}
	r.sessionStart()
	defer r.sessionEnd()
	s := &bridgeSession{
		id: randomID("s_"), gateway: name, conn: newConn(ws, r.cfg.PingInterval),
		limiter: newRateLimiter(r.cfg.FrameRate, r.cfg.FrameBurst),
	}
	r.serveBridge(s, ip)
}

func (r *Relay) serveBridge(s *bridgeSession, ip string) {
	ctx := scoped(r.ctx, "gateway", s.gateway, "session", s.id)

	// hello, within HelloTimeout.
	_ = s.conn.ws.SetReadDeadline(r.now().Add(r.cfg.HelloTimeout))
	kind, data, err := s.conn.ws.ReadMessage()
	if err != nil {
		logging.Infow(ctx, "bridge sent no hello", "error", err.Error())
		s.conn.close(CloseProtocolError, "hello required")
		return
	}
	var h helloMsg
	if kind != websocket.TextMessage || json.Unmarshal(data, &h) != nil || h.Type != "hello" {
		s.conn.sendJSON(errorMsg{Type: "error", Code: "hello_required", Message: "first message must be hello"})
		s.conn.close(CloseProtocolError, "hello required")
		return
	}
	if h.Protocol != ProtocolVersion {
		s.conn.sendJSON(errorMsg{Type: "error", Code: "unsupported_protocol",
			Message: fmt.Sprintf("relay speaks protocol %d", ProtocolVersion)})
		s.conn.close(CloseUnsupported, "unsupported protocol version")
		return
	}
	if h.Gateway != s.gateway {
		s.conn.close(CloseGatewayMismatch, "gateway name does not match token")
		return
	}
	s.conn.extendRead()

	pinned, mismatch, err := r.checkPubkey(s.gateway, h.Node)
	if err != nil {
		logging.Errorw(ctx, "pubkey check failed", "error", err)
		s.conn.close(1011, "internal error")
		return
	}
	if !r.attachBridge(s, &h, pinned, mismatch) {
		return // the relay is shutting down; attachBridge closed it
	}
	logging.Infow(ctx, "bridge connected", "ip", ip, "agent", h.Agent.Name, "version", h.Agent.Version,
		"os", h.Agent.OS, "arch", h.Agent.Arch, "link", h.Link.Kind, "node_state", h.Node.State)
	r.offerUpdate(s, h.Agent.Version)

	reason := r.bridgeLoop(s)
	if r.detachBridge(s) {
		r.event(s.gateway, "disconnect", reason)
		logging.Infow(ctx, "bridge disconnected", "reason", reason)
	}
}

// bridgeLoop reads until the socket ends, returning why.
func (r *Relay) bridgeLoop(s *bridgeSession) string {
	for {
		kind, data, err := s.conn.ws.ReadMessage()
		if err != nil {
			s.conn.close(CloseNormal, "")
			return closeReason(err)
		}
		s.conn.extendRead()
		s.limiter.wait()
		switch kind {
		case websocket.BinaryMessage:
			if len(data) > MaxFrameBytes {
				s.conn.sendJSON(errorMsg{Type: "error", Code: "frame_too_large",
					Message: fmt.Sprintf("max frame is %d bytes", MaxFrameBytes)})
				s.conn.close(CloseProtocolError, "frame too large")
				return "frame too large"
			}
			r.fromNode(s, data)
		case websocket.TextMessage:
			r.bridgeControl(s, data)
		}
	}
}

func (r *Relay) bridgeControl(s *bridgeSession, data []byte) {
	var env envelope
	if json.Unmarshal(data, &env) != nil {
		return
	}
	switch env.Type {
	case "node_status":
		var m nodeStatusMsg
		if json.Unmarshal(data, &m) != nil {
			return
		}
		pinned, mismatch, err := r.checkPubkey(s.gateway, m.Node)
		if err != nil {
			logging.Errorw(r.ctx, "pubkey check failed", "gateway", s.gateway, "error", err)
			return
		}
		r.updateNode(s, m.Node, pinned, mismatch)
	case "stats":
		var m statsMsg
		if json.Unmarshal(data, &m) != nil {
			return
		}
		now := r.now()
		r.mu.Lock()
		if g := r.gws[s.gateway]; g != nil && g.bridge == s {
			st := m.Stats
			g.stats, g.statsAt, g.lastSeen = &st, now, now
		}
		r.mu.Unlock()
		r.audit("stats", func(st Storage) error { return st.RecordStats(s.gateway, m.Stats, now) })
	case "log":
		r.bridgeLog(s, data)
	case "error":
		var m errorMsg
		if json.Unmarshal(data, &m) == nil {
			// The code is ours to log; the message is free text from the bridge
			// and could echo anything, so only its length is recorded.
			logging.Warnw(r.ctx, "bridge reported error", "gateway", s.gateway, "code", m.Code, "message_len", len(m.Message))
		}
	}
	// Unknown types are ignored (forward compatibility).
}

// checkPubkey applies pubkey pinning to a reported node and persists the
// outcome synchronously, returning the gateway's pinned and mismatching keys.
//
//   - First connected pubkey seen with nothing pinned: pin it.
//   - Matches the pin: fine (and clears a previous mismatch — the node is back).
//   - Differs: record it as the mismatch. Leases stay blocked until an admin
//     accepts it (AcceptPubkey).
//
// A node that is not connected, or reports no well-formed key, changes nothing.
func (r *Relay) checkPubkey(gateway string, n NodeInfo) (pinned, mismatch string, err error) {
	r.pubkeyMu.Lock()
	defer r.pubkeyMu.Unlock()
	rec, err := r.store.Gateway(gateway)
	if err != nil {
		return "", "", err
	}
	pinned, mismatch = rec.PinnedPubkey, rec.MismatchPubkey
	pk, ok := NormalizePubkey(n.Pubkey)
	if n.State != NodeConnected || !ok {
		return pinned, mismatch, nil
	}
	var kind string
	switch {
	case pinned == "":
		pinned, mismatch, kind = pk, "", "pubkey_pinned"
	case pk == pinned:
		if mismatch != "" {
			mismatch, kind = "", "pubkey_restored"
		}
	case pk != mismatch:
		mismatch, kind = pk, "pubkey_mismatch"
	}
	if kind == "" {
		return pinned, mismatch, nil
	}
	if err := r.store.SetPubkeys(gateway, pinned, mismatch); err != nil {
		return "", "", err
	}
	r.event(gateway, kind, pk)
	if kind == "pubkey_mismatch" {
		logging.Warnw(r.ctx, "node pubkey does not match the pinned key; leases blocked until an admin accepts it",
			"gateway", gateway, "pinned", rec.PinnedPubkey, "reported", pk)
	} else {
		logging.Infow(r.ctx, "node pubkey "+kind[len("pubkey_"):], "gateway", gateway, "pubkey", pk)
	}
	return pinned, mismatch, nil
}

func (r *Relay) gwLocked(name string) *gwState {
	g := r.gws[name]
	if g == nil {
		g = &gwState{name: name}
		r.gws[name] = g
	}
	return g
}

// attachBridge makes s the gateway's live session, replacing any older one.
// It refuses (closing s, returning false) once shutdown has begun: the close
// sweep has already run and would never see this session.
func (r *Relay) attachBridge(s *bridgeSession, h *helloMsg, pinned, mismatch string) bool {
	now := r.now()
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		s.conn.close(CloseGoingAway, "relay restarting")
		return false
	}
	g := r.gwLocked(s.gateway)
	old := g.bridge
	g.bridge, g.hello, g.node = s, h, h.Node
	g.connectedAt, g.lastSeen = now, now
	g.pinned, g.mismatch = pinned, mismatch
	r.m.bridgeSessions.Add(1)
	s.conn.sendJSON(welcomeMsg{Type: "welcome", Session: s.id,
		PingIntervalS: int(r.cfg.PingInterval / time.Second), ServerTime: rfc3339(now)})
	if old != nil {
		old.conn.close(CloseReplaced, "replaced by a newer session")
	}
	if l := g.lease; l != nil {
		// The lease survives a session replacement, but only onto a node that
		// can still take it.
		if r.usableLocked(g) {
			s.conn.sendJSON(attachMsg{Type: "attach", Lease: l.ID, Client: l.Client})
			l.holder.Notify(r.gatewayStatusLocked(g))
		} else {
			r.endLeaseLocked(l, r.unusableReasonLocked(g), true)
		}
	}
	r.mu.Unlock()

	if old != nil {
		r.event(s.gateway, "replaced", old.id)
		logging.Warnw(r.ctx, "bridge session replaced (two bridges sharing one token will flap)",
			"gateway", s.gateway, "old_session", old.id, "new_session", s.id)
	}
	r.event(s.gateway, "connect", s.id)
	hc := *h
	r.audit("hello", func(st Storage) error { return st.RecordHello(s.gateway, &hc, now) })
	return true
}

// updateNode applies a node_status from the current session.
func (r *Relay) updateNode(s *bridgeSession, n NodeInfo, pinned, mismatch string) {
	r.mu.Lock()
	g := r.gws[s.gateway]
	if g == nil || g.bridge != s {
		r.mu.Unlock()
		return
	}
	changed := g.node.State != n.State
	g.node, g.lastSeen = n, r.now()
	g.pinned, g.mismatch = pinned, mismatch
	if l := g.lease; l != nil {
		l.holder.Notify(r.gatewayStatusLocked(g))
		switch {
		case n.State == NodePaused:
			// Pausing is the host's right: the lease ends at once, and new
			// clients get node_paused until the bridge reports connected.
			r.endLeaseLocked(l, ReasonHostPaused, true)
		case mismatch != "" || (n.State == NodeConnected && !r.usableLocked(g)):
			r.endLeaseLocked(l, ReasonPubkeyMismatch, true)
		}
	}
	r.mu.Unlock()
	if changed {
		r.event(s.gateway, "node_"+n.State, "")
	}
}

// usableLocked reports whether a lease may run on g's node: bridge online,
// node connected, and reporting exactly the pinned key.
func (r *Relay) usableLocked(g *gwState) bool {
	if g.bridge == nil || g.mismatch != "" || g.node.State != NodeConnected || g.pinned == "" {
		return false
	}
	pk, _ := NormalizePubkey(g.node.Pubkey)
	return pk == g.pinned
}

func (r *Relay) unusableReasonLocked(g *gwState) string {
	switch {
	case g.bridge == nil:
		return ReasonGatewayOffline
	case g.mismatch != "":
		return ReasonPubkeyMismatch
	case g.node.State == NodePaused:
		return ReasonHostPaused
	case g.node.State != NodeConnected:
		return ReasonGatewayOffline
	default:
		return ReasonPubkeyMismatch
	}
}

// detachBridge clears s if it is still the gateway's session, ending any
// lease. It reports whether s was current (a replaced session is not).
func (r *Relay) detachBridge(s *bridgeSession) bool {
	now := r.now()
	r.mu.Lock()
	g := r.gws[s.gateway]
	current := g != nil && g.bridge == s
	if current {
		g.bridge, g.lastSeen = nil, now
		if l := g.lease; l != nil {
			l.holder.Notify(r.gatewayStatusLocked(g))
			r.endLeaseLocked(l, ReasonGatewayOffline, true)
		}
	}
	r.mu.Unlock()
	if current {
		r.audit("last seen", func(st Storage) error { return st.RecordSeen(s.gateway, now) })
	}
	return current
}

// closeBridge closes a gateway's live session, if any.
func (r *Relay) closeBridge(gateway string, code int, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gws[gateway]
	if g == nil || g.bridge == nil {
		return false
	}
	g.bridge.conn.close(code, text)
	return true
}

func closeReason(err error) string {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return fmt.Sprintf("close %d", ce.Code)
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return "ping timeout"
	}
	return "connection lost"
}
