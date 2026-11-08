// Package relaytest is a scripted stand-in for the relay's bridge endpoint,
// for driving a bridge in tests. Each bridge connection becomes a Session the
// test inspects and controls directly.
package relaytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// Timeout bounds every Expect call.
var Timeout = 5 * time.Second

// Server accepts bridges at /mesh/relay/{gateway}/dial.
type Server struct {
	Token         string
	PingIntervalS int
	// ReadDelay throttles reading binary frames, like the relay's rate limit.
	ReadDelay time.Duration
	// RejectStatus, when set, refuses every upgrade with this HTTP status.
	RejectStatus atomic.Int32

	srv      *httptest.Server
	sessions chan *Session
	dials    atomic.Int64
}

// New starts a server that accepts token.
func New(token string) *Server {
	s := &Server{Token: token, PingIntervalS: 25, sessions: make(chan *Session, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/mesh/relay/{gateway}/dial", s.handle)
	s.srv = httptest.NewServer(mux)
	return s
}

// URL is the relay base URL for bridge config (ws://…/mesh/relay).
func (s *Server) URL() string { return "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/mesh/relay" }

// Dials counts upgrade attempts, including rejected ones.
func (s *Server) Dials() int64 { return s.dials.Load() }

// Close stops the server.
func (s *Server) Close() { s.srv.CloseClientConnections(); s.srv.Close() }

// Next waits for the next bridge session to send hello.
func (s *Server) Next() (*Session, error) {
	select {
	case sess := <-s.sessions:
		return sess, nil
	case <-time.After(Timeout):
		return nil, errors.New("relaytest: no bridge session")
	}
}

// NoSession reports an error if a session arrives within d.
func (s *Server) NoSession(d time.Duration) error {
	select {
	case <-s.sessions:
		return errors.New("relaytest: unexpected bridge session")
	case <-time.After(d):
		return nil
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.dials.Add(1)
	if st := s.RejectStatus.Load(); st != 0 {
		http.Error(w, "rejected", int(st))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{meshproto.BridgeSubprotocol}})
	if err != nil {
		return
	}
	if conn.Subprotocol() != meshproto.BridgeSubprotocol {
		conn.Close(websocket.StatusCode(meshproto.CloseProtocolError), "subprotocol")
		return
	}
	conn.SetReadLimit(meshproto.MaxControlBytes)
	ctx := r.Context()
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	typ, data, err := conn.Read(hctx)
	cancel()
	var hello meshproto.Hello
	if err != nil || typ != websocket.MessageText || json.Unmarshal(data, &hello) != nil || hello.Type != meshproto.TypeHello {
		conn.Close(websocket.StatusCode(meshproto.CloseProtocolError), "hello required")
		return
	}
	if hello.Gateway != r.PathValue("gateway") {
		conn.Close(websocket.StatusCode(meshproto.CloseGatewayMismatch), "gateway mismatch")
		return
	}
	sess := &Session{
		Conn: conn, Hello: hello,
		control: make(chan json.RawMessage, 256), frames: make(chan []byte, 4096), closed: make(chan error, 1),
	}
	_ = sess.Send(meshproto.Welcome{Type: meshproto.TypeWelcome, Session: fmt.Sprintf("s_%d", s.dials.Load()),
		PingIntervalS: s.PingIntervalS, ServerTime: time.Now().UTC().Format(time.RFC3339)})
	s.sessions <- sess
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			sess.closed <- err
			return
		}
		if typ == websocket.MessageBinary {
			if s.ReadDelay > 0 {
				time.Sleep(s.ReadDelay)
			}
			sess.frames <- data
			continue
		}
		sess.control <- data
	}
}

// Session is one connected bridge.
type Session struct {
	Conn    *websocket.Conn
	Hello   meshproto.Hello
	control chan json.RawMessage
	frames  chan []byte
	closed  chan error
	// helloSeen is set once ExpectNode has considered the hello's node.
	helloSeen bool
}

// Send writes a control message.
func (s *Session) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	return s.Conn.Write(ctx, websocket.MessageText, b)
}

// SendFrame writes a binary frame, as if from the lease holder.
func (s *Session) SendFrame(f []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	return s.Conn.Write(ctx, websocket.MessageBinary, f)
}

// Attach tells the bridge a client took lease id.
func (s *Session) Attach(id string) error {
	return s.Send(meshproto.Attach{Type: meshproto.TypeAttach, Lease: id, Client: "test-client"})
}

// Close closes the session with code.
func (s *Session) Close(code int, reason string) error {
	return s.Conn.Close(websocket.StatusCode(code), reason)
}

// Expect waits for the next control message of type typ (skipping others)
// and decodes it into out, if non-nil.
func (s *Session) Expect(typ string, out any) error {
	deadline := time.After(Timeout)
	for {
		select {
		case m := <-s.control:
			if meshproto.TypeOf(m) != typ {
				continue
			}
			if out != nil {
				return json.Unmarshal(m, out)
			}
			return nil
		case err := <-s.closed:
			s.closed <- err
			return fmt.Errorf("relaytest: closed waiting for %q: %w", typ, err)
		case <-deadline:
			return fmt.Errorf("relaytest: no %q within %s", typ, Timeout)
		}
	}
}

// ExpectNode waits for the bridge to report node state (in hello or a later
// node_status) and returns the node.
func (s *Session) ExpectNode(state string) (meshproto.Node, error) {
	if !s.helloSeen {
		s.helloSeen = true
		if s.Hello.Node.State == state {
			return s.Hello.Node, nil
		}
	}
	deadline := time.Now().Add(Timeout)
	for time.Now().Before(deadline) {
		var m meshproto.NodeStatus
		if err := s.Expect(meshproto.TypeNodeStatus, &m); err != nil {
			return m.Node, err
		}
		if m.State == state {
			return m.Node, nil
		}
	}
	return meshproto.Node{}, fmt.Errorf("relaytest: node never reached %q", state)
}

// ExpectFrame waits for a binary frame from the bridge.
func (s *Session) ExpectFrame() ([]byte, error) {
	select {
	case f := <-s.frames:
		return f, nil
	case <-time.After(Timeout):
		return nil, fmt.Errorf("relaytest: no frame within %s", Timeout)
	}
}

// NoFrame reports an error if a frame arrives within d.
func (s *Session) NoFrame(d time.Duration) error {
	select {
	case f := <-s.frames:
		return fmt.Errorf("relaytest: unexpected %d-byte frame", len(f))
	case <-time.After(d):
		return nil
	}
}

// ExpectClosed waits for the bridge to close the session and returns the
// close code (-1 without a close frame).
func (s *Session) ExpectClosed() (int, error) {
	select {
	case err := <-s.closed:
		s.closed <- err
		code := int(websocket.CloseStatus(err))
		if code == -1 {
			return code, fmt.Errorf("relaytest: closed without a close frame: %w", err)
		}
		return code, nil
	case <-time.After(Timeout):
		return 0, fmt.Errorf("relaytest: not closed within %s", Timeout)
	}
}
