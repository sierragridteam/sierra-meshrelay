package relay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-meshrelay/nodepolicy"
)

// Config tunes the relay. Zero values take the defaults in withDefaults.
type Config struct {
	// LeaseDuration is how long a lease lasts before it must be renewed (30m).
	LeaseDuration time.Duration
	// LeaseIdleTimeout ends a lease with no frames in either direction (10m).
	LeaseIdleTimeout time.Duration
	// LeaseWarning is how far ahead of expiry the holder gets lease_ending (60s).
	LeaseWarning time.Duration
	// MaxLeaseTotal caps how long renewals may extend one lease (8h), so a
	// forgotten agent that keeps a chatty session alive cannot squat a gateway
	// forever. Negative disables the cap.
	MaxLeaseTotal time.Duration
	// HelloTimeout is how long a bridge has to send hello (10s).
	HelloTimeout time.Duration
	// PingInterval is the websocket ping cadence, and is sent in welcome (25s).
	PingInterval time.Duration
	// FrameRate / FrameBurst bound messages per connection (50/s, burst 100).
	FrameRate  float64
	FrameBurst float64
	// AuthFailLimit failures from one IP within AuthFailWindow lock it out for
	// AuthLockout (10 in 10m -> 15m).
	AuthFailLimit  int
	AuthFailWindow time.Duration
	AuthLockout    time.Duration
	// ClientIPHeader, when set, names the header carrying the real client IP
	// behind the reverse proxy ("X-Forwarded-For" uses its rightmost entry —
	// the one the proxy appended). Empty uses the socket's remote address.
	ClientIPHeader string
	// BootstrapAdmins are config-defined admin tokens (name -> SHA-256 hex),
	// from meshRelay.admins.
	// They exist so there is an admin before the clients table has any rows,
	// and they cannot be revoked through the API.
	BootstrapAdmins map[string]string
	// DebugPayloadGateways log frame payloads for the named gateways. Off by
	// default; every entry is announced at startup. Never leave one on.
	DebugPayloadGateways []string
	// TickInterval drives lease expiry checks (1s).
	TickInterval time.Duration
	// SessionDrainTimeout is how long shutdown waits for every websocket
	// handler to return after closing them (5s). A handoff counts sessions
	// still open past it as a failed release.
	SessionDrainTimeout time.Duration
	// PublicBaseURL is the relay's public origin (https://data.sierragridteam.org),
	// used to build the pairing verification URL and the relay URL handed to a
	// newly paired bridge.
	PublicBaseURL string
	// Pairing tunes bridge pairing; zero values take defaults.
	Pairing PairingConfig
	// Gate admits requests to every relay route. A deploy handoff closes it
	// to stop admitting new work (sierra-data passes its handoff gate). Nil
	// means always open.
	Gate Gate
	// ReleaseBaseURL is where bridges download updates from (default
	// DefaultReleaseBaseURL); the offer's URL is <base>/<version>.
	ReleaseBaseURL string
	// SecretKey (32 bytes) seals secrets the relay must be able to read back:
	// shared repeaters' passwords. Without it, repeaters can't be shared.
	SecretKey []byte
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.LeaseDuration, 30*time.Minute)
	def(&c.LeaseIdleTimeout, 10*time.Minute)
	def(&c.LeaseWarning, 60*time.Second)
	def(&c.MaxLeaseTotal, 8*time.Hour)
	def(&c.HelloTimeout, 10*time.Second)
	def(&c.PingInterval, 25*time.Second)
	def(&c.AuthFailWindow, 10*time.Minute)
	def(&c.AuthLockout, 15*time.Minute)
	def(&c.TickInterval, time.Second)
	def(&c.SessionDrainTimeout, 5*time.Second)
	if c.FrameRate == 0 {
		c.FrameRate = 50
	}
	if c.FrameBurst == 0 {
		c.FrameBurst = 2 * c.FrameRate
	}
	if c.AuthFailLimit == 0 {
		c.AuthFailLimit = 10
	}
	if c.PublicBaseURL == "" {
		c.PublicBaseURL = "https://data.sierragridteam.org"
	}
	c.Pairing = c.Pairing.withDefaults()
	return c
}

// Relay is the live state: one entry per gateway with a bridge attached or a
// lease held.
type Relay struct {
	cfg   Config
	store Storage
	ctx   context.Context // carries the prefab logger
	now   func() time.Time

	mu  sync.Mutex
	gws map[string]*gwState

	// pubkeyMu serializes pin/mismatch decisions (bridge hello, node_status,
	// accept-pubkey), which read and write the store outside mu.
	pubkeyMu sync.Mutex

	// touchMu guards touched: when each client's last use was last recorded
	// (TouchClient is written at most once per touchEvery).
	touchMu sync.Mutex
	touched map[string]time.Time

	auditCh     chan func(Storage) error
	auditWG     sync.WaitGroup
	auditMu     sync.RWMutex // guards auditClosed against a send on the closed channel
	auditClosed bool
	// sessions counts live websocket handlers, so shutdown can let them record
	// their disconnects before the audit writer stops. liveSessions is the
	// same count, readable.
	sessions     sync.WaitGroup
	liveSessions atomic.Int64
	// stopping (guarded by mu) refuses new leases and bridge attachments once
	// shutdown has begun, so nothing slips in behind the close sweep.
	stopping bool
	// gate admits the relay's routes; closing it is how a handoff stops
	// admitting. Config.Gate, or an always-open gate.
	gate Gate

	lockout *ipLockout
	pairs   *pairingState

	logMu      sync.Mutex
	logPending map[string]*pendingLogs // bridge log lines waiting to be written
	debug      map[string]bool
	m          metrics

	lifeMu  sync.Mutex         // guards cancel and halted: Init runs on prefab's goroutine, Stop on the handoff's
	cancel  context.CancelFunc // stops Run; set by Init
	halted  bool               // Stop ran; a late Init must not start Run
	stopped chan struct{}      // closed when Run returns
}

// PluginName is the relay's prefab plugin name.
const PluginName = "meshrelay"

var (
	_ prefab.Plugin              = (*Relay)(nil)
	_ prefab.OptionProvider      = (*Relay)(nil)
	_ prefab.InitializablePlugin = (*Relay)(nil)
	_ prefab.ShutdownPlugin      = (*Relay)(nil)
)

// Name implements prefab.Plugin.
func (r *Relay) Name() string { return PluginName }

// Init implements prefab.InitializablePlugin: prefab starts the relay's
// background loop (audit writer, lease ticker, metrics) with the server.
func (r *Relay) Init(ctx context.Context, _ *prefab.Registry) error {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.lifeMu.Lock()
	if r.halted {
		// Stopped before prefab got round to Init (a cancelled activation):
		// the store may already be closed, so the loop must never start.
		r.lifeMu.Unlock()
		cancel()
		close(r.stopped)
		return nil
	}
	r.cancel = cancel
	r.lifeMu.Unlock()
	go func() {
		defer close(r.stopped)
		r.Run(runCtx)
	}()
	return nil
}

// Shutdown implements prefab.ShutdownPlugin. prefab's HTTP drain cannot see
// hijacked websockets, so this is what closes them: every lease ends with
// relay_shutdown, every bridge gets 1001, and the audit queue is flushed.
func (r *Relay) Shutdown(ctx context.Context) error {
	cancel := r.runCancel()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop is the handoff's form of Shutdown: it waits for the loop to finish
// (sessions closed, audit queue flushed and stopped) and reports any session
// that outlived SessionDrainTimeout as an error, because a session still
// running is a possible store user. Idempotent, and safe alongside prefab's
// own Shutdown on SIGTERM. A relay whose loop never started has nothing to
// stop.
func (r *Relay) Stop(ctx context.Context) error {
	r.lifeMu.Lock()
	r.halted = true
	cancel := r.cancel
	r.lifeMu.Unlock()
	if cancel == nil {
		return nil // never started, and now never will
	}
	cancel()
	select {
	case <-r.stopped:
	case <-ctx.Done():
		return fmt.Errorf("meshrelay: stop: %w", ctx.Err())
	}
	if n := r.liveSessions.Load(); n > 0 {
		return fmt.Errorf("meshrelay: %d websocket session(s) still open after %s", n, r.cfg.SessionDrainTimeout)
	}
	return nil
}

// Gate is the admission gate in front of every relay route.
func (r *Relay) Gate() Gate { return r.gate }

func (r *Relay) runCancel() context.CancelFunc {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	return r.cancel
}

// LiveSessions is the number of websocket handlers still running.
func (r *Relay) LiveSessions() int64 { return r.liveSessions.Load() }

// sessionStart / sessionEnd bracket a websocket handler.
func (r *Relay) sessionStart() { r.sessions.Add(1); r.liveSessions.Add(1) }
func (r *Relay) sessionEnd()   { r.liveSessions.Add(-1); r.sessions.Done() }

// scoped returns ctx with a logger carrying the given fields.
func scoped(ctx context.Context, kv ...any) context.Context {
	l := logging.FromContext(ctx)
	for i := 0; i+1 < len(kv); i += 2 {
		l = l.With(kv[i].(string), kv[i+1])
	}
	return logging.With(ctx, l)
}

// gwState is a gateway's live state. Guarded by Relay.mu.
type gwState struct {
	name        string
	bridge      *bridgeSession
	lease       *Lease
	hello       *helloMsg
	node        NodeInfo
	connectedAt time.Time
	lastSeen    time.Time
	stats       *Stats
	statsAt     time.Time
	// pinned / mismatch mirror the store row, refreshed on every attach and by
	// the admin API. They are the lease gate, so they must never be stale-open.
	pinned   string
	mismatch string
}

type metrics struct {
	framesToNode   atomic.Int64
	framesFromNode atomic.Int64
	framesDropped  atomic.Int64
	framesRefused  atomic.Int64 // nodepolicy: either direction
	authFailures   atomic.Int64
	bridgeSessions atomic.Int64 // total accepted
	leases         atomic.Int64 // total granted
	clientConns    atomic.Int64 // currently open client sockets
}

// New builds a relay over an open store (the SQLite *Store from OpenStore, or
// another Storage such as relay/dynamostore). ctx carries the logger.
func New(ctx context.Context, cfg Config, st Storage) *Relay {
	cfg = cfg.withDefaults()
	r := &Relay{
		cfg:     cfg,
		store:   st,
		ctx:     logging.EnsureLogger(ctx),
		stopped: make(chan struct{}),
		now:     time.Now,
		gws:     map[string]*gwState{},
		auditCh: make(chan func(Storage) error, 1024),
		lockout: newIPLockout(cfg.AuthFailLimit, cfg.AuthFailWindow, cfg.AuthLockout),
		pairs:   newPairingState(cfg.Pairing),
		debug:   map[string]bool{},
		gate:    cfg.Gate,
	}
	if r.gate == nil {
		r.gate = openGate{}
	}
	for _, g := range cfg.DebugPayloadGateways {
		r.debug[g] = true
		logging.Warnw(ctx, "PAYLOAD DEBUG LOGGING ENABLED for gateway: frames (which may hold private messages and channel keys) will be logged",
			"gateway", g)
	}
	return r
}

// Run drives the audit writer, lease ticker and metrics log until ctx ends,
// then closes every session.
func (r *Relay) Run(ctx context.Context) {
	if n, err := r.store.CloseOrphanLeases(r.now()); err != nil {
		logging.Errorw(r.ctx, "closing orphan leases", "error", err)
	} else if n > 0 {
		logging.Infow(r.ctx, "closed leases left open by the previous process", "count", n)
	}
	r.auditWG.Add(1)
	go r.auditLoop()

	tick := time.NewTicker(r.cfg.TickInterval)
	defer tick.Stop()
	metricsTick := time.NewTicker(time.Minute)
	defer metricsTick.Stop()
	var prev MetricsSnapshot
	for {
		select {
		case <-ctx.Done():
			start := time.Now()
			r.shutdown()
			// Sessions record their disconnects through the audit queue, so
			// the writer stops only after they have returned (or the bound).
			waitTimeout(&r.sessions, r.cfg.SessionDrainTimeout)
			sessionsDone := time.Now()
			r.auditMu.Lock()
			r.auditClosed = true
			close(r.auditCh)
			r.auditMu.Unlock()
			r.auditWG.Wait()
			logging.Infow(r.ctx, "Relay sessions closed and audit queue flushed",
				"sessionDrain", sessionsDone.Sub(start), "auditFlush", time.Since(sessionsDone),
				"sessionsStillOpen", r.liveSessions.Load())
			return
		case <-tick.C:
			r.tickLeases()
			r.lockout.prune(r.now())
		case <-metricsTick.C:
			cur := r.Metrics()
			logging.Infow(r.ctx, "relay metrics",
				"gateways_online", cur.GatewaysOnline, "leases_active", cur.LeasesActive,
				"client_conns", cur.ClientConns,
				"frames_to_node_per_min", cur.FramesToNode-prev.FramesToNode,
				"frames_from_node_per_min", cur.FramesFromNode-prev.FramesFromNode,
				"frames_dropped_per_min", cur.FramesDropped-prev.FramesDropped,
				"frames_refused_per_min", cur.FramesRefused-prev.FramesRefused,
				"auth_failures_per_min", cur.AuthFailures-prev.AuthFailures)
			prev = cur
		}
	}
}

// audit queues a store write off the hot path. Writes run in order on one
// goroutine; a full queue drops the write (logged) rather than stall a frame.
func (r *Relay) audit(what string, fn func(Storage) error) {
	wrapped := func(s Storage) error {
		if err := fn(s); err != nil {
			logging.Errorw(r.ctx, "relay audit write failed", "what", what, "error", err)
		}
		return nil
	}
	r.auditMu.RLock()
	defer r.auditMu.RUnlock()
	if r.auditClosed {
		logging.Warnw(r.ctx, "relay audit write after shutdown; dropping", "what", what)
		return
	}
	select {
	case r.auditCh <- wrapped:
	default:
		logging.Errorw(r.ctx, "relay audit queue full; dropping write", "what", what)
	}
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(d):
	}
}

func (r *Relay) auditLoop() {
	defer r.auditWG.Done()
	for fn := range r.auditCh {
		_ = fn(r.store)
	}
}

func (r *Relay) event(gw, kind, detail string) {
	e := GatewayEvent{At: r.now(), Kind: kind, Detail: detail}
	r.audit("gateway event", func(s Storage) error { return s.AddGatewayEvent(gw, e) })
}

func (r *Relay) shutdown() {
	r.mu.Lock()
	r.stopping = true
	var leases []*Lease
	var bridges []*bridgeSession
	for _, g := range r.gws {
		if g.lease != nil {
			leases = append(leases, g.lease)
		}
		if g.bridge != nil {
			bridges = append(bridges, g.bridge)
		}
	}
	for _, l := range leases {
		r.endLeaseLocked(l, ReasonShutdown, true)
	}
	r.mu.Unlock()
	for _, b := range bridges {
		b.conn.close(CloseGoingAway, "relay restarting")
	}
}

// --- leases -----------------------------------------------------------------

// LeaseHolder is whoever holds a lease: an external websocket client, or a
// holder inside the relay's process (an internal poller), which sends frames
// with Relay.Send. Methods are called with the relay mutex
// held, so they must not block or call back into the relay.
type LeaseHolder interface {
	// Deliver hands over one frame from the node.
	Deliver(frame []byte)
	// Notify sends a control message (lease, lease_ending, gateway_status).
	Notify(msg any)
	// End tells the holder its lease is over; it must disconnect.
	End(reason string)
}

// Lease is one exclusive session on a gateway's node.
type Lease struct {
	ID       string
	Gateway  string
	Client   string
	Role     Role
	Priority int
	holder   LeaseHolder

	started   time.Time
	expires   time.Time // guarded by Relay.mu
	warned    bool      // guarded by Relay.mu
	ended     bool      // guarded by Relay.mu
	lastFrame atomic.Int64

	framesTo, framesFrom, bytesTo, bytesFrom atomic.Int64
}

// ExpiresAt is the lease's current expiry.
func (l *Lease) ExpiresAt() time.Time { return l.expires }

func (l *Lease) touch(now time.Time) { l.lastFrame.Store(now.UnixNano()) }

// Priority levels. A higher priority preempts a lower one, so an internal
// poller (PriorityBackground) yields to any human or agent session.
const (
	PriorityBackground = -10
	PriorityNormal     = 0
)

// LeaseRequest asks for a lease.
type LeaseRequest struct {
	Gateway  string
	Client   string
	Role     Role
	Priority int
	// Preempt takes the lease from its current holder regardless of priority
	// (admins only; the caller checks).
	Preempt bool
	Holder  LeaseHolder
}

// ErrBusy means another holder has the gateway.
var ErrBusy = errors.New("gateway busy")

// UnavailableError means the gateway cannot take a lease right now; Status is
// what to tell the client.
type UnavailableError struct {
	Status gatewayStatusMsg
	Code   string
}

func (e *UnavailableError) Error() string { return "gateway unavailable: " + e.Code }

func (r *Relay) gatewayStatusLocked(g *gwState) gatewayStatusMsg {
	st := gatewayStatusMsg{Type: "gateway_status", Bridge: "offline", Node: &NodeInfo{State: NodeDisconnected}}
	if g != nil && g.bridge != nil {
		st.Bridge = "online"
		n := g.node
		if g.mismatch != "" {
			n.State = NodeMismatch
		}
		st.Node = &n
	}
	return st
}

// Acquire grants a lease, or explains why not.
func (r *Relay) Acquire(req LeaseRequest) (*Lease, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gws[req.Gateway]
	if r.stopping {
		return nil, &UnavailableError{Status: r.gatewayStatusLocked(g), Code: ReasonShutdown}
	}
	if g == nil || g.bridge == nil {
		return nil, &UnavailableError{Status: r.gatewayStatusLocked(g), Code: "gateway_offline"}
	}
	if !r.usableLocked(g) {
		code := "pubkey_unverified"
		switch {
		case g.mismatch != "":
			code = "pubkey_mismatch"
		case g.node.State != NodeConnected:
			code = "node_" + g.node.State
		}
		return nil, &UnavailableError{Status: r.gatewayStatusLocked(g), Code: code}
	}
	if cur := g.lease; cur != nil {
		if !req.Preempt && req.Priority <= cur.Priority {
			return nil, ErrBusy
		}
		r.endLeaseLocked(cur, ReasonPreempted, true)
	}
	l := &Lease{
		ID: randomID("l_"), Gateway: req.Gateway, Client: req.Client, Role: req.Role,
		Priority: req.Priority, holder: req.Holder, started: now, expires: now.Add(r.cfg.LeaseDuration),
	}
	l.touch(now)
	g.lease = l
	r.m.leases.Add(1)
	g.bridge.conn.sendJSON(attachMsg{Type: "attach", Lease: l.ID, Client: l.Client})
	rec := LeaseRecord{ID: l.ID, Gateway: l.Gateway, Client: l.Client, Role: l.Role, StartedAt: now}
	r.audit("lease start", func(s Storage) error { return s.InsertLease(rec) })
	logging.Infow(r.ctx, "lease granted", "gateway", l.Gateway, "lease", l.ID, "client", l.Client, "role", l.Role,
		"preempt", req.Preempt)
	l.holder.Notify(r.leaseMsgLocked(g, l))
	return l, nil
}

func (r *Relay) leaseMsgLocked(g *gwState, l *Lease) leaseMsg {
	n := g.node
	return leaseMsg{Type: "lease", Lease: l.ID, Gateway: l.Gateway, ExpiresAt: rfc3339(l.expires), Node: &n}
}

// Renew extends a lease. It answers with a new lease message, or an error.
func (r *Relay) Renew(l *Lease) error {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gws[l.Gateway]
	if l.ended || g == nil || g.lease != l {
		return errors.New("lease is not active")
	}
	next := now.Add(r.cfg.LeaseDuration)
	if r.cfg.MaxLeaseTotal > 0 {
		limit := l.started.Add(r.cfg.MaxLeaseTotal)
		if !next.Before(limit) {
			next = limit
		}
		if !next.After(l.expires) {
			return errors.New("lease has reached its maximum length")
		}
	}
	l.expires, l.warned = next, false
	l.holder.Notify(r.leaseMsgLocked(g, l))
	return nil
}

// Release ends a lease because its holder went away. A no-op if the relay
// already ended it.
func (r *Relay) Release(l *Lease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLeaseLocked(l, reasonClientClosed, false)
}

// endLeaseLocked ends l. With notify, the holder gets lease_ending (in_s 0)
// and is told to disconnect.
func (r *Relay) endLeaseLocked(l *Lease, reason string, notify bool) {
	if l.ended {
		return
	}
	l.ended = true
	now := r.now()
	if g := r.gws[l.Gateway]; g != nil && g.lease == l {
		g.lease = nil
		if g.bridge != nil {
			g.bridge.conn.sendJSON(detachMsg{Type: "detach", Lease: l.ID})
		}
	}
	if notify {
		l.holder.Notify(leaseEndingMsg{Type: "lease_ending", Lease: l.ID, InS: 0, Reason: reason})
		l.holder.End(reason)
	}
	rec := LeaseRecord{
		ID: l.ID, Gateway: l.Gateway, Client: l.Client, Role: l.Role, StartedAt: l.started, EndedAt: &now, EndReason: reason,
		FramesToNode: l.framesTo.Load(), FramesFromNode: l.framesFrom.Load(),
		BytesToNode: l.bytesTo.Load(), BytesFromNode: l.bytesFrom.Load(),
	}
	r.audit("lease end", func(s Storage) error { return s.EndLease(rec) })
	logging.Infow(r.ctx, "lease ended", "gateway", l.Gateway, "lease", l.ID, "client", l.Client, "reason", reason,
		"duration_s", int(now.Sub(l.started).Seconds()),
		"frames_to_node", rec.FramesToNode, "frames_from_node", rec.FramesFromNode)
}

// tickLeases enforces expiry, the expiry warning and the idle timeout.
func (r *Relay) tickLeases() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.gws {
		l := g.lease
		if l == nil {
			continue
		}
		idle := now.Sub(time.Unix(0, l.lastFrame.Load()))
		switch {
		case idle >= r.cfg.LeaseIdleTimeout:
			r.endLeaseLocked(l, ReasonIdle, true)
		case !now.Before(l.expires):
			r.endLeaseLocked(l, ReasonExpiry, true)
		case !l.warned && l.expires.Sub(now) <= r.cfg.LeaseWarning:
			l.warned = true
			l.holder.Notify(leaseEndingMsg{Type: "lease_ending", Lease: l.ID,
				InS: int(l.expires.Sub(now).Round(time.Second).Seconds()), Reason: ReasonExpiry})
		}
	}
}

// EndLeasesFor ends every lease a client holds (token revoked).
func (r *Relay) EndLeasesFor(client, reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, g := range r.gws {
		if g.lease != nil && g.lease.Client == client {
			r.endLeaseLocked(g.lease, reason, true)
			n++
		}
	}
	return n
}

// --- frames -----------------------------------------------------------------

// ErrLeaseEnded means a frame could not be sent because the lease is no
// longer the gateway's active lease, or its bridge stopped accepting frames.
var ErrLeaseEnded = errors.New("lease ended")

// ErrFrameTooLarge means a frame is over MaxFrameBytes.
var ErrFrameTooLarge = errors.New("frame too large")

// Send forwards one companion frame payload to the node, as a websocket
// client's binary message would be. It is for lease holders inside the relay's
// process (an internal poller), which have no socket to write to. It applies
// the frame size limit but not the per-connection rate limit: the caller paces
// itself. Frames from the node arrive through the holder's Deliver.
func (r *Relay) Send(l *Lease, frame []byte) error {
	if len(frame) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if !r.toNode(l, frame) {
		return ErrLeaseEnded
	}
	return nil
}

// toNode forwards a frame from the lease holder to its gateway's bridge. A
// frame nodepolicy refuses never leaves the relay: the holder gets the
// firmware's "disabled" reply instead, and the lease carries on.
func (r *Relay) toNode(l *Lease, frame []byte) bool {
	r.mu.Lock()
	g := r.gws[l.Gateway]
	if l.ended || g == nil || g.lease != l || g.bridge == nil {
		r.mu.Unlock()
		r.m.framesDropped.Add(1)
		return false
	}
	if reason, ok := nodepolicy.ToNode(frame); !ok {
		l.touch(r.now())
		l.holder.Deliver(nodepolicy.Refusal())
		r.mu.Unlock()
		r.refused(l.Gateway, l.ID, l.Client, "to_node", reason)
		return true
	}
	b := g.bridge
	r.mu.Unlock()
	now := r.now()
	l.touch(now)
	l.framesTo.Add(1)
	l.bytesTo.Add(int64(len(frame)))
	r.m.framesToNode.Add(1)
	r.debugFrame(l.Gateway, "to_node", frame)
	return b.conn.sendBinary(frame)
}

// fromNode forwards a frame from a bridge to the lease holder, if any.
func (r *Relay) fromNode(b *bridgeSession, frame []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gws[b.gateway]
	if g == nil || g.bridge != b || g.lease == nil {
		r.m.framesDropped.Add(1)
		return
	}
	l := g.lease
	if reason, ok := nodepolicy.FromNode(frame); !ok {
		r.refused(b.gateway, l.ID, l.Client, "from_node", reason)
		return
	}
	l.touch(r.now())
	l.framesFrom.Add(1)
	l.bytesFrom.Add(int64(len(frame)))
	r.m.framesFromNode.Add(1)
	r.debugFrame(b.gateway, "from_node", frame)
	l.holder.Deliver(frame)
}

// refused counts and logs a frame nodepolicy stopped. Never the payload,
// even with payload debugging on: the reason names what it was.
func (r *Relay) refused(gw, lease, client, dir, reason string) {
	r.m.framesRefused.Add(1)
	logging.Warnw(r.ctx, "refused a frame by node policy", "gateway", gw, "lease", lease, "client", client,
		"dir", dir, "reason", reason)
}

func (r *Relay) debugFrame(gw, dir string, frame []byte) {
	if r.debug[gw] {
		logging.Debugw(r.ctx, "frame payload (debug)", "gateway", gw, "dir", dir, "len", len(frame), "hex", hexString(frame))
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = digits[c>>4], digits[c&0xf]
	}
	return string(out)
}

// --- status -----------------------------------------------------------------

// MetricsSnapshot is the relay's counters and gauges.
type MetricsSnapshot struct {
	GatewaysOnline int   `json:"gateways_online"`
	BridgeSessions int64 `json:"bridge_sessions_total"`
	LeasesActive   int   `json:"leases_active"`
	LeasesTotal    int64 `json:"leases_total"`
	ClientConns    int64 `json:"client_conns"`
	FramesToNode   int64 `json:"frames_to_node_total"`
	FramesFromNode int64 `json:"frames_from_node_total"`
	FramesDropped  int64 `json:"frames_dropped_total"`
	FramesRefused  int64 `json:"frames_refused_total"`
	AuthFailures   int64 `json:"auth_failures_total"`
	AuthLockedIPs  int   `json:"auth_locked_ips"`
}

// Metrics snapshots the counters.
func (r *Relay) Metrics() MetricsSnapshot {
	m := MetricsSnapshot{
		BridgeSessions: r.m.bridgeSessions.Load(), LeasesTotal: r.m.leases.Load(),
		ClientConns: r.m.clientConns.Load(), FramesToNode: r.m.framesToNode.Load(),
		FramesFromNode: r.m.framesFromNode.Load(), FramesDropped: r.m.framesDropped.Load(),
		FramesRefused: r.m.framesRefused.Load(), AuthFailures: r.m.authFailures.Load(), AuthLockedIPs: r.lockout.locked(r.now()),
	}
	r.mu.Lock()
	for _, g := range r.gws {
		if g.bridge != nil {
			m.GatewaysOnline++
		}
		if g.lease != nil {
			m.LeasesActive++
		}
	}
	r.mu.Unlock()
	return m
}

// --- auth lockout -----------------------------------------------------------

// ipLockout locks out a source IP after repeated authentication failures.
type ipLockout struct {
	limit         int
	window, block time.Duration
	mu            sync.Mutex
	byIP          map[string]*ipFailures
}

type ipFailures struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

func newIPLockout(limit int, window, block time.Duration) *ipLockout {
	return &ipLockout{limit: limit, window: window, block: block, byIP: map[string]*ipFailures{}}
}

func (l *ipLockout) isLocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.byIP[ip]
	return f != nil && now.Before(f.lockedUntil)
}

// fail records a failure and reports whether it locked the IP.
func (l *ipLockout) fail(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.byIP[ip]
	if f == nil || now.Sub(f.windowStart) > l.window {
		f = &ipFailures{windowStart: now}
		l.byIP[ip] = f
	}
	f.count++
	if f.count >= l.limit && !now.Before(f.lockedUntil) {
		f.lockedUntil = now.Add(l.block)
		return true
	}
	return false
}

func (l *ipLockout) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, f := range l.byIP {
		if now.Sub(f.windowStart) > l.window && !now.Before(f.lockedUntil) {
			delete(l.byIP, ip)
		}
	}
}

func (l *ipLockout) locked(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, f := range l.byIP {
		if now.Before(f.lockedUntil) {
			n++
		}
	}
	return n
}

// sortedNames returns map keys in order (stable status output).
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
