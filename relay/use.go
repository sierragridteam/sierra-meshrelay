package relay

import (
	"context"
	"errors"

	"github.com/sierragridteam/sierra-meshrelay/meshops"
)

// sessionHolder is a LeaseHolder that feeds a meshops.Session.
type sessionHolder struct {
	s      *meshops.Session
	cancel context.CancelCauseFunc
}

func (h *sessionHolder) Deliver(frame []byte) { h.s.Deliver(frame) }
func (h *sessionHolder) Notify(any)           {}
func (h *sessionHolder) End(reason string) {
	h.s.End()
	h.cancel(&LeaseEndedError{Reason: reason})
}

// LeaseEndedError says why a lease used by Use ended early.
type LeaseEndedError struct{ Reason string }

func (e *LeaseEndedError) Error() string { return "lease ended: " + e.Reason }
func (e *LeaseEndedError) Is(t error) bool {
	return t == ErrLeaseEnded
}

// Use holds a lease on req.Gateway while fn runs companion operations on its
// node, then releases it. req.Holder is ignored. Use returns Acquire's error
// (ErrBusy, *UnavailableError) if it can't get the lease, and an error
// matching ErrLeaseEnded if the lease ends while fn runs (preempted by a
// higher priority, the host paused, the bridge went away).
func (r *Relay) Use(ctx context.Context, req LeaseRequest, fn func(context.Context, *meshops.Session) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	h := &sessionHolder{cancel: cancel}
	var lease *Lease
	h.s = meshops.NewSession(func(f []byte) error {
		if lease == nil {
			return ErrLeaseEnded
		}
		return r.Send(lease, f)
	})
	req.Holder = h
	l, err := r.Acquire(req)
	if err != nil {
		return err
	}
	lease = l
	defer r.Release(l)
	err = fn(ctx, h.s)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if errors.Is(err, meshops.ErrSessionEnded) {
		return ErrLeaseEnded
	}
	return err
}
