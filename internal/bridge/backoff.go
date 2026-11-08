package bridge

import (
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Backoff decides how long to wait before redialling the relay.
//
// Normal failures back off exponentially from Min to Max with ±Jitter, and the
// sequence resets once a session has stayed up for ResetAfter. Auth failures
// back off AuthWait: retrying a bad token quickly would trip the relay's
// per-IP lockout (10 failures in 10 minutes), which locks out every bridge
// behind the same IP. An unsupported protocol waits UpgradeWait.
type Backoff struct {
	Min, Max    time.Duration
	Jitter      float64
	ResetAfter  time.Duration
	AuthWait    time.Duration
	UpgradeWait time.Duration

	attempt int
}

// DefaultBackoff matches the protocol's reconnect rules.
func DefaultBackoff() *Backoff {
	return &Backoff{
		Min: time.Second, Max: time.Minute, Jitter: 0.2,
		ResetAfter: 5 * time.Minute, AuthWait: 15 * time.Minute, UpgradeWait: time.Hour,
	}
}

// SessionResult is how a relay session ended.
type SessionResult struct {
	// HTTPStatus is the upgrade response status when the dial failed, else 0.
	HTTPStatus int
	// CloseCode is the WebSocket close code, or -1 if there wasn't one.
	CloseCode int
	// Welcomed is true if the session got as far as welcome.
	Welcomed bool
	// Uptime is how long the session was established.
	Uptime time.Duration
	Err    error
}

// Class describes why a session ended, for logging and the wait choice.
type Class int

const (
	ClassRetry   Class = iota // transient: back off and retry
	ClassAuth                 // bad token, wrong gateway, or locked out
	ClassUpgrade              // the relay doesn't speak our protocol version
)

// Classify sorts a session result.
func Classify(r SessionResult) Class {
	switch r.HTTPStatus {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		return ClassAuth
	case http.StatusUpgradeRequired:
		return ClassUpgrade
	}
	switch r.CloseCode {
	case meshproto.CloseBadToken, meshproto.CloseGatewayMismatch:
		return ClassAuth
	case meshproto.CloseUnsupported:
		return ClassUpgrade
	}
	return ClassRetry
}

// Next returns the wait before the next dial.
func (b *Backoff) Next(r SessionResult) time.Duration {
	if r.Welcomed && r.Uptime >= b.ResetAfter {
		b.attempt = 0
	}
	switch Classify(r) {
	case ClassAuth:
		return b.AuthWait
	case ClassUpgrade:
		return b.UpgradeWait
	}
	d := b.Min << min(b.attempt, 30)
	if d > b.Max || d <= 0 {
		d = b.Max
	} else {
		b.attempt++
	}
	if b.Jitter > 0 {
		f := 1 + b.Jitter*(2*rand.Float64()-1)
		d = time.Duration(float64(d) * f)
	}
	return d
}
