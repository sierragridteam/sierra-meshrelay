package relay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/gorilla/websocket"
)

// wsHolder is a lease held by an external websocket client.
type wsHolder struct{ c *conn }

func (h wsHolder) Deliver(frame []byte) { h.c.sendBinary(frame) }
func (h wsHolder) Notify(msg any)       { h.c.sendJSON(msg) }
func (h wsHolder) End(reason string)    { h.c.close(CloseNormal, "lease ended: "+reason) }

// handleClient serves /mesh/relay/{gateway}.
func (r *Relay) handleClient(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("gateway")
	ip := r.clientIP(req)
	if r.lockout.isLocked(ip, r.now()) {
		http.Error(w, "too many authentication failures", http.StatusTooManyRequests)
		return
	}
	who, err := r.authClient(req)
	if errors.Is(err, ErrNotFound) {
		r.authFailed(w, ip, "client", name)
		return
	}
	if errors.Is(err, ErrTokenExpired) {
		logging.Infow(r.ctx, "expired token", "kind", "client", "gateway", name)
		http.Error(w, "this token has expired; ask for a new one", http.StatusUnauthorized)
		return
	}
	if err != nil {
		logging.Errorw(r.ctx, "client auth lookup", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ValidGatewayName(name) {
		http.Error(w, "unknown gateway", http.StatusNotFound)
		return
	}
	rec, err := r.store.Gateway(name)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "unknown gateway", http.StatusNotFound)
		return
	} else if err != nil {
		logging.Errorw(r.ctx, "gateway lookup", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if why := mayLease(who, rec); why != "" {
		logging.Infow(r.ctx, "lease refused: not allowed", "gateway", name, "client", who.Name, "role", who.Role)
		http.Error(w, why, http.StatusForbidden)
		return
	}
	if !hasSubprotocol(req, ClientSubprotocol) {
		http.Error(w, "unsupported subprotocol; want "+ClientSubprotocol, http.StatusUpgradeRequired)
		return
	}
	preempt := isTrue(req.URL.Query().Get("preempt"))
	if preempt && who.Role != RoleAdmin {
		http.Error(w, "only an admin can preempt a lease", http.StatusForbidden)
		return
	}
	ws, err := upgrader.Upgrade(w, req, http.Header{"Sec-WebSocket-Protocol": {ClientSubprotocol}})
	if err != nil {
		return
	}
	r.sessionStart()
	defer r.sessionEnd()
	c := newConn(ws, r.cfg.PingInterval)
	r.m.clientConns.Add(1)
	defer r.m.clientConns.Add(-1)
	r.serveClient(c, name, who, preempt)
}

func (r *Relay) serveClient(c *conn, gateway string, who *ClientRecord, preempt bool) {
	ctx := scoped(r.ctx, "gateway", gateway, "client", who.Name)
	l, err := r.Acquire(LeaseRequest{Gateway: gateway, Client: who.Name, Role: who.Role,
		Priority: PriorityNormal, Preempt: preempt, Holder: wsHolder{c}})
	var unavailable *UnavailableError
	switch {
	case errors.Is(err, ErrBusy):
		c.sendJSON(errorMsg{Type: "error", Code: "busy", Message: "another client holds this gateway's lease"})
		c.close(CloseBusy, "gateway busy")
		logging.Infow(ctx, "lease refused: busy")
		return
	case errors.As(err, &unavailable):
		// Don't hang: say what state the gateway is in, then go.
		c.sendJSON(unavailable.Status)
		c.sendJSON(errorMsg{Type: "error", Code: unavailable.Code})
		c.close(CloseNormal, "gateway unavailable")
		logging.Infow(ctx, "lease refused: unavailable", "code", unavailable.Code)
		return
	case err != nil:
		c.close(1011, "internal error")
		return
	}
	defer r.Release(l)

	limiter := newRateLimiter(r.cfg.FrameRate, r.cfg.FrameBurst)
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			c.close(CloseNormal, "")
			return
		}
		c.extendRead()
		limiter.wait()
		switch kind {
		case websocket.BinaryMessage:
			if len(data) > MaxFrameBytes {
				c.sendJSON(errorMsg{Type: "error", Code: "frame_too_large",
					Message: fmt.Sprintf("max frame is %d bytes", MaxFrameBytes)})
				c.close(CloseProtocolError, "frame too large")
				return
			}
			r.toNode(l, data)
		case websocket.TextMessage:
			var m renewMsg
			if json.Unmarshal(data, &m) != nil || m.Type != "renew" {
				continue // unknown types are ignored
			}
			if m.Lease != l.ID {
				c.sendJSON(errorMsg{Type: "error", Code: "unknown_lease"})
				continue
			}
			if err := r.Renew(l); err != nil {
				c.sendJSON(errorMsg{Type: "error", Code: "renew_denied", Message: err.Error()})
			}
		}
	}
}

// ErrTokenExpired means a client token is genuine but past its expiry. It
// doesn't count toward the IP lockout.
var ErrTokenExpired = errors.New("token expired")

// touchEvery bounds how often a client's last use is written.
const touchEvery = time.Hour

// authClient resolves a client bearer token: config-defined bootstrap admins
// first, then the clients table. A host token must start with
// HostTokenPrefix and any other with ClientTokenPrefix, so a host token
// can't pass as another kind (or be accepted by a relay from before them).
func (r *Relay) authClient(req *http.Request) (*ClientRecord, error) {
	tok := bearerToken(req.Header.Get("Authorization"))
	host := strings.HasPrefix(tok, HostTokenPrefix)
	if !host && !strings.HasPrefix(tok, ClientTokenPrefix) {
		return nil, ErrNotFound
	}
	hash := TokenHash(tok)
	if !host {
		for _, name := range sortedNames(r.cfg.BootstrapAdmins) {
			if subtle.ConstantTimeCompare([]byte(hash), []byte(r.cfg.BootstrapAdmins[name])) == 1 {
				return &ClientRecord{Name: name, Role: RoleAdmin, Bootstrap: true}, nil
			}
		}
	}
	c, err := r.store.ClientByToken(hash)
	if err != nil {
		return nil, err
	}
	if host != (c.Role == RoleHost) {
		return nil, ErrNotFound
	}
	now := r.now()
	if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
		return nil, ErrTokenExpired
	}
	r.touch(c.Name, now)
	return c, nil
}

// touch records a client's use, at most once per touchEvery, off the
// request path.
func (r *Relay) touch(name string, now time.Time) {
	r.touchMu.Lock()
	if r.touched == nil {
		r.touched = map[string]time.Time{}
	}
	last, ok := r.touched[name]
	if ok && now.Sub(last) < touchEvery {
		r.touchMu.Unlock()
		return
	}
	r.touched[name] = now
	r.touchMu.Unlock()
	r.audit("client last use", func(s Storage) error {
		if err := s.TouchClient(name, now); !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	})
}

// mayLease reports why who may not take a lease on gateway (rec), or "" if
// it may. Admins may use any gateway; a host only its own, and only a token
// issued after the gateway was created (a deleted gateway's name can come
// back for someone else).
func mayLease(who *ClientRecord, rec *GatewayRecord) string {
	switch who.Role {
	case RoleAdmin:
		return ""
	case RoleHost:
		if who.Gateway != rec.Name || who.CreatedAt.Before(rec.CreatedAt) {
			return "this token is for gateway " + who.Gateway + ", not " + rec.Name
		}
		return ""
	}
	return retiredRole(who.Role)
}

// retiredRole is the refusal for a token with a role no longer issued.
func retiredRole(role Role) string {
	return "tokens with the " + string(role) + " role no longer work; ask a SIERRA admin for a new one"
}

func (r *Relay) authFailed(w http.ResponseWriter, ip, kind, gateway string) {
	r.m.authFailures.Add(1)
	if r.lockout.fail(ip, r.now()) {
		logging.Warnw(r.ctx, "locking out IP after repeated auth failures", "ip", ip, "for", r.cfg.AuthLockout.String())
	}
	logging.Infow(r.ctx, "auth failed", "kind", kind, "ip", ip, "gateway", gateway)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// clientIP is the request's source address, as the lockout keys it.
func (r *Relay) clientIP(req *http.Request) string {
	if h := r.cfg.ClientIPHeader; h != "" {
		if v := req.Header.Get(h); v != "" {
			if strings.EqualFold(h, "X-Forwarded-For") {
				parts := strings.Split(v, ",")
				return strings.TrimSpace(parts[len(parts)-1])
			}
			return strings.TrimSpace(v)
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func isTrue(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes":
		return true
	}
	return false
}
