// Package relayclient is a client of the relay's client endpoint
// (sierra-mesh-client.v1): take a lease on a gateway and exchange companion
// frame payloads with its node. It backs the live tests now and
// `meshbridge connect` later.
package relayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode"

	"github.com/coder/websocket"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// RefusedError means the relay didn't grant a lease.
type RefusedError struct {
	Code      string // relay error code, e.g. busy, gateway_offline, node_paused
	CloseCode int
	Status    *meshproto.GatewayStatus
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("relay refused the lease: %s (close %d)", e.Code, e.CloseCode)
}

// HTTPError means the relay refused the upgrade (401 bad token, 403
// forbidden, 404 unknown gateway, 429 locked out, ...).
type HTTPError struct {
	Status int
	// Body is the start of the relay's answer: why, in a sentence.
	Body string
	Err  error
}

func (e *HTTPError) Error() string { return fmt.Sprintf("relay answered HTTP %d: %v", e.Status, e.Err) }
func (e *HTTPError) Unwrap() error { return e.Err }

// reason makes a refused upgrade's body safe to show: its first line, only
// printable characters, at most 200 of them, and nothing from an HTML page
// (a proxy's error page, not the relay's answer).
func reason(body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	if strings.HasPrefix(line, "<") {
		return ""
	}
	line = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, line)
	if r := []rune(line); len(r) > 200 {
		line = string(r[:200]) + "…"
	}
	return strings.TrimSpace(line)
}

// Conn is a client session holding a lease.
type Conn struct {
	ws    *websocket.Conn
	Lease meshproto.Lease

	// Frames delivers payloads from the node. Control delivers control
	// messages (lease, lease_ending, gateway_status, error) as raw JSON.
	// Both close when the session ends; Err then reports why.
	Frames  chan []byte
	Control chan json.RawMessage

	mu  sync.Mutex
	err error
}

// Dial connects to gateway on the relay at base (wss://…/mesh/relay) and
// waits for a lease.
func Dial(ctx context.Context, base, gateway, token string, preempt bool) (*Conn, error) {
	u := strings.TrimRight(base, "/") + "/" + gateway
	if preempt {
		u += "?preempt=1"
	}
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
		Subprotocols: []string{meshproto.ClientSubprotocol},
	})
	var body string
	if resp != nil && resp.Body != nil {
		if err != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			body = reason(string(b))
		}
		resp.Body.Close()
	}
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, &HTTPError{Status: resp.StatusCode, Body: body, Err: err}
		}
		return nil, fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(meshproto.MaxControlBytes + 1024)

	refused := &RefusedError{CloseCode: -1}
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			ws.CloseNow()
			if refused.Code != "" || refused.Status != nil {
				refused.CloseCode = int(websocket.CloseStatus(err))
				return nil, refused
			}
			return nil, fmt.Errorf("waiting for lease: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		switch meshproto.TypeOf(data) {
		case meshproto.TypeLease:
			c := &Conn{ws: ws, Frames: make(chan []byte, 256), Control: make(chan json.RawMessage, 64)}
			if err := json.Unmarshal(data, &c.Lease); err != nil {
				ws.CloseNow()
				return nil, fmt.Errorf("bad lease message: %w", err)
			}
			go c.readLoop()
			return c, nil
		case meshproto.TypeError:
			var m meshproto.Error
			_ = json.Unmarshal(data, &m)
			refused.Code = m.Code
		case meshproto.TypeGatewayStatus:
			var m meshproto.GatewayStatus
			_ = json.Unmarshal(data, &m)
			refused.Status = &m
		}
	}
}

func (c *Conn) readLoop() {
	defer close(c.Frames)
	defer close(c.Control)
	for {
		typ, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
		if typ == websocket.MessageBinary {
			c.Frames <- data
		} else {
			c.Control <- json.RawMessage(data)
		}
	}
}

// Err reports why the session ended, once Frames and Control have closed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// SendFrame sends one companion payload to the node.
func (c *Conn) SendFrame(ctx context.Context, payload []byte) error {
	if len(payload) > meshproto.MaxFrameBytes {
		return fmt.Errorf("frame of %d bytes exceeds %d", len(payload), meshproto.MaxFrameBytes)
	}
	return c.ws.Write(ctx, websocket.MessageBinary, payload)
}

// Renew asks to extend the lease. The relay answers with a new lease message
// (or an error) on Control.
func (c *Conn) Renew(ctx context.Context) error {
	b, _ := json.Marshal(meshproto.Renew{Type: meshproto.TypeRenew, Lease: c.Lease.Lease})
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// Close ends the session, releasing the lease.
func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
