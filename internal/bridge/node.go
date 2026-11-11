package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
	"github.com/sierragridteam/sierra-meshrelay/nodepolicy"
)

// runNode keeps a verified link to the node open for as long as ctx lives.
func (b *Bridge) runNode(ctx context.Context) {
	delay := b.opts.NodeRetryMin
	backoff := func() {
		b.waitOrRelink(ctx, delay)
		delay = min(delay*2, b.opts.NodeRetryMax)
	}
	for ctx.Err() == nil {
		b.waitWhilePaused(ctx)
		if ctx.Err() != nil {
			return
		}
		link, err := b.opts.Opener.Open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.nodeDown(nodeErrorText(err), err)
			if errors.Is(err, nodelink.ErrNotConfigured) {
				b.waitOrRelink(ctx, b.opts.NodeRetryMax)
				continue
			}
			backoff()
			continue
		}
		if b.nodeOpens.Add(1) > 1 {
			b.log.Debug("node link reopened", "link", link.Detail())
		}
		res := b.serveLink(ctx, link)
		switch res {
		case linkMismatch:
			b.waitOrRelink(ctx, b.opts.MismatchRetry)
		case linkFailedIdentify:
			backoff()
		case linkServed:
			// It was up and verified, so reopen promptly.
			delay = b.opts.NodeRetryMin
			b.waitOrRelink(ctx, delay)
		case linkRelinked, linkPaused:
			delay = b.opts.NodeRetryMin
		}
	}
}

type linkResult int

const (
	linkFailedIdentify linkResult = iota
	linkMismatch
	linkServed
	linkRelinked
	linkPaused
)

// serveLink identity-checks link, then forwards frames until it fails.
func (b *Bridge) serveLink(ctx context.Context, link nodelink.Link) linkResult {
	linkCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	frames := make(chan []byte, b.opts.OutQueue)
	readErr := make(chan error, 1)
	go func() {
		defer close(frames)
		for {
			f, err := link.ReadFrame()
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-linkCtx.Done():
				readErr <- linkCtx.Err()
				return
			}
		}
	}()

	var relinked, paused atomic.Bool
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		defer link.Close()
		for {
			p, _, pauseCh := b.pauseSnapshot()
			if p {
				paused.Store(true)
				b.log.Info("pausing: releasing the node link")
				return
			}
			select {
			case <-linkCtx.Done():
				return
			case <-b.relink:
				relinked.Store(true)
				b.log.Info("relink requested; reopening the node link")
				return
			case <-pauseCh:
			}
		}
	}()
	defer func() { cancel(); <-closed }()

	id, err := companion.Identify(linkCtx, frames, link.WriteFrame, b.opts.IdentifyTimeout)
	if err != nil {
		cancel()
		<-closed
		if relinked.Load() {
			return linkRelinked
		}
		if paused.Load() {
			return linkPaused
		}
		if ctx.Err() != nil {
			return linkFailedIdentify // shutting down
		}
		b.nodeDown("identity check failed", err)
		return linkFailedIdentify
	}
	node := meshproto.Node{State: meshproto.NodeConnected, Pubkey: id.Pubkey, Name: id.Name, FW: id.FW, Model: id.Model}
	if want := b.opts.Config.Node.ExpectedPubkey; want != "" && id.Pubkey != want {
		node.State = meshproto.NodeMismatch
		if prev, _, _, _ := b.nodeSnapshot(); prev != node {
			b.log.Warn("this isn't the node SIERRA expects; contact your SIERRA contact",
				"pubkey", id.Pubkey, "expected", want, "name", id.Name)
		}
		b.setNode(node, nil)
		return linkMismatch
	}
	kv := []any{"link", link.Kind(), "detail", link.Detail(), "name", id.Name, "pubkey", id.Pubkey, "fw", id.FW, "model", id.Model}
	if d := b.nodeUp(); d > 0 {
		kv = append(kv, "unavailable_for", d.String())
	}
	b.log.Info("node connected", kv...)
	b.setNode(node, link)

	for f := range frames {
		b.fromNode(f)
	}
	err = <-readErr
	if ctx.Err() != nil {
		return linkServed // shutting down
	}
	if paused.Load() {
		return linkPaused // waitWhilePaused reports the paused state
	}
	if relinked.Load() {
		b.setNode(meshproto.Node{State: meshproto.NodeDisconnected, Error: "relink"}, nil)
		return linkRelinked
	}
	b.nodeDown(nodeErrorText(err), err)
	return linkServed
}

// nodeDownSummaryEvery is how often a node outage is logged after its start.
const nodeDownSummaryEvery = 15 * time.Minute

// nodeDown records a failed try at the node. An outage is logged when it
// starts, then summarized every nodeDownSummaryEvery: retries come every few
// seconds to a minute, and logging each one filled the log (and the relay's
// copy of it) within hours.
func (b *Bridge) nodeDown(reason string, err error) {
	n := meshproto.Node{State: meshproto.NodeDisconnected, Error: reason}
	now := time.Now()
	if b.downSince.IsZero() {
		b.downSince, b.downLogged, b.downTries = now, now, 0
		b.log.Warn("node unavailable", "reason", reason, "error", err)
	} else if now.Sub(b.downLogged) >= nodeDownSummaryEvery {
		b.downLogged = now
		b.log.Info("node still unavailable", "reason", reason, "error", err,
			"for", now.Sub(b.downSince).Round(time.Second).String(), "tries", b.downTries+1)
	}
	b.downTries++
	b.setNode(n, nil)
}

// nodeUp ends an outage; it returns how long the node was unavailable (0 if
// it wasn't).
func (b *Bridge) nodeUp() time.Duration {
	if b.downSince.IsZero() {
		return 0
	}
	d := time.Since(b.downSince).Round(time.Second)
	b.downSince, b.downTries = time.Time{}, 0
	return d
}

// nodeErrorText turns a link error into a short reason for node_status.
func nodeErrorText(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, nodelink.ErrNodeNotFound):
		return "no node found on this network"
	case errors.Is(err, nodelink.ErrNotConfigured):
		return "no node address configured"
	case errors.Is(err, companion.ErrFrameTooLarge):
		return "node link out of sync"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return "node link closed"
	case errors.As(err, &ne) && ne.Timeout():
		return "node not responding"
	case err != nil:
		return "can't reach node"
	}
	return ""
}

// waitOrRelink waits d, ctx's end, or a relink request (which skips the wait).
func (b *Bridge) waitOrRelink(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	case <-b.relink:
	}
}

// fromNode forwards a frame read from the node to the relay, if a client is
// attached. When one is, it blocks until the relay takes the frame: a slow
// relay slows node reads rather than dropping frames.
func (b *Bridge) fromNode(f []byte) {
	if reason, ok := nodepolicy.FromNode(f); !ok {
		b.refuse("from_node", reason)
		return
	}
	b.framesFromNode.Add(1)
	s := b.currentSession()
	if s == nil || !s.isAttached() {
		b.droppedFromNode.Add(1)
		return
	}
	select {
	case s.out <- f:
		b.lastTraffic.Store(time.Now().UnixNano())
	case <-s.done:
		b.droppedFromNode.Add(1)
	}
}

// refuse counts and logs a frame nodepolicy stopped, never its contents.
func (b *Bridge) refuse(dir, reason string) {
	b.refusedFrames.Add(1)
	b.log.Warn("refused a frame by node policy", "dir", dir, "reason", reason)
}

// toNode writes a frame that arrived on relay session s to the node. Every
// such frame comes from a remote session, so nodepolicy applies to all of
// them: a refused command never reaches the node, and the session gets the
// firmware's "disabled" reply instead. That reply waits for room in the
// session's queue like any node frame, but gives up once the session's I/O
// is cancelled: this runs on the read loop, which teardown waits for.
func (b *Bridge) toNode(s *session, f []byte) {
	if reason, ok := nodepolicy.ToNode(f); !ok {
		b.refuse("to_node", reason)
		select {
		case s.out <- nodepolicy.Refusal():
		case <-s.ioCtx.Done():
		case <-s.done:
		}
		return
	}
	link := b.currentLink()
	if link == nil {
		b.droppedToNode.Add(1)
		return
	}
	if err := link.WriteFrame(f); err != nil {
		b.droppedToNode.Add(1)
		b.log.Warn("writing to node failed; reopening the link", "error", err)
		link.Close()
		return
	}
	b.framesToNode.Add(1)
	b.lastTraffic.Store(time.Now().UnixNano())
}
