// Package connect implements client mode (`meshbridge connect`): it turns a
// remote gateway into a local TCP companion port, so meshcore-cli and other
// TCP companion apps work unchanged.
//
// A lease is taken when a local app connects and released when it
// disconnects, so an idle shim never holds the gateway. One local connection
// is served at a time, because a node supports only one app session.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/relayclient"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Dialer takes a lease. relayclient.Dial is the real one.
type Dialer func(ctx context.Context) (*relayclient.Conn, error)

// Options configure a Shim.
type Options struct {
	Gateway string
	Dial    Dialer
	// Say prints a plain status line for the user.
	Say func(format string, args ...any)
	// RenewWhen limits auto-renew to local apps that sent a frame this
	// recently (default 10 min, matching the relay's idle timeout).
	RenewWhen time.Duration
}

// Shim serves local companion connections over relay leases.
type Shim struct {
	opts Options
	slot chan struct{} // holds one token while a local app is served
}

// slotWait is how long a new local connection waits for the previous session
// to finish releasing its lease before it's refused.
const slotWait = 10 * time.Second

// New creates a Shim.
func New(opts Options) *Shim {
	if opts.Say == nil {
		opts.Say = func(string, ...any) {}
	}
	if opts.RenewWhen == 0 {
		opts.RenewWhen = 10 * time.Minute
	}
	return &Shim{opts: opts, slot: make(chan struct{}, 1)}
}

// Serve accepts local connections on ln until ctx ends, then waits for any
// session to release its lease.
func (s *Shim) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTimer(slotWait)
			defer t.Stop()
			select {
			case s.slot <- struct{}{}:
			case <-t.C:
				s.opts.Say("Refused a second local connection from %s: one app at a time.", c.RemoteAddr())
				c.Close()
				return
			case <-ctx.Done():
				c.Close()
				return
			}
			defer func() { <-s.slot }()
			s.serveOne(ctx, c)
		}()
	}
}

func (s *Shim) serveOne(ctx context.Context, local net.Conn) {
	defer local.Close()
	s.opts.Say("App connected from %s; requesting %s…", local.RemoteAddr(), s.opts.Gateway)
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	rc, err := s.opts.Dial(dctx)
	cancel()
	if err != nil {
		s.opts.Say("%s", Explain(err))
		return
	}
	defer func() {
		rc.Close()
		s.opts.Say("Released %s.", s.opts.Gateway)
	}()
	s.opts.Say("Connected to %s (%s). Lease until %s.", s.opts.Gateway, nodeLabel(rc.Lease.Node), localTime(rc.Lease.ExpiresAt))

	var lastActive atomicTime
	lastActive.set(time.Now())
	done := make(chan string, 3)

	// Local app → node.
	go func() {
		r := companion.NewReader(local, companion.HeaderToNode, meshproto.MaxFrameBytes)
		for {
			f, err := r.ReadFrame()
			if err != nil {
				if errors.Is(err, companion.ErrFrameTooLarge) {
					done <- "The app sent an oversized frame."
				} else {
					done <- "App disconnected."
				}
				return
			}
			lastActive.set(time.Now())
			if err := rc.SendFrame(ctx, f); err != nil {
				done <- "Lost the relay connection."
				return
			}
		}
	}()

	// Node → local app, and lease events.
	go func() {
		frames, control := rc.Frames, rc.Control
		for frames != nil || control != nil {
			select {
			case f, ok := <-frames:
				if !ok {
					frames = nil
					continue
				}
				if _, err := local.Write(companion.Encode(companion.HeaderFromNode, f)); err != nil {
					done <- "App disconnected."
					return
				}
			case m, ok := <-control:
				if !ok {
					control = nil
					continue
				}
				if msg, end := s.handleControl(ctx, rc, m, lastActive.get()); msg != "" {
					s.opts.Say("%s", msg)
					if end {
						done <- ""
						return
					}
				}
			}
		}
		done <- "The relay ended the session."
	}()

	select {
	case msg := <-done:
		if msg != "" {
			s.opts.Say("%s", msg)
		}
	case <-ctx.Done():
	}
	local.Close()
}

// handleControl reacts to a relay control message. It returns a line for the
// user (or "") and whether the session is over.
func (s *Shim) handleControl(ctx context.Context, rc *relayclient.Conn, m json.RawMessage, lastActive time.Time) (string, bool) {
	switch meshproto.TypeOf(m) {
	case meshproto.TypeLease:
		var l meshproto.Lease
		_ = json.Unmarshal(m, &l)
		return fmt.Sprintf("Lease renewed until %s.", localTime(l.ExpiresAt)), false
	case meshproto.TypeLeaseEnding:
		var le meshproto.LeaseEnding
		_ = json.Unmarshal(m, &le)
		if le.InS > 0 {
			if time.Since(lastActive) > s.opts.RenewWhen {
				return fmt.Sprintf("Lease ends in %ds; the app has been idle, so not renewing.", le.InS), false
			}
			if err := rc.Renew(ctx); err != nil {
				return "Couldn't renew the lease.", false
			}
			return "", false
		}
		return LeaseEndedText(le.Reason), true
	case meshproto.TypeGatewayStatus:
		var gs meshproto.GatewayStatus
		_ = json.Unmarshal(m, &gs)
		if gs.Bridge == "offline" {
			return "The gateway went offline.", false
		}
		if gs.Node != nil && gs.Node.State != meshproto.NodeConnected {
			return fmt.Sprintf("The gateway's node is %s.", gs.Node.State), false
		}
	case meshproto.TypeError:
		var e meshproto.Error
		_ = json.Unmarshal(m, &e)
		if e.Code == meshproto.ErrRenewDenied {
			return "The lease has reached its 8-hour limit and will end soon. Reconnect to start a new one.", false
		}
		return CodeText(e.Code, nil), false
	}
	return "", false
}

// Explain turns a Dial error into a plain sentence.
func Explain(err error) string {
	var re *relayclient.RefusedError
	if errors.As(err, &re) {
		var until string
		if re.Status != nil && re.Status.Node != nil {
			until = re.Status.Node.Until
		}
		code := re.Code
		if code == "" && re.CloseCode == meshproto.CloseBusy {
			code = meshproto.ErrBusy
		}
		if code == "" && re.Status != nil && re.Status.Bridge == "offline" {
			code = meshproto.ErrGatewayOffline
		}
		return CodeText(code, &until)
	}
	var he *relayclient.HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 401:
			if strings.Contains(he.Body, "expired") {
				return sentence(he.Body)
			}
			return "The relay didn't accept your token."
		case 403:
			// Why: a host token for another gateway, a retired role, or a
			// preempt without an admin token.
			if he.Body != "" {
				return sentence(he.Body)
			}
			return "Your token isn't allowed to do that."
		case 404:
			return "No such gateway."
		case 429:
			return "Too many failed sign-ins from this network; wait 15 minutes."
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
		return "Couldn't reach the relay."
	}
	return fmt.Sprintf("Couldn't connect: %v", err)
}

// sentence capitalizes s and ends it with a full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// CodeText turns a relay error code into a plain sentence.
func CodeText(code string, until *string) string {
	switch code {
	case meshproto.ErrBusy:
		return "Someone else is using this gateway."
	case meshproto.ErrGatewayOffline:
		return "Gateway is offline."
	case "node_" + meshproto.NodePaused:
		if until != nil && *until != "" {
			return fmt.Sprintf("The host has paused sharing until %s.", localTime(*until))
		}
		return "The host has paused sharing."
	case meshproto.ErrPubkeyMismatch, meshproto.ErrPubkeyUnverified:
		return "Gateway's node needs checking by a SIERRA admin."
	case "node_" + meshproto.NodeDisconnected:
		return "The gateway is online, but its node isn't connected."
	case meshproto.ErrUnknownLease:
		return "The relay didn't recognise this lease."
	case "":
		return "The relay refused the session."
	}
	return fmt.Sprintf("The relay refused the session (%s).", code)
}

// LeaseEndedText explains why a lease ended.
func LeaseEndedText(reason string) string {
	switch reason {
	case "expiry":
		return "The lease expired."
	case "preempted":
		return "A SIERRA admin took over this gateway."
	case "gateway_offline":
		return "The gateway went offline."
	case "idle":
		return "The lease ended after 10 minutes without traffic."
	case "revoked":
		return "Access was revoked."
	case "pubkey_mismatch":
		return "Gateway's node needs checking by a SIERRA admin."
	case "relay_shutdown":
		return "The relay is restarting; reconnect in a moment."
	case "host_paused":
		return "The host paused sharing."
	}
	return "The lease ended."
}

func nodeLabel(n *meshproto.Node) string {
	if n == nil || n.Name == "" {
		return "node"
	}
	return n.Name
}

func localTime(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	return t.Local().Format("15:04")
}

type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }
