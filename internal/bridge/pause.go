package bridge

import (
	"context"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/state"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Pausing is the host's right. A paused bridge closes its node link, which
// frees the node's one companion connection for the host's own app, and
// reports node_status "paused" (with "until" for a timed pause). The relay
// ends any lease and turns new clients away with node_paused.
//
// A timed pause resumes by itself when it runs out, and also on restart. A
// pause "until resumed" is saved to Options.State so it survives a
// restart, and only Resume ends it.

// pauseState is guarded by Bridge.mu.
type pauseState struct {
	paused  bool
	until   time.Time     // zero: until resumed
	changed chan struct{} // closed and replaced on every change
}

// NodeErrResuming is the node error while sharing has just resumed and the
// node link is being reopened.
const NodeErrResuming = "resuming"

// Pause stops sharing the node until until, or until Resume if until is zero.
// A time in the past resumes at once.
func (b *Bridge) Pause(until time.Time) {
	if !until.IsZero() && !until.After(time.Now()) {
		b.Resume()
		return
	}
	b.mu.Lock()
	b.pause.paused, b.pause.until = true, until
	b.pauseChangedLocked()
	b.mu.Unlock()
	b.savePause(until.IsZero())
	if until.IsZero() {
		b.log.Info("sharing paused until resumed")
	} else {
		b.log.Info("sharing paused", "until", until.Format(time.RFC3339))
	}
}

// Resume ends a pause. Resuming an unpaused bridge does nothing.
func (b *Bridge) Resume() {
	b.mu.Lock()
	was := b.pause.paused
	b.pause.paused, b.pause.until = false, time.Time{}
	if was {
		// Leave "paused" before the node loop wakes, so nothing reports a
		// paused node while sharing is on, and the loop's own "connected"
		// can't be overwritten by a late update here.
		if b.node.State == meshproto.NodePaused {
			b.setNodeLocked(meshproto.Node{State: meshproto.NodeDisconnected, Error: NodeErrResuming})
		}
		b.pauseChangedLocked()
	}
	b.mu.Unlock()
	if was {
		b.savePause(false)
		b.log.Info("sharing resumed")
	}
}

// Paused reports whether sharing is paused, and until when (zero: until
// resumed).
func (b *Bridge) Paused() (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pause.paused, b.pause.until
}

func (b *Bridge) pauseChangedLocked() {
	close(b.pause.changed)
	b.pause.changed = make(chan struct{})
}

func (b *Bridge) pauseSnapshot() (bool, time.Time, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pause.paused, b.pause.until, b.pause.changed
}

// waitWhilePaused holds the node loop while sharing is paused, reporting the
// paused state. It returns once sharing resumes or ctx ends.
func (b *Bridge) waitWhilePaused(ctx context.Context) {
	for ctx.Err() == nil {
		paused, until, changed := b.pauseSnapshot()
		if !paused {
			return
		}
		n := meshproto.Node{State: meshproto.NodePaused}
		var expire <-chan time.Time
		if !until.IsZero() {
			n.Until = until.UTC().Format(time.RFC3339)
			d := time.Until(until)
			if d <= 0 {
				b.Resume()
				continue
			}
			t := time.NewTimer(d)
			expire = t.C
			defer t.Stop()
		}
		b.setNode(n, nil)
		select {
		case <-changed:
		case <-expire:
			b.log.Info("pause ran out")
			b.Resume()
		case <-ctx.Done():
		}
	}
}

// loadState applies the saved state at startup.
func (b *Bridge) loadState() {
	if b.opts.State.Get().PausedUntilResumed {
		b.pause.paused = true
		b.log.Info("sharing is paused until resumed (saved from before restart)")
	}
}

func (b *Bridge) savePause(untilResumed bool) {
	if err := b.opts.State.Update(func(s *state.State) { s.PausedUntilResumed = untilResumed }); err != nil {
		b.log.Warn("couldn't save state; a pause until resumed won't survive a restart", "error", err)
	}
}
