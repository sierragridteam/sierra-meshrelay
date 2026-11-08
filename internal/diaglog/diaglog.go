// Package diaglog keeps the bridge's recent log lines (Info and above) so
// the bridge can send them to the relay as `log` messages, where SIERRA
// admins can read them (`meshbridge admin logs <gateway>`). That's how
// someone helping a host sees why a bridge is stuck without reaching the
// host's computer.
//
// The bridge never logs frame payloads, so neither does this. As a second
// guard, attributes whose key sounds secret (token, password, passphrase,
// secret, pin) are redacted, and values are truncated.
package diaglog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Limits on what one entry carries.
const (
	MaxMsg      = 300
	MaxAttrs    = 16
	MaxAttrLen  = 200
	DefaultSize = 300
)

// Ring holds the most recent entries.
type Ring struct {
	mu      sync.Mutex
	buf     []meshproto.LogEntry
	size    int
	next    uint64 // seq of the next entry (first is 1)
	changed chan struct{}
}

// NewRing returns a ring holding the last size entries.
func NewRing(size int) *Ring {
	if size <= 0 {
		size = DefaultSize
	}
	return &Ring{size: size, next: 1, changed: make(chan struct{})}
}

// Add appends an entry, giving it the next sequence number.
func (r *Ring) Add(e meshproto.LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq = r.next
	r.next++
	r.buf = append(r.buf, e)
	if len(r.buf) > r.size {
		r.buf = r.buf[len(r.buf)-r.size:]
	}
	close(r.changed)
	r.changed = make(chan struct{})
}

// Since returns entries after seq, and how many after seq were already
// evicted (lost).
func (r *Ring) Since(seq uint64) (out []meshproto.LogEntry, lost int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.buf {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	first := r.next
	if len(r.buf) > 0 {
		first = r.buf[0].Seq
	}
	if first > seq+1 {
		lost = int(first - seq - 1)
	}
	return out, lost
}

// Changed is closed when an entry is added after the call.
func (r *Ring) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// Handler returns a slog.Handler that passes records to inner and copies
// those at Info and above into the ring.
func (r *Ring) Handler(inner slog.Handler) slog.Handler { return &tee{ring: r, inner: inner} }

type tee struct {
	ring   *Ring
	inner  slog.Handler
	attrs  []slog.Attr
	groups string
}

func (t *tee) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || t.inner.Enabled(ctx, l)
}

func (t *tee) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelInfo {
		e := meshproto.LogEntry{T: rec.Time.UTC().Format(time.RFC3339Nano), Level: rec.Level.String(), Msg: trunc(rec.Message, MaxMsg)}
		add := func(key string, v slog.Value) bool {
			if len(e.Attrs) >= MaxAttrs {
				return false
			}
			if e.Attrs == nil {
				e.Attrs = map[string]string{}
			}
			e.Attrs[key] = value(key, v)
			return true
		}
		for _, a := range t.attrs { // already prefixed with their groups
			add(a.Key, a.Value)
		}
		rec.Attrs(func(a slog.Attr) bool { return add(t.groups+a.Key, a.Value) })
		t.ring.Add(e)
	}
	if t.inner.Enabled(ctx, rec.Level) {
		return t.inner.Handle(ctx, rec)
	}
	return nil
}

func (t *tee) WithAttrs(as []slog.Attr) slog.Handler {
	c := *t
	c.inner = t.inner.WithAttrs(as)
	c.attrs = append(append([]slog.Attr(nil), t.attrs...), prefixed(t.groups, as)...)
	return &c
}

func (t *tee) WithGroup(name string) slog.Handler {
	c := *t
	c.inner = t.inner.WithGroup(name)
	c.groups = t.groups + name + "."
	return &c
}

func prefixed(groups string, as []slog.Attr) []slog.Attr {
	if groups == "" {
		return as
	}
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = slog.Attr{Key: groups + a.Key, Value: a.Value}
	}
	return out
}

// secretWords are key words (split on anything not a letter or digit) whose
// value is redacted: "control_token" and "pin" are, "pubkey" and
// "ping_interval" aren't.
var secretWords = map[string]bool{"token": true, "password": true, "passphrase": true, "secret": true,
	"pin": true, "key": true, "auth": true, "authorization": true}

func value(key string, v slog.Value) string {
	words := strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, w := range words {
		if secretWords[w] {
			return "[redacted]"
		}
	}
	return trunc(fmt.Sprint(v.Resolve().Any()), MaxAttrLen)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
