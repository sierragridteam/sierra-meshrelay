// Package bridge joins a node link to the relay link.
//
// The two links are independent: the relay connection stays up while the node
// is missing (the relay sees "bridge online, node offline"), and the node link
// keeps retrying while the relay is unreachable. Frames pass through unchanged,
// except the few package nodepolicy refuses (secrets and firmware updates);
// otherwise the only companion parsing is the identity check at node link-up.
//
// Never log frame payloads. They can carry private messages and channel keys.
package bridge

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/config"
	"github.com/sierragridteam/sierra-meshrelay/internal/diaglog"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/internal/state"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Options configures a Bridge. Zero durations take defaults.
type Options struct {
	Config  config.Config
	Opener  nodelink.Opener
	Log     *slog.Logger
	Version string

	// LinkKind and LinkDetail describe the configured link for hello while no
	// node link is open.
	LinkKind, LinkDetail string

	IdentifyTimeout time.Duration // per identity-check reply (default 5s)
	NodeRetryMin    time.Duration // node reopen backoff start (default 1s)
	NodeRetryMax    time.Duration // node reopen backoff cap (default 30s)
	MismatchRetry   time.Duration // recheck interval after a pubkey mismatch (default 30s)
	StatsInterval   time.Duration // stats message interval (default 60s)
	HelloTimeout    time.Duration // wait for welcome (default 10s)
	OutQueue        int           // node→relay frame buffer (default 64)
	Backoff         *Backoff      // relay reconnect policy (default DefaultBackoff)

	// State persists what must survive a restart (a pause "until resumed").
	// Nil keeps it in memory only.
	State *state.File

	// Diag holds recent log lines to send to the relay (`log` messages), so
	// SIERRA admins can see why a bridge is stuck. Nil sends none.
	Diag *diaglog.Ring

	// OnUpdate is called (on its own goroutine) when the relay offers a
	// bridge update. Nil only logs it.
	OnUpdate func(meshproto.UpdateAvailable)
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.IdentifyTimeout, 5*time.Second)
	def(&o.NodeRetryMin, time.Second)
	def(&o.NodeRetryMax, 30*time.Second)
	def(&o.MismatchRetry, 30*time.Second)
	def(&o.StatsInterval, time.Minute)
	def(&o.HelloTimeout, 10*time.Second)
	if o.OutQueue == 0 {
		o.OutQueue = 64
	}
	if o.Backoff == nil {
		o.Backoff = DefaultBackoff()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.LinkKind == "" {
		o.LinkKind = meshproto.LinkTCP
	}
}

// Bridge runs the node and relay links.
type Bridge struct {
	opts    Options
	log     *slog.Logger
	started time.Time

	mu          sync.Mutex
	node        meshproto.Node
	linkKind    string
	linkDetail  string
	nodeVer     uint64
	nodeChanged chan struct{} // closed and replaced on every node change
	link        nodelink.Link // open, identity-checked link; nil otherwise
	session     *session      // established relay session; nil otherwise

	relink chan struct{}
	pause  pauseState // guarded by mu

	// The current node outage, for its log lines (runNode's goroutine only).
	downSince, downLogged time.Time
	downTries             int

	relayClass  Class        // how the last relay session ended (guarded by mu)
	lastTraffic atomic.Int64 // unix nanos of the last frame forwarded either way

	nodeOpens       atomic.Int64
	relayWelcomes   atomic.Int64
	framesToNode    atomic.Int64
	framesFromNode  atomic.Int64
	droppedFromNode atomic.Int64
	droppedToNode   atomic.Int64
	refusedFrames   atomic.Int64  // nodepolicy, either direction
	diagSent        atomic.Uint64 // last log seq the relay has
}

// New creates a Bridge.
func New(opts Options) *Bridge {
	opts.defaults()
	b := &Bridge{
		opts:        opts,
		log:         opts.Log,
		node:        meshproto.Node{State: meshproto.NodeDisconnected},
		linkKind:    opts.LinkKind,
		linkDetail:  opts.LinkDetail,
		nodeChanged: make(chan struct{}),
		relink:      make(chan struct{}, 1),
		pause:       pauseState{changed: make(chan struct{})},
		relayClass:  ClassRetry,
	}
	b.loadState()
	return b
}

// Run runs both links until ctx ends.
func (b *Bridge) Run(ctx context.Context) error {
	b.started = time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.runNode(ctx) }()
	go func() { defer wg.Done(); b.runRelay(ctx) }()
	wg.Wait()
	return ctx.Err()
}

// Relink closes and reopens the node link.
func (b *Bridge) Relink() {
	select {
	case b.relink <- struct{}{}:
	default:
	}
}

// Status is a snapshot of the bridge's state.
type Status struct {
	Node            meshproto.Node
	LinkKind        string
	LinkDetail      string
	RelayConnected  bool
	Attached        bool
	FramesToNode    int64
	FramesFromNode  int64
	DroppedFromNode int64
	DroppedToNode   int64
	// RefusedFrames counts frames nodepolicy stopped, either way. They are
	// not in FramesToNode, FramesFromNode or the Dropped counts.
	RefusedFrames int64

	Paused     bool
	PauseUntil time.Time // zero while paused: until resumed
	// LastTraffic is when a frame last passed between a client and the node
	// (zero: never).
	LastTraffic time.Time
	// RelayClass is how the last relay session ended; ClassAuth means the
	// relay rejected this bridge's credentials.
	RelayClass Class
}

// Status returns the current state.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{
		Node: b.node, LinkKind: b.linkKind, LinkDetail: b.linkDetail,
		RelayConnected:  b.session != nil,
		FramesToNode:    b.framesToNode.Load(),
		FramesFromNode:  b.framesFromNode.Load(),
		DroppedFromNode: b.droppedFromNode.Load(),
		DroppedToNode:   b.droppedToNode.Load(),
		RefusedFrames:   b.refusedFrames.Load(),
		Paused:          b.pause.paused,
		PauseUntil:      b.pause.until,
		RelayClass:      b.relayClass,
	}
	if t := b.lastTraffic.Load(); t != 0 {
		st.LastTraffic = time.Unix(0, t)
	}
	if b.session != nil {
		st.Attached = b.session.isAttached()
	}
	return st
}

// setNode records a node change and wakes anyone waiting on it.
func (b *Bridge) setNode(n meshproto.Node, link nodelink.Link) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.link = link
	if link != nil {
		b.linkKind, b.linkDetail = link.Kind(), link.Detail()
	}
	b.setNodeLocked(n)
}

func (b *Bridge) setNodeLocked(n meshproto.Node) {
	if n == b.node {
		return
	}
	b.node = n
	b.nodeVer++
	close(b.nodeChanged)
	b.nodeChanged = make(chan struct{})
}

// nodeSnapshot returns the node state, its version and a channel closed on the
// next change.
func (b *Bridge) nodeSnapshot() (meshproto.Node, meshproto.Link, uint64, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.node, meshproto.Link{Kind: b.linkKind, Detail: b.linkDetail}, b.nodeVer, b.nodeChanged
}

func (b *Bridge) currentLink() nodelink.Link {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.link
}

func (b *Bridge) currentSession() *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

func (b *Bridge) clearSession(s *session) {
	b.mu.Lock()
	if b.session == s {
		b.session = nil
	}
	b.mu.Unlock()
}

func (b *Bridge) agent() meshproto.Agent {
	return meshproto.Agent{Name: "meshbridge", Version: b.opts.Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// sleep waits d or until ctx ends, reporting whether the full wait happened.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Idle reports whether no client is using the node.
func (b *Bridge) Idle() bool {
	b.mu.Lock()
	s := b.session
	b.mu.Unlock()
	return s == nil || !s.isAttached()
}
